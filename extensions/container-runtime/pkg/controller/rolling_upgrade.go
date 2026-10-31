package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const rolloutOwnerAnnotation = ext.Group + "/upgrade-selector"
const rolloutOwner = "idle-rollout-v1"

// AgentRolloutReconciler owns only AgentRolloutReady and LastAgentUpgrade on
// ContainerCluster. It selects operations; it never grants installation or
// releases allocations. Client must bypass the informer cache.
type AgentRolloutReconciler struct {
	Client client.Client
	Now    func() time.Time
}

// SetupWithManager installs the separate, opt-in idle rollout controller.
func (r *AgentRolloutReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).Named("container-agent-rollout").For(&ext.ContainerCluster{}).Complete(r)
}

func (r AgentRolloutReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func objectRef(o metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}

func currentCondition(items []metav1.Condition, generation int64, name string) bool {
	c := meta.FindStatusCondition(items, name)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

func (r AgentRolloutReconciler) report(ctx context.Context, cc *ext.ContainerCluster, ready bool, reason, message string) (ctrl.Result, error) {
	before := cc.DeepCopy()
	condition(&cc.Status.Conditions, cc.Generation, "AgentRolloutReady", ready, reason, message)
	return persist(ctx, r.Client, before, cc)
}

func (r AgentRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, errors.New("uncached management client required")
	}
	var cc ext.ContainerCluster
	if err := r.Client.Get(ctx, req.NamespacedName, &cc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if cc.UID == "" || cc.Generation < 1 || cc.Spec.Runtime != "Raw" && cc.Spec.Runtime != "Docker" || cc.Spec.MaxUnavailable < 0 || cc.Spec.MaxUnavailable > 1 {
		return r.report(ctx, &cc, false, "UnsupportedConfiguration", "Idle rollouts require a Raw or Docker Cluster and maxUnavailable of 0 (default one) or 1")
	}
	if err := (ContainerClusterReconciler{Client: r.Client}).validateCluster(ctx, &cc); err != nil {
		if errors.Is(err, errInvalidBinding) || apierrors.IsNotFound(err) {
			return r.report(ctx, &cc, false, "InvalidBinding", "Current Cluster ownership cannot be proven")
		}
		return ctrl.Result{}, err
	}
	if cc.Status.ObservedGeneration != cc.Generation || !currentCondition(cc.Status.Conditions, cc.Generation, "Validated") {
		return r.report(ctx, &cc, false, "WaitingForValidation", "The Cluster validator must observe the current desired generation")
	}
	var nativeCluster native.Cluster
	if err := getNative(ctx, r.Client, cc.Spec.Cluster, &nativeCluster); err != nil {
		return ctrl.Result{}, err
	}
	if !nativeCluster.DeletionTimestamp.IsZero() {
		return r.report(ctx, &cc, false, "InvalidBinding", "The native Cluster is being deleted")
	}
	key := client.ObjectKey{Namespace: cc.Namespace, Name: companion.UpgradeName(cc.Spec.Cluster.UID)}
	var op ext.AgentUpgrade
	err := r.Client.Get(ctx, key, &op)
	if err == nil {
		return r.existing(ctx, &cc, &op)
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// Also reject operations claiming this Cluster outside its canonical slot.
	var operations ext.AgentUpgradeList
	if err := r.Client.List(ctx, &operations, client.InNamespace(cc.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range operations.Items {
		if other.Spec.Cluster == cc.Spec.Cluster || other.Spec.ClusterRecord.UID == string(cc.UID) {
			return r.report(ctx, &cc, false, "OperationConflict", "An operation outside the canonical Cluster slot requires administrator review")
		}
	}
	if cc.Spec.TargetAgentRelease == "" {
		return r.report(ctx, &cc, false, "Paused", "No target AgentRelease is configured; no new operation will be selected")
	}
	var release ext.AgentRelease
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: cc.Spec.TargetAgentRelease}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			return r.report(ctx, &cc, false, "TargetUnavailable", "The configured target release does not exist")
		}
		return ctrl.Result{}, err
	}
	if release.UID == "" || !release.DeletionTimestamp.IsZero() || tenant.IDOf(&release) != tenant.IDOf(&cc) || !release.Status.Available || release.Status.ObservedGeneration != release.Generation || artifact.ValidateSpec(release.Spec) != nil {
		return r.report(ctx, &cc, false, "TargetUnavailable", "A current available release in the owning tenant is required")
	}
	node, allReady, err := r.selectNode(ctx, &cc, &release)
	if err != nil {
		if errors.Is(err, errInvalidBinding) || apierrors.IsNotFound(err) {
			return r.report(ctx, &cc, false, "NodeBindingConflict", "A current member has an invalid native, tenant or credential binding")
		}
		return ctrl.Result{}, err
	}
	if node == nil {
		if allReady {
			return r.report(ctx, &cc, true, "AllNodesObserved", "Every current member has a fresh authenticated observation of the target version")
		}
		return r.report(ctx, &cc, false, "WaitingForIdleNode", "Waiting for current, idle, upgrade-capable membership without an allocation")
	}
	// Re-read configuration before producing intent. A later target change still
	// cannot authorize installation: reservation and claim revalidate independently.
	var fresh ext.ContainerCluster
	if err := r.Client.Get(ctx, req.NamespacedName, &fresh); err != nil {
		return ctrl.Result{}, err
	}
	if fresh.UID != cc.UID || fresh.ResourceVersion != cc.ResourceVersion {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	wanted, err := rolloutOperation(&cc, node, &release)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Client.Create(ctx, wanted); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return r.report(ctx, &cc, false, "OperationSelected", "One idle node was selected; the separate reservation and scoped grant remain mandatory")
}

func rolloutOperation(cc *ext.ContainerCluster, n *ext.ContainerNode, release *ext.AgentRelease) (*ext.AgentUpgrade, error) {
	spec := ext.AgentUpgradeSpec{Cluster: cc.Spec.Cluster, ClusterRecord: objectRef(cc), NodeRecord: objectRef(n), Release: objectRef(release), ReleaseSpec: release.Spec, TenantID: tenant.IDOf(cc), InstanceID: n.Spec.InstanceID, CredentialID: n.Annotations[ext.AgentCredentialAnnotation], FromVersion: n.Status.AgentVersion}
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	operation := uuid.NewSHA1(uuid.NameSpaceOID, data).String()
	return &ext.AgentUpgrade{ObjectMeta: metav1.ObjectMeta{Name: companion.UpgradeName(cc.Spec.Cluster.UID), Namespace: cc.Namespace, Labels: map[string]string{tenant.IDLabel: tenant.IDOf(cc)}, Annotations: map[string]string{rolloutOwnerAnnotation: rolloutOwner, ext.Group + "/provision-operation": operation}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cc, ext.GroupVersion.WithKind("ContainerCluster"))}}, Spec: spec}, nil
}

func (r AgentRolloutReconciler) freshNode(n *ext.ContainerNode) bool {
	return n.Status.ObservedGeneration == n.Generation && n.Status.AgentCredentialID != "" && n.Status.AgentCredentialID == n.Annotations[ext.AgentCredentialAnnotation] && currentCondition(n.Status.Conditions, n.Generation, "Ready") && currentCondition(n.Status.Conditions, n.Generation, "Validated") && n.Status.LastHeartbeatTime != nil && freshHeartbeat(n.Status.LastHeartbeatTime.Time, r.now())
}

func (r AgentRolloutReconciler) selectNode(ctx context.Context, cc *ext.ContainerCluster, release *ext.AgentRelease) (*ext.ContainerNode, bool, error) {
	var nodes ext.ContainerNodeList
	var held ext.ContainerAllocationList
	if err := r.Client.List(ctx, &nodes, client.InNamespace(cc.Namespace)); err != nil {
		return nil, false, err
	}
	if err := r.Client.List(ctx, &held); err != nil {
		return nil, false, err
	}
	sort.Slice(nodes.Items, func(i, j int) bool {
		if nodes.Items[i].Name == nodes.Items[j].Name {
			return nodes.Items[i].UID < nodes.Items[j].UID
		}
		return nodes.Items[i].Name < nodes.Items[j].Name
	})
	count := 0
	allReady := true
	var selected *ext.ContainerNode
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Cluster != cc.Spec.Cluster {
			return nil, false, invalidBinding("foreign Cluster reference in authoritative namespace")
		}
		count++
		if n.UID == "" || n.Spec.Runtime != cc.Spec.Runtime || n.Spec.Runtime != "Raw" && n.Spec.Runtime != "Docker" || !n.DeletionTimestamp.IsZero() || tenant.IDOf(n) != tenant.IDOf(cc) {
			return nil, false, errInvalidBinding
		}
		err := (ContainerNodeReconciler{Client: r.Client}).validateNode(ctx, n)
		if err != nil {
			return nil, false, err
		}
		var nativeNode native.Node
		if err := getNative(ctx, r.Client, n.Spec.Node, &nativeNode); err != nil {
			return nil, false, err
		}
		if !nativeNode.DeletionTimestamp.IsZero() {
			return nil, false, invalidBinding("native Node is being deleted")
		}
		fresh := r.freshNode(n)
		if fresh && n.Status.AgentVersion == release.Spec.Version {
			continue
		}
		allReady = false
		// Old target-fenced nodes intentionally report Schedulable=false. The
		// upgrade selector relies on the stronger fresh idle heartbeat and
		// shared-lock checks instead of treating that fence as busy.
		if selected != nil || !fresh || n.Status.Busy || n.Status.AgentVersion == "" || n.Status.AgentVersion == release.Spec.Version || len(n.Spec.AuthorizedGPUUUIDs) == 0 || !slices.Contains(n.Status.Capabilities, ext.AgentUpgradeCapability) || !slices.Contains(n.Status.Capabilities, "agent-platform/"+release.Spec.OS+"/"+release.Spec.Architecture) {
			continue
		}
		occupied := false
		for _, a := range held.Items {
			if a.Spec.Node == n.Spec.Node || a.Spec.NodeRecordUID == string(n.UID) || a.Spec.NetworkScope == n.Spec.NetworkScope && a.Spec.PrivateAddress == n.Spec.PrivateAddress {
				occupied = true
			}
			for _, gpu := range a.Spec.GPUUUIDs {
				if slices.Contains(n.Spec.AuthorizedGPUUUIDs, gpu) {
					occupied = true
				}
			}
		}
		if !occupied {
			selected = n
		}
	}
	return selected, count > 0 && allReady, nil
}

