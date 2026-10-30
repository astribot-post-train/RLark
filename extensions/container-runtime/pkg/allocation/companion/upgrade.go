package companion

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// UpgradeFinalizer retains the per-Cluster operation while node ownership or
// application remains uncertain. Only upgrade reconciliation owns it.
const UpgradeFinalizer = "containers.rlark.io/upgrade-protection"

// UpgradeName is the single operation slot for one exact native Cluster UID.
func UpgradeName(clusterUID string) string {
	return "cluster-" + strings.TrimPrefix(Name(clusterUID), "node-")
}

// UpgradeRequest locates an already persisted, immutable upgrade operation.
// It never accepts an independent release, GPU set or allocation identity.
type UpgradeRequest struct{ Operation client.ObjectKey }

// ReserveUpgrade competes with Task allocation on the same API Create. It
// revalidates authorization after creation; a failed/lost response leaves a
// visible lock and retries can recover only its exact operation identity.
func (m Manager) ReserveUpgrade(ctx context.Context, req UpgradeRequest) (*ext.ContainerAllocation, error) {
	if m.Client == nil || m.timeout() <= 0 {
		return nil, errors.New("uncached client and positive heartbeat timeout required")
	}
	op, node, err := m.upgradeTarget(ctx, req, true)
	if err != nil {
		return nil, err
	}
	wanted := upgradeLock(op, node)
	var current ext.ContainerAllocation
	key := client.ObjectKeyFromObject(wanted)
	err = m.Client.Get(ctx, key, &current)
	if err == nil {
		return sameReservation(&current, wanted)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	ready := meta.FindStatusCondition(node.Status.Conditions, "Ready")
	if node.Status.Busy || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != node.Generation || node.Status.ObservedGeneration != node.Generation || node.Status.AgentCredentialID != op.Spec.CredentialID || node.Status.LastHeartbeatTime == nil || node.Status.LastHeartbeatTime.After(m.now()) || m.now().Sub(node.Status.LastHeartbeatTime.Time) > m.timeout() || node.Status.AgentVersion != op.Spec.FromVersion || node.Status.AgentVersion == op.Spec.ReleaseSpec.Version {
		return nil, fmt.Errorf("%w: upgrade requires a fresh idle observation of the source version", ErrUnavailable)
	}
	// Duplicate node/address/device inventory is ambiguous even while idle.
	var inventory ext.ContainerNodeList
	if err := m.Client.List(ctx, &inventory); err != nil {
		return nil, err
	}
	for _, other := range inventory.Items {
		if other.UID == node.UID {
			continue
		}
		if other.Spec.Node == node.Spec.Node || other.Spec.NetworkScope == node.Spec.NetworkScope && other.Spec.PrivateAddress == node.Spec.PrivateAddress {
			return nil, ErrIdentity
		}
		for _, gpu := range other.Spec.AuthorizedGPUUUIDs {
			if slices.Contains(node.Spec.AuthorizedGPUUUIDs, gpu) {
				return nil, ErrIdentity
			}
		}
	}
	var held ext.ContainerAllocationList
	if err := m.Client.List(ctx, &held); err != nil {
		return nil, err
	}
	for _, a := range held.Items {
		if a.Spec.Node.UID == node.Spec.Node.UID || a.Spec.NetworkScope == node.Spec.NetworkScope && a.Spec.PrivateAddress == node.Spec.PrivateAddress {
			return nil, ErrUnavailable
		}
		for _, gpu := range a.Spec.GPUUUIDs {
			if slices.Contains(node.Spec.AuthorizedGPUUUIDs, gpu) {
				return nil, ErrUnavailable
			}
		}
	}
	if err := m.Client.Create(ctx, wanted); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := m.Client.Get(ctx, key, &current); err != nil {
			return nil, err
		}
		if _, err := sameReservation(&current, wanted); err != nil {
			return nil, err
		}
		wanted = &current
	}
	// A credential/target/deletion change during allocation I/O cannot grant an
	// upgrade. The durable lock stays held for exact reconciliation/recovery.
	latestOp, latestNode, err := m.upgradeTarget(ctx, req, true)
	if err != nil {
		return nil, err
	}
	if latestOp.UID != op.UID || !reflect.DeepEqual(latestOp.Spec, op.Spec) || !reflect.DeepEqual(latestNode.Spec, node.Spec) {
		return nil, ErrIdentity
	}
	if wanted.UID == "" {
		return nil, fmt.Errorf("%w: allocation UID was not observed", ErrUnavailable)
	}
	return wanted, nil
}

