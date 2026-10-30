package companion

import (
	"context"
	"reflect"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ValidUpgradeAttempt accepts a caller-retained canonical nonzero UUID.
func ValidUpgradeAttempt(attempt string) bool {
	id, err := uuid.Parse(attempt)
	return err == nil && id != uuid.Nil && id.String() == attempt
}

// ValidUpgradeCompletion verifies the receipt belongs to this exact grant.
// A receipt proves only what the trusted updater reported; it is not a new grant.
func ValidUpgradeCompletion(op *ext.AgentUpgrade) bool {
	c := op.Status.Completion
	if c == nil || !ValidUpgradeAttempt(op.Status.AttemptID) || c.AttemptID != op.Status.AttemptID || op.Status.StartedAt == nil || op.Status.StartedAt.IsZero() || c.CompletedAt.IsZero() || !c.HeartbeatAt.After(op.Status.StartedAt.Time) || c.CompletedAt.Before(&c.HeartbeatAt) || op.Status.ObservedGeneration != op.Generation || op.Status.Allocation == nil || op.Status.Allocation.UID == "" {
		return false
	}
	proof := meta.FindStatusCondition(op.Status.Conditions, "ApplicationComplete")
	grant := meta.FindStatusCondition(op.Status.Conditions, "ApplicationGranted")
	if proof == nil || proof.Status != metav1.ConditionTrue || proof.ObservedGeneration != op.Generation || proof.Reason != c.Result || grant == nil || grant.Status != metav1.ConditionTrue || grant.ObservedGeneration != op.Generation {
		return false
	}
	return c.Result == "Succeeded" && c.Version == op.Spec.ReleaseSpec.Version || c.Result == "RolledBack" && c.Version == op.Spec.FromVersion
}

// ObserveUpgrade proves retained object-instance ownership without creating or
// releasing a lock. Deletion, target changes and heartbeat expiry do not erase
// ownership. A nil allocation means absence was observed, never installation
// permission or cleanup proof. The caller separately enforces active credentials.
func (m Manager) ObserveUpgrade(ctx context.Context, req UpgradeRequest) (*ext.AgentUpgrade, *ext.ContainerNode, *ext.ContainerAllocation, error) {
	if m.Client == nil {
		return nil, nil, nil, ErrIdentity
	}
	var op ext.AgentUpgrade
	if err := m.Client.Get(ctx, req.Operation, &op); err != nil {
		return nil, nil, nil, err
	}
	if op.UID == "" || op.Generation < 1 || op.Spec.TenantID == "" || op.Name != UpgradeName(op.Spec.Cluster.UID) || tenant.IDOf(&op) != op.Spec.TenantID || !controllerutil.ContainsFinalizer(&op, UpgradeFinalizer) {
		return nil, nil, nil, ErrIdentity
	}
	var node ext.ContainerNode
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.NodeRecord.Namespace, Name: op.Spec.NodeRecord.Name}, &node); err != nil {
		return nil, nil, nil, err
	}
	if reference(&node) != op.Spec.NodeRecord || node.Namespace != op.Namespace || node.Spec.Cluster != op.Spec.Cluster || node.Spec.Runtime != "Raw" || node.Spec.InstanceID != op.Spec.InstanceID || tenant.IDOf(&node) != op.Spec.TenantID {
		return nil, nil, nil, ErrIdentity
	}
	var cluster native.Cluster
	var nn native.Node
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.Cluster.Namespace, Name: op.Spec.Cluster.Name}, &cluster); err != nil {
		return nil, nil, nil, err
	}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nn); err != nil {
		return nil, nil, nil, err
	}
	if ownership.MatchesNative(op.Spec.Cluster, &cluster) != nil || ownership.MatchesNative(node.Spec.Node, &nn) != nil || cluster.Spec.AgentNamespace != node.Namespace || nn.Namespace != node.Namespace || nn.Spec.AgentType != native.AgentTypeRaw || tenant.IDOf(&cluster) != op.Spec.TenantID || tenant.IDOf(&nn) != op.Spec.TenantID || nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 {
		return nil, nil, nil, ErrIdentity
	}
	var clusterList native.ClusterList
	if err := m.Client.List(ctx, &clusterList); err != nil {
		return nil, nil, nil, err
	}
	for _, other := range clusterList.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == node.Namespace {
			return nil, nil, nil, ErrIdentity
		}
	}
	var cc ext.ContainerCluster
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.ClusterRecord.Namespace, Name: op.Spec.ClusterRecord.Name}, &cc); err != nil {
		return nil, nil, nil, err
	}
	owner := metav1.GetControllerOf(&op)
	if reference(&cc) != op.Spec.ClusterRecord || cc.Spec.Cluster != op.Spec.Cluster || cc.Namespace != op.Namespace || cc.Spec.AgentNamespace != op.Namespace || cc.Spec.Runtime != node.Spec.Runtime || tenant.IDOf(&cc) != op.Spec.TenantID || owner == nil || owner.UID != cc.UID || owner.Name != cc.Name || owner.Kind != "ContainerCluster" || owner.APIVersion != ext.GroupVersion.String() {
		return nil, nil, nil, ErrIdentity
	}
	var a ext.ContainerAllocation
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: Name(node.Spec.Node.UID)}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return &op, &node, nil, nil
		}
		return nil, nil, nil, err
	}
	wanted := upgradeLock(&op, &node)
	// A replacement at the same deterministic name proves the witnessed old
	// allocation is gone. It grants no rights over the replacement.
	if op.Status.Allocation != nil && op.Status.Allocation.UID != "" && op.Status.Allocation.UID != string(a.UID) {
		return &op, &node, nil, nil
	}
	if a.UID == "" || a.Generation < 1 || !reflect.DeepEqual(a.Spec, wanted.Spec) || !reflect.DeepEqual(a.OwnerReferences, wanted.OwnerReferences) || tenant.IDOf(&a) != op.Spec.TenantID || op.Status.Allocation == nil || *op.Status.Allocation != reference(&a) {
		return nil, nil, nil, ErrIdentity
	}
	return &op, &node, &a, nil
}