func (r AgentRolloutReconciler) existing(ctx context.Context, cc *ext.ContainerCluster, op *ext.AgentUpgrade) (ctrl.Result, error) {
	owner := metav1.GetControllerOf(op)
	if op.UID == "" || op.Spec.Cluster != cc.Spec.Cluster || op.Spec.ClusterRecord != objectRef(cc) || op.Spec.TenantID != tenant.IDOf(cc) || tenant.IDOf(op) != tenant.IDOf(cc) || op.Annotations[rolloutOwnerAnnotation] != rolloutOwner || owner == nil || owner.UID != cc.UID || owner.Name != cc.Name || owner.Kind != "ContainerCluster" || owner.APIVersion != ext.GroupVersion.String() {
		return r.report(ctx, cc, false, "OperationConflict", "The Cluster slot contains a manual or foreign operation; it will not be adopted or deleted")
	}
	if !op.DeletionTimestamp.IsZero() {
		return r.report(ctx, cc, false, "WaitingForOperationDeletion", "The exact prior operation must disappear before selecting another node")
	}
	if op.Status.Phase == "Succeeded" {
		if !companion.ValidUpgradeCompletion(op) || op.Status.Completion.Result != "Succeeded" || !currentCondition(op.Status.Conditions, op.Generation, "AllocationReleased") {
			return r.report(ctx, cc, false, "CompletionUnproven", "Success requires an exact authenticated receipt and observed allocation deletion")
		}
		if op.Status.AcknowledgedAttemptID != op.Status.AttemptID {
			return r.report(ctx, cc, false, "WaitingForAgentJournal", "The exact updater must confirm its terminal journal is durable before automatic reaping")
		}
		receipt := &ext.AgentRolloutReceipt{Operation: ext.ObservedObjectReference(objectRef(op)), NodeRecord: ext.ObservedObjectReference(op.Spec.NodeRecord), Release: ext.ObservedObjectReference(op.Spec.Release), SHA256: op.Spec.ReleaseSpec.SHA256, CompletedAt: op.Status.Completion.CompletedAt}
		if !reflect.DeepEqual(cc.Status.LastAgentUpgrade, receipt) {
			before := cc.DeepCopy()
			cc.Status.LastAgentUpgrade = receipt
			condition(&cc.Status.Conditions, cc.Generation, "AgentRolloutReady", false, "SuccessRecorded", "The latest exact successful completion is saved before operation removal")
			return persist(ctx, r.Client, before, cc)
		}
		return r.deleteOperation(ctx, cc, op, "SuccessfulOperationRecorded")
	}
	switch op.Status.Phase {
	case "", "Pending", "Reserved":
		current := cc.Spec.TargetAgentRelease == op.Spec.Release.Name
		if current {
			var release ext.AgentRelease
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.Release.Namespace, Name: op.Spec.Release.Name}, &release)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			current = err == nil && objectRef(&release) == op.Spec.Release && release.Spec == op.Spec.ReleaseSpec
		}
		if !current {
			// A Reserved operation may already have a local pre-claim journal.
			// Keep its API identity until explicit operator/updater recovery;
			// deleting it would strand that journal after a lost claim response.
			if op.Status.Allocation != nil || op.Status.AttemptID != "" {
				return r.report(ctx, cc, false, "TargetChangedRecoveryRequired", "The target changed after an assignment was exposed; recover the owning updater and explicitly cancel the operation")
			}
			return r.deleteOperation(ctx, cc, op, "TargetChanged")
		}
	case "RolledBack", "Failed", "Cancelled":
		return r.report(ctx, cc, false, "AdministratorRecoveryRequired", "A failed, rolled-back or cancelled operation is retained; investigate and explicitly remove it before retrying")
	}
	return r.report(ctx, cc, false, "OperationInProgress", "The exact selected operation must finish or recover before another node is selected")
}

func (r AgentRolloutReconciler) deleteOperation(ctx context.Context, cc *ext.ContainerCluster, op *ext.AgentUpgrade, reason string) (ctrl.Result, error) {
	uid, rv := op.UID, op.ResourceVersion
	if err := r.Client.Delete(ctx, op, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return r.report(ctx, cc, false, reason, "Requested exact operation deletion; cleanup and observed disappearance remain mandatory")
}
