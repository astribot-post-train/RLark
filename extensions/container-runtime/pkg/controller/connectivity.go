package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/rawnode"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	nodeHeartbeatPrefix        = "node-heartbeat-"
	heartbeatLeaseName         = "heartbeat"
	heartbeatDuration          = 45 * time.Second
	heartbeatRequeue           = 15 * time.Second
	nativeProjectionAnnotation = "containers.rlark.io/native-heartbeat-projection"
)

// ConnectivityReconciler bridges authenticated companion observations to native
// heartbeat Leases. Core controllers retain ownership of connection status.
// Native Raw capability and credential projections are fenced by companion UID;
// Spec and workload reservations are never changed.
type ConnectivityReconciler struct {
	Client client.Client
	Now    func() time.Time
}

func (r ConnectivityReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ConnectivityReconciler) SetupWithManager(m ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		var ref ext.NativeObjectReference
		switch v := obj.(type) {
		case *ext.ContainerNode:
			ref = v.Spec.Cluster
		case *ext.ContainerCluster:
			ref = v.Spec.Cluster
		}
		if ref.Name == "" {
			return nil
		}
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
	})
	return builder.ControllerManagedBy(m).Named("container-connectivity").
		For(&native.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&ext.ContainerNode{}, enqueue).Watches(&ext.ContainerCluster{}, enqueue).Complete(r)
}

func (r ConnectivityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster native.Cluster
	if err := r.Client.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	result := ctrl.Result{RequeueAfter: heartbeatRequeue}
	if cluster.Spec.AgentNamespace == "" || !cluster.DeletionTimestamp.IsZero() {
		return result, nil
	}
	var companions ext.ContainerClusterList
	if err := r.Client.List(ctx, &companions, client.InNamespace(cluster.Spec.AgentNamespace)); err != nil {
		return result, err
	}
	var companion *ext.ContainerCluster
	for i := range companions.Items {
		candidate := &companions.Items[i]
		if ownership.MatchesNative(candidate.Spec.Cluster, &cluster) == nil {
			if companion != nil {
				return result, invalidBinding("ambiguous container Cluster binding")
			}
			companion = candidate
		}
	}
	if companion == nil || !companion.DeletionTimestamp.IsZero() {
		return result, nil
	}
	// Recheck authoritative membership and uniqueness rather than trusting a
	// previous Validated condition across changes to dependent resources.
	if err := (ContainerClusterReconciler{Client: r.Client}).validateCluster(ctx, companion); err != nil {
		return result, err
	}
	var nodes ext.ContainerNodeList
	if err := r.Client.List(ctx, &nodes, client.InNamespace(cluster.Spec.AgentNamespace)); err != nil {
		return result, err
	}
	now := r.now()
	var latest *metav1.Time
	var failures []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.Cluster != companion.Spec.Cluster {
			continue
		}
		if !node.DeletionTimestamp.IsZero() || !conditionTrue(node.Status.Conditions, "Validated", node.Generation) || !conditionTrue(node.Status.Conditions, "Ready", node.Generation) || node.Status.ObservedGeneration != node.Generation || node.Status.AgentCredentialID == "" || node.Status.AgentCredentialID != node.Annotations[ext.AgentCredentialAnnotation] || !fresh(node.Status.LastHeartbeatTime, now) {
			continue
		}
		if err := (ContainerNodeReconciler{Client: r.Client}).validateNode(ctx, node); err != nil {
			failures = append(failures, err)
			continue
		}
		duplicate := false
		for j := range nodes.Items {
			if i != j && nodes.Items[j].Spec.Node == node.Spec.Node {
				duplicate = true
				break
			}
		}
		if duplicate {
			failures = append(failures, invalidBinding("ambiguous container Node binding"))
			continue
		}
		if err := r.projectNode(ctx, node); err != nil {
			failures = append(failures, err)
			continue
		}
		if latest == nil || latest.Before(node.Status.LastHeartbeatTime) {
			latest = node.Status.LastHeartbeatTime.DeepCopy()
		}
	}
	// Keep the original server-side observation time: reconciling an old heartbeat
	// must never extend liveness, including after this process restarts.
	if latest != nil {
		key := types.NamespacedName{Namespace: cluster.Spec.AgentNamespace, Name: heartbeatLeaseName}
		if err := r.renewLease(ctx, &cluster, "Cluster", key, string(companion.UID), string(cluster.UID), latest.Time); err != nil {
			failures = append(failures, err)
		}
	}
	// No observation means no renewal. Retain expired Leases for diagnostics;
	// native controllers expire them even if this adapter is stopped.
	return result, errors.Join(failures...)
}

func conditionTrue(conditions []metav1.Condition, kind string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == kind {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
		}
	}
	return false
}

func fresh(at *metav1.Time, now time.Time) bool {
	return at != nil && !at.After(now) && now.Sub(at.Time) <= heartbeatDuration
}