// ValidateUpgradeIntent proves scope and ownership before a controller adds a
// finalizer. It has no external effects and grants no application authority.
func (m Manager) ValidateUpgradeIntent(ctx context.Context, req UpgradeRequest) error {
	if m.Client == nil {
		return ErrIdentity
	}
	_, _, err := m.upgradeTarget(ctx, req, false)
	return err
}

// ReserveCancelledUpgrade establishes an exact node-lock witness after the
// operation is durably Cancelled. It needs no heartbeat or available target;
// current object-instance ownership still has to be provable. This closes the
// normal create/status-response-loss recovery path before lock cleanup.
func (m Manager) ReserveCancelledUpgrade(ctx context.Context, req UpgradeRequest) (*ext.ContainerAllocation, error) {
	if m.Client == nil {
		return nil, ErrIdentity
	}
	var op ext.AgentUpgrade
	if err := m.Client.Get(ctx, req.Operation, &op); err != nil {
		return nil, err
	}
	if op.UID == "" || op.Name != UpgradeName(op.Spec.Cluster.UID) || op.Status.Phase != "Cancelled" || op.Status.ObservedGeneration != op.Generation || !controllerutil.ContainsFinalizer(&op, UpgradeFinalizer) || tenant.IDOf(&op) != op.Spec.TenantID {
		return nil, ErrIdentity
	}
	var node ext.ContainerNode
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.NodeRecord.Namespace, Name: op.Spec.NodeRecord.Name}, &node); err != nil {
		return nil, err
	}
	if reference(&node) != op.Spec.NodeRecord || node.Spec.Cluster != op.Spec.Cluster || node.Spec.InstanceID != op.Spec.InstanceID || node.Namespace != op.Namespace || tenant.IDOf(&node) != op.Spec.TenantID {
		return nil, ErrIdentity
	}
	var cc ext.ContainerCluster
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.ClusterRecord.Namespace, Name: op.Spec.ClusterRecord.Name}, &cc); err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(&op)
	if reference(&cc) != op.Spec.ClusterRecord || cc.Spec.Cluster != op.Spec.Cluster || cc.Namespace != op.Namespace || tenant.IDOf(&cc) != op.Spec.TenantID || owner == nil || owner.UID != cc.UID || owner.Name != cc.Name || owner.Kind != "ContainerCluster" || owner.APIVersion != ext.GroupVersion.String() {
		return nil, ErrIdentity
	}
	wanted := upgradeLock(&op, &node)
	var current ext.ContainerAllocation
	key := client.ObjectKeyFromObject(wanted)
	if err := m.Client.Get(ctx, key, &current); err == nil {
		// A cancelled lock may already be in deletion with its finalizer retained.
		if current.Spec.Upgrade == nil || !reflect.DeepEqual(current.Spec, wanted.Spec) || !reflect.DeepEqual(current.OwnerReferences, wanted.OwnerReferences) || tenant.IDOf(&current) != op.Spec.TenantID {
			return nil, ErrIdentity
		}
		return &current, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if op.Status.Allocation != nil {
		return nil, ErrUnavailable
	} // Never recreate a witnessed, released operation.
	if err := m.Client.Create(ctx, wanted); err != nil {
		return nil, err
	}
	return wanted, nil
}

func upgradeLock(op *ext.AgentUpgrade, node *ext.ContainerNode) *ext.ContainerAllocation {
	wanted := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: Name(node.Spec.Node.UID), Namespace: node.Namespace, Finalizers: []string{Finalizer}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(op, ext.GroupVersion.WithKind("AgentUpgrade"))}}, Spec: ext.ContainerAllocationSpec{Cluster: op.Spec.Cluster, Node: node.Spec.Node, TenantID: op.Spec.TenantID, NodeRecordUID: string(node.UID), NodeInstanceID: node.Spec.InstanceID, PrivateAddress: node.Spec.PrivateAddress, NetworkScope: node.Spec.NetworkScope, GPUUUIDs: slices.Clone(node.Spec.AuthorizedGPUUUIDs), Upgrade: &ext.UpgradeAllocation{Operation: reference(op), Release: op.Spec.Release, SHA256: op.Spec.ReleaseSpec.SHA256}}}
	slices.Sort(wanted.Spec.GPUUUIDs)
	tenant.Inherit(op, wanted)
	return wanted
}

