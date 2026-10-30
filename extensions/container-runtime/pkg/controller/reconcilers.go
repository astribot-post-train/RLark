// Package controller reconciles the experimental companion API. Validation is
// deliberately separate from Agent observation, allocation and runtime effects.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/rawnode"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const clusterFinalizer = "containers.rlark.io/cluster-protection"
const nodeFinalizer = "containers.rlark.io/node-protection"
const repairInterval = 30 * time.Second

var errInvalidBinding = errors.New("invalid companion binding")

func invalidBinding(message string) error { return fmt.Errorf("%w: %s", errInvalidBinding, message) }

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func condition(items *[]metav1.Condition, generation int64, kind string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(items, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation})
}

// persist avoids status-event loops and uses optimistic concurrency. All these
// validation statuses have a single controller owner; Agent fields are untouched.
func persist(ctx context.Context, c client.Client, before, after client.Object) (ctrl.Result, error) {
	if !reflect.DeepEqual(before, after) {
		if err := c.Status().Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: repairInterval}, nil
}

// ContainerClusterReconciler validates native instance, tenant and membership.
// It owns Validated and phase, never connectivity or allocation.
type ContainerClusterReconciler struct{ Client client.Client }

// SetupWithManager installs the validation controller. Periodic fresh inspection
// repairs dependency changes without relying on a complete watch history.
func (r *ContainerClusterReconciler) SetupWithManager(m ctrl.Manager) error {
	return builder.ControllerManagedBy(m).For(&ext.ContainerCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}
func (r ContainerClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var v ext.ContainerCluster
	if err := r.Client.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !v.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&v, clusterFinalizer) {
			return ctrl.Result{}, nil
		}
		var nodes ext.ContainerNodeList
		var allocations ext.ContainerAllocationList
		if err := r.Client.List(ctx, &nodes); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.List(ctx, &allocations); err != nil {
			return ctrl.Result{}, err
		}
		blocked := false
		for _, n := range nodes.Items {
			if n.Spec.Cluster == v.Spec.Cluster {
				blocked = true
			}
		}
		for _, a := range allocations.Items {
			if a.Spec.Cluster == v.Spec.Cluster {
				blocked = true
			}
		}
		before := v.DeepCopy()
		if blocked {
			condition(&v.Status.Conditions, v.Generation, "DeletionBlocked", true, "ChildrenRemain", "Container nodes or allocations still reference this native Cluster instance")
			return persist(ctx, r.Client, before, &v)
		}
		controllerutil.RemoveFinalizer(&v, clusterFinalizer)
		return ctrl.Result{}, r.Client.Patch(ctx, &v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if !controllerutil.ContainsFinalizer(&v, clusterFinalizer) {
		before := v.DeepCopy()
		controllerutil.AddFinalizer(&v, clusterFinalizer)
		if err := r.Client.Patch(ctx, &v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := v.DeepCopy()
	validErr := r.validateCluster(ctx, &v)
	v.Status.ObservedGeneration = v.Generation
	v.Status.Phase = "Configured"
	if validErr != nil {
		v.Status.Phase = "Invalid"
	}
	reason, message := "BindingValidated", "Native Cluster instance and authoritative namespace are valid; no Agent connectivity is implied"
	if validErr != nil {
		reason, message = "InvalidBinding", "Native Cluster, tenant, runtime, or authoritative membership is invalid"
	}
	condition(&v.Status.Conditions, v.Generation, "Validated", validErr == nil, reason, message)
	result, err := persist(ctx, r.Client, before, &v)
	if err != nil {
		return result, err
	}
	if validErr != nil && !errors.Is(validErr, errInvalidBinding) && !apierrors.IsNotFound(validErr) {
		return ctrl.Result{}, validErr
	}
	return result, nil
}
func (r ContainerClusterReconciler) validateCluster(ctx context.Context, v *ext.ContainerCluster) error {
	if v.Spec.Runtime != "Raw" && v.Spec.Runtime != "Docker" {
		return invalidBinding("unsupported runtime")
	}
	var nativeCluster native.Cluster
	if err := getNative(ctx, r.Client, v.Spec.Cluster, &nativeCluster); err != nil {
		return err
	}
	if v.Namespace != nativeCluster.Spec.AgentNamespace || v.Spec.AgentNamespace != nativeCluster.Spec.AgentNamespace || v.Spec.AgentNamespace == "" || tenant.IDOf(v) != tenant.IDOf(&nativeCluster) {
		return invalidBinding("tenant or authoritative namespace differs")
	}
	var clusters native.ClusterList
	if err := r.Client.List(ctx, &clusters); err != nil {
		return err
	}
	for _, other := range clusters.Items {
		if other.UID != nativeCluster.UID && other.Spec.AgentNamespace == nativeCluster.Spec.AgentNamespace {
			return invalidBinding("ambiguous authoritative namespace")
		}
	}
	var companions ext.ContainerClusterList
	if err := r.Client.List(ctx, &companions); err != nil {
		return err
	}
	for _, other := range companions.Items {
		if other.UID != v.UID && other.Spec.Cluster == v.Spec.Cluster {
			return invalidBinding("ambiguous container Cluster binding")
		}
	}
	return nil
}

// ContainerNodeReconciler owns validation only. Schedulable remains false until
// a scoped Agent transport, trusted liveness and capability admission exist.
type ContainerNodeReconciler struct{ Client client.Client }

func (r *ContainerNodeReconciler) SetupWithManager(m ctrl.Manager) error {
	return builder.ControllerManagedBy(m).For(&ext.ContainerNode{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}
func (r ContainerNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var v ext.ContainerNode
	if err := r.Client.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !v.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&v, nodeFinalizer) {
			return ctrl.Result{}, nil
		}
		var allocations ext.ContainerAllocationList
		if err := r.Client.List(ctx, &allocations); err != nil {
			return ctrl.Result{}, err
		}
		for _, allocation := range allocations.Items {
			if allocation.Spec.NodeRecordUID == string(v.UID) || allocation.Spec.Node == v.Spec.Node {
				before := v.DeepCopy()
				v.Status.Schedulable = false
				condition(&v.Status.Conditions, v.Generation, "DeletionBlocked", true, "AllocationsRemain", "Allocation cleanup must finish before removing the container Node")
				return persist(ctx, r.Client, before, &v)
			}
		}
		before := v.DeepCopy()
		controllerutil.RemoveFinalizer(&v, nodeFinalizer)
		return ctrl.Result{}, r.Client.Patch(ctx, &v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if !controllerutil.ContainsFinalizer(&v, nodeFinalizer) {
		before := v.DeepCopy()
		controllerutil.AddFinalizer(&v, nodeFinalizer)
		if err := r.Client.Patch(ctx, &v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := v.DeepCopy()
	validErr := r.validateNode(ctx, &v)
	v.Status.ObservedGeneration = v.Generation
	v.Status.Schedulable = false
	reason, message := "BindingValidated", "Native Node instance, tenant and Cluster membership are valid"
	if validErr != nil {
		reason, message = "InvalidBinding", "Native Node, tenant, runtime, incarnation or membership is invalid"
	}
	condition(&v.Status.Conditions, v.Generation, "Validated", validErr == nil, reason, message)
	condition(&v.Status.Conditions, v.Generation, "Ready", false, "AgentNotObserved", "Scoped Agent transport and capability observation are not enabled")
	result, err := persist(ctx, r.Client, before, &v)
	if err != nil {
		return result, err
	}
	if validErr != nil && !errors.Is(validErr, errInvalidBinding) && !apierrors.IsNotFound(validErr) {
		return ctrl.Result{}, validErr
	}
	return result, nil
}
func (r ContainerNodeReconciler) validateNode(ctx context.Context, v *ext.ContainerNode) error {
	if err := rawnode.ValidateCapability(&native.RawNodeStatus{Incarnation: v.Spec.InstanceID, TrainingIP: v.Spec.PrivateAddress, NetworkScope: v.Spec.NetworkScope, Environment: v.Spec.Environment, AuthorizedGPUUUIDs: v.Spec.AuthorizedGPUUUIDs}); err != nil {
		return invalidBinding("private IPv4 address, environment and authorized GPU UUIDs are required")
	}
	var cluster native.Cluster
	var node native.Node
	if err := getNative(ctx, r.Client, v.Spec.Cluster, &cluster); err != nil {
		return err
	}
	if err := getNative(ctx, r.Client, v.Spec.Node, &node); err != nil {
		return err
	}
	if v.Namespace != cluster.Spec.AgentNamespace || node.Namespace != cluster.Spec.AgentNamespace || v.Spec.InstanceID == "" || tenant.IDOf(v) != tenant.IDOf(&cluster) || tenant.IDOf(&node) != tenant.IDOf(&cluster) {
		return invalidBinding("invalid node membership or tenant")
	}
	if string(node.Spec.AgentType) != string(v.Spec.Runtime) || node.Status.Reservation != nil || len(node.Status.DockerReservations) != 0 {
		return invalidBinding("runtime mismatch or active legacy reservation")
	}
	var list ext.ContainerClusterList
	if err := r.Client.List(ctx, &list, client.InNamespace(v.Namespace)); err != nil {
		return err
	}
	var companion *ext.ContainerCluster
	for i := range list.Items {
		if list.Items[i].Spec.Cluster == v.Spec.Cluster {
			if companion != nil {
				return invalidBinding("ambiguous container Cluster")
			}
			companion = &list.Items[i]
		}
	}
	if companion == nil || companion.Spec.Runtime != v.Spec.Runtime || !companion.DeletionTimestamp.IsZero() {
		return invalidBinding("container Cluster unavailable")
	}
	return (ContainerClusterReconciler{Client: r.Client}).validateCluster(ctx, companion)
}

// ContainerJobReconciler validates a typed external ownership binding without
// claiming the Job or dispatching Tasks during the extraction stage.
type ContainerJobReconciler struct{ Client client.Client }

func (r *ContainerJobReconciler) SetupWithManager(m ctrl.Manager) error {
	return builder.ControllerManagedBy(m).For(&ext.ContainerJob{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}
func (r ContainerJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var v ext.ContainerJob
	if err := r.Client.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := v.DeepCopy()
	validErr := r.validateJob(ctx, &v)
	v.Status.ObservedGeneration = v.Generation
	v.Status.Phase = "Validated"
	reason, message := "BindingValidated", "Typed Job ownership is valid; scheduling is not enabled"
	if validErr != nil {
		v.Status.Phase = "Rejected"
		reason, message = "InvalidBinding", "Native Job instance, declared controller, runtime, Cluster or tenant does not match"
	}
	condition(&v.Status.Conditions, v.Generation, "Validated", validErr == nil, reason, message)
	result, err := persist(ctx, r.Client, before, &v)
	if err != nil {
		return result, err
	}
	if validErr != nil && !errors.Is(validErr, errInvalidBinding) && !apierrors.IsNotFound(validErr) {
		return ctrl.Result{}, validErr
	}
	return result, nil
}
func (r ContainerJobReconciler) validateJob(ctx context.Context, v *ext.ContainerJob) error {
	var job native.Job
	if err := getNative(ctx, r.Client, v.Spec.Job, &job); err != nil {
		return err
	}
	if err := ownership.JobBindingMatches(v, &job, tenant.IDOf(&job)); err != nil {
		return invalidBinding("Job UID, tenant or runtime differs")
	}
	if tenant.IDOf(v) != tenant.IDOf(&job) || job.Annotations[nativejob.ControllerOwnerAnnotation] != ext.Group {
		return invalidBinding("tenant or declared controller differs")
	}
	if v.Spec.Runtime != "Raw" && v.Spec.Runtime != "Docker" {
		return invalidBinding("unsupported runtime")
	}
	for _, task := range job.Spec.Tasks {
		if string(task.Runtime.Kind) != string(v.Spec.Runtime) {
			return invalidBinding("mixed runtimes are unsupported")
		}
	}
	var cluster native.Cluster
	if err := getNative(ctx, r.Client, v.Spec.Cluster, &cluster); err != nil {
		return err
	}
	if tenant.IDOf(&cluster) != v.Spec.TenantID || cluster.Spec.AgentNamespace != v.Namespace {
		return invalidBinding("wrong Cluster tenant or namespace")
	}
	return nil
}

// AgentReleaseReconciler separates metadata validation from availability. No
// artifact is advertised available before authenticated publication is wired.
type AgentReleaseReconciler struct{ Client client.Client }

func (r *AgentReleaseReconciler) SetupWithManager(m ctrl.Manager) error {
	return builder.ControllerManagedBy(m).For(&ext.AgentRelease{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}
func (r AgentReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var v ext.AgentRelease
	if err := r.Client.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := v.DeepCopy()
	valid := v.Spec.Version != "" && v.Spec.OS == "linux" && (v.Spec.Architecture == "amd64" || v.Spec.Architecture == "arm64") && sha256Pattern.MatchString(v.Spec.SHA256) && v.Spec.ArtifactPath != "" && v.Spec.Protocol == "v1"
	reason, message := "MetadataValidated", "Release metadata is valid; artifact publication is not enabled"
	if !valid {
		reason, message = "InvalidArtifact", "A supported platform/protocol, version, path and lowercase SHA-256 are required"
	}
	v.Status.ObservedGeneration = v.Generation
	v.Status.Available = false
	condition(&v.Status.Conditions, v.Generation, "Validated", valid, reason, message)
	condition(&v.Status.Conditions, v.Generation, "Available", false, "NotPublished", "Authenticated artifact publication is not enabled")
	return persist(ctx, r.Client, before, &v)
}