func (r ConnectivityReconciler) projectNode(ctx context.Context, node *ext.ContainerNode) error {
	var nn native.Node
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nn); err != nil {
		return err
	}
	if err := ownership.MatchesNative(node.Spec.Node, &nn); err != nil {
		return err
	}
	if nn.UID == "" || node.UID == "" || !nn.DeletionTimestamp.IsZero() || tenant.IDOf(&nn) != tenant.IDOf(node) || string(nn.Spec.AgentType) != string(node.Spec.Runtime) {
		return invalidBinding("native Node identity changed")
	}

	// Check Lease ownership before writing any projection on the native Node.
	key := types.NamespacedName{Namespace: nn.Namespace, Name: nodeHeartbeatPrefix + nn.Name}
	var lease coordinationv1.Lease
	if err := r.Client.Get(ctx, key, &lease); err == nil {
		if !projectionLeaseOwned(&lease, &nn, "Node", string(node.UID)) {
			return fmt.Errorf("heartbeat Lease %s is not owned by this container Node", key)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	if node.Spec.Runtime == "Raw" {
		owner := nn.Annotations[nativeProjectionAnnotation]
		if owner != "" && owner != string(node.UID) || owner == "" && (nn.Status.Raw != nil || nn.Annotations[rawnode.CredentialAnnotation] != "") {
			return invalidBinding("native Raw projection ownership cannot be proven")
		}
		desired := &native.RawNodeStatus{Incarnation: node.Spec.InstanceID, TrainingIP: node.Spec.PrivateAddress, NetworkScope: node.Spec.NetworkScope, Environment: node.Spec.Environment, AuthorizedGPUUUIDs: append([]string(nil), node.Spec.AuthorizedGPUUUIDs...)}
		if nn.Status.Raw != nil && !rawnode.SameCapability(nn.Status.Raw, desired) {
			return invalidBinding("native Raw capability projection conflicts with the registered ContainerNode")
		}
		before := nn.DeepCopy()
		if nn.Annotations == nil {
			nn.Annotations = map[string]string{}
		}
		nn.Annotations[nativeProjectionAnnotation] = string(node.UID)
		nn.Annotations[rawnode.CredentialAnnotation] = node.Status.AgentCredentialID
		if !reflect.DeepEqual(nn.Annotations, before.Annotations) {
			if err := r.Client.Patch(ctx, &nn, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("patch native Raw identity: %w", err)
			}
		}
		before = nn.DeepCopy()
		nn.Status.Raw = desired
		if !rawnode.SameCapability(before.Status.Raw, nn.Status.Raw) {
			if err := r.Client.Status().Patch(ctx, &nn, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("patch native Raw capability: %w", err)
			}
		}
	}
	holder := rawnode.Identity{NodeUID: string(nn.UID), Incarnation: node.Spec.InstanceID, CredentialID: node.Status.AgentCredentialID}.HeartbeatHolder()
	return r.renewLease(ctx, &nn, "Node", key, string(node.UID), holder, node.Status.LastHeartbeatTime.Time)
}

func projectionLeaseOwned(lease *coordinationv1.Lease, parent client.Object, kind, projection string) bool {
	owner := metav1.GetControllerOf(lease)
	return projection != "" && parent.GetUID() != "" && lease.Annotations[nativeProjectionAnnotation] == projection && owner != nil && owner.APIVersion == native.GroupVersion.String() && owner.Kind == kind && owner.Name == parent.GetName() && owner.UID == parent.GetUID()
}

func (r ConnectivityReconciler) renewLease(ctx context.Context, parent client.Object, kind string, key types.NamespacedName, projection, holder string, at time.Time) error {
	if projection == "" || parent.GetUID() == "" {
		return invalidBinding("heartbeat projection UID required")
	}
	var lease coordinationv1.Lease
	err := r.Client.Get(ctx, key, &lease)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return fmt.Errorf("read heartbeat Lease: %w", err)
	}
	if !missing && !projectionLeaseOwned(&lease, parent, kind, projection) {
		return fmt.Errorf("heartbeat Lease %s ownership cannot be proven", key)
	}
	before := lease.DeepCopy()
	if missing {
		owner := metav1.NewControllerRef(parent, native.GroupVersion.WithKind(kind))
		// Heartbeat cleanup must never block native resource deletion, and this
		// adapter needs no permission to modify the parent's finalizers.
		blockDeletion := false
		owner.BlockOwnerDeletion = &blockDeletion
		lease = coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: map[string]string{nativeProjectionAnnotation: projection}, OwnerReferences: []metav1.OwnerReference{*owner}}}
	}
	renew := metav1.NewMicroTime(at)
	duration := int32(heartbeatDuration / time.Second)
	lease.Spec.RenewTime = &renew
	lease.Spec.LeaseDurationSeconds = &duration
	lease.Spec.HolderIdentity = &holder
	if missing {
		return r.Client.Create(ctx, &lease)
	}
	if reflect.DeepEqual(before.Spec, lease.Spec) {
		return nil
	}
	return r.Client.Patch(ctx, &lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