func (m Manager) upgradeTarget(ctx context.Context, req UpgradeRequest, requireFinalizer bool) (*ext.AgentUpgrade, *ext.ContainerNode, error) {
	var op ext.AgentUpgrade
	if err := m.Client.Get(ctx, req.Operation, &op); err != nil {
		return nil, nil, err
	}
	if op.UID == "" || op.Generation < 1 || op.Name != UpgradeName(op.Spec.Cluster.UID) || !op.DeletionTimestamp.IsZero() || requireFinalizer && !controllerutil.ContainsFinalizer(&op, UpgradeFinalizer) || op.Spec.Cluster.UID == "" || op.Spec.InstanceID == "" || op.Spec.CredentialID == "" || op.Spec.FromVersion == "" || op.Spec.TenantID == "" || tenant.IDOf(&op) != op.Spec.TenantID {
		return nil, nil, ErrIdentity
	}
	if op.Status.Phase != "" && op.Status.Phase != "Pending" && op.Status.Phase != "Reserved" {
		return nil, nil, ErrUnavailable
	}
	var node ext.ContainerNode
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.NodeRecord.Namespace, Name: op.Spec.NodeRecord.Name}, &node); err != nil {
		return nil, nil, err
	}
	if reference(&node) != op.Spec.NodeRecord || node.Spec.Cluster != op.Spec.Cluster || node.Namespace != op.Namespace || node.Spec.InstanceID != op.Spec.InstanceID || node.Annotations[ext.AgentCredentialAnnotation] != op.Spec.CredentialID || tenant.IDOf(&node) != op.Spec.TenantID || node.Spec.Runtime != "Raw" || !node.DeletionTimestamp.IsZero() || len(node.Spec.AuthorizedGPUUUIDs) == 0 {
		return nil, nil, ErrIdentity
	}
	validated := meta.FindStatusCondition(node.Status.Conditions, "Validated")
	if validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != node.Generation {
		return nil, nil, ErrUnavailable
	}
	var cluster native.Cluster
	var nativeNode native.Node
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.Cluster.Namespace, Name: op.Spec.Cluster.Name}, &cluster); err != nil {
		return nil, nil, err
	}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nativeNode); err != nil {
		return nil, nil, err
	}
	if ownership.MatchesNative(op.Spec.Cluster, &cluster) != nil || ownership.MatchesNative(node.Spec.Node, &nativeNode) != nil || cluster.Spec.AgentNamespace != node.Namespace || tenant.IDOf(&cluster) != op.Spec.TenantID || tenant.IDOf(&nativeNode) != op.Spec.TenantID || nativeNode.Namespace != node.Namespace || nativeNode.Spec.AgentType != native.AgentTypeRaw || !cluster.DeletionTimestamp.IsZero() || !nativeNode.DeletionTimestamp.IsZero() || nativeNode.Status.Reservation != nil || len(nativeNode.Status.DockerReservations) != 0 {
		return nil, nil, ErrIdentity
	}
	var nativeClusters native.ClusterList
	if err := m.Client.List(ctx, &nativeClusters); err != nil {
		return nil, nil, err
	}
	for _, other := range nativeClusters.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return nil, nil, ErrIdentity
		}
	}
	var companions ext.ContainerClusterList
	if err := m.Client.List(ctx, &companions, client.InNamespace(node.Namespace)); err != nil {
		return nil, nil, err
	}
	var cc *ext.ContainerCluster
	for i := range companions.Items {
		if companions.Items[i].Spec.Cluster == op.Spec.Cluster {
			if cc != nil {
				return nil, nil, ErrIdentity
			}
			cc = &companions.Items[i]
		}
	}
	if cc == nil || reference(cc) != op.Spec.ClusterRecord || cc.Spec.Runtime != node.Spec.Runtime || cc.Spec.AgentNamespace != node.Namespace || cc.Spec.TargetAgentRelease != op.Spec.Release.Name || op.Spec.Release.Namespace != node.Namespace || tenant.IDOf(cc) != op.Spec.TenantID || !cc.DeletionTimestamp.IsZero() {
		return nil, nil, ErrIdentity
	}
	cv := meta.FindStatusCondition(cc.Status.Conditions, "Validated")
	if cv == nil || cv.Status != metav1.ConditionTrue || cv.ObservedGeneration != cc.Generation || cc.Status.ObservedGeneration != cc.Generation {
		return nil, nil, ErrUnavailable
	}
	owner := metav1.GetControllerOf(&op)
	if owner == nil || owner.UID != cc.UID || owner.Name != cc.Name || owner.Kind != "ContainerCluster" || owner.APIVersion != ext.GroupVersion.String() {
		return nil, nil, ErrIdentity
	}
	var release ext.AgentRelease
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.Release.Namespace, Name: op.Spec.Release.Name}, &release); err != nil {
		return nil, nil, err
	}
	if reference(&release) != op.Spec.Release || release.UID == "" || !release.DeletionTimestamp.IsZero() || tenant.IDOf(&release) != op.Spec.TenantID || release.Spec != op.Spec.ReleaseSpec || artifact.ValidateSpec(release.Spec) != nil || !release.Status.Available || release.Status.ObservedGeneration != release.Generation {
		return nil, nil, ErrUnavailable
	}
	return &op, &node, nil
}
