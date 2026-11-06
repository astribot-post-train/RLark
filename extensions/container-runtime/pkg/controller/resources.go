package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/noderesources"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/ownership"
	tenant "github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ResourcesReconciler projects only extension-owned Raw resource fields. It uses
// an uncached client and never renews connectivity or acquires/releases locks.
type ResourcesReconciler struct {
	Client client.Client
	Now    func() time.Time
}

func (r ResourcesReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ResourcesReconciler) SetupWithManager(m ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		var nodes ext.ContainerNodeList
		if err := r.Client.List(ctx, &nodes); err != nil {
			return nil
		} // Periodic repair also inspects all dependencies.
		var requests []ctrl.Request
		for _, n := range nodes.Items {
			match := false
			switch v := obj.(type) {
			case *native.Node:
				match = n.Spec.Node.UID == string(v.UID)
			case *native.Cluster:
				match = n.Spec.Cluster.UID == string(v.UID)
			case *ext.ContainerAllocation:
				match = string(n.UID) == v.Spec.NodeRecordUID || n.Spec.Node.UID == v.Spec.Node.UID || n.Namespace == v.Namespace
			case *ext.ContainerCluster, *ext.AgentRelease:
				match = n.Namespace == obj.GetNamespace()
			}
			if match {
				requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&n)})
			}
		}
		return requests
	})
	nodeChanges := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, ok1 := e.ObjectOld.(*native.Node)
		next, ok2 := e.ObjectNew.(*native.Node)
		return !ok1 || !ok2 || old.Generation != next.Generation || !reflect.DeepEqual(old.Annotations, next.Annotations) || !reflect.DeepEqual(old.Status.Reservation, next.Status.Reservation) || !reflect.DeepEqual(old.Status.DockerReservations, next.Status.DockerReservations)
	}}
	return builder.ControllerManagedBy(m).Named("container-resources").For(&ext.ContainerNode{}).
		Watches(&native.Node{}, enqueue, builder.WithPredicates(nodeChanges)).
		Watches(&native.Cluster{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&ext.ContainerCluster{}, enqueue).Watches(&ext.ContainerAllocation{}, enqueue).
		Watches(&ext.AgentRelease{}, enqueue).Complete(r)
}

func (r ResourcesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result := ctrl.Result{RequeueAfter: heartbeatRequeue}
	var cn ext.ContainerNode
	if err := r.Client.Get(ctx, req.NamespacedName, &cn); err != nil {
		return result, client.IgnoreNotFound(err)
	}
	if cn.Spec.Runtime != "Raw" {
		return result, nil
	}
	var nn native.Node
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cn.Spec.Node.Namespace, Name: cn.Spec.Node.Name}, &nn); err != nil {
		return result, client.IgnoreNotFound(err)
	}
	if cn.UID == "" || ownership.MatchesNative(cn.Spec.Node, &nn) != nil || nn.Spec.AgentType != native.AgentTypeRaw || tenant.IDOf(&nn) != tenant.IDOf(&cn) {
		return result, invalidBinding("resource projection native identity differs")
	}
	owner := nn.Annotations[noderesources.ProjectionAnnotation]
	if owner != "" && owner != string(cn.UID) || owner == "" && (len(nn.Status.Capacity) != 0 || len(nn.Status.Allocatable) != 0 || len(nn.Status.Used) != 0 || nn.Status.NodeInfo != (native.NodeInfo{}) || len(nn.Status.Addresses) != 0) {
		return result, invalidBinding("native resource projection ownership cannot be proven")
	}
	validationErr := (ContainerNodeReconciler{Client: r.Client}).validateNode(ctx, &cn)
	var ccList ext.ContainerClusterList
	if validationErr == nil {
		validationErr = r.Client.List(ctx, &ccList, client.InNamespace(cn.Namespace))
	}
	var cc *ext.ContainerCluster
	if validationErr == nil {
		for i := range ccList.Items {
			if ccList.Items[i].Spec.Cluster == cn.Spec.Cluster {
				if cc != nil {
					validationErr = invalidBinding("ambiguous resource Cluster binding")
					break
				}
				cc = &ccList.Items[i]
			}
		}
		if cc == nil {
			validationErr = invalidBinding("resource Cluster binding missing")
		}
		if validationErr == nil {
			validationErr = (ContainerClusterReconciler{Client: r.Client}).validateCluster(ctx, cc)
		}
	}
	var nc native.Cluster
	if validationErr == nil {
		validationErr = r.Client.Get(ctx, client.ObjectKey{Namespace: cn.Spec.Cluster.Namespace, Name: cn.Spec.Cluster.Name}, &nc)
	}
	if validationErr != nil {
		// Never claim a new Node under an invalid binding. An existing proven
		// projection can be invalidated without modifying historical quantities.
		if owner != string(cn.UID) {
			return result, validationErr
		}
		return result, errors.Join(validationErr, r.patchView(ctx, &nn, &cn, noderesources.View{Reason: "ResourceBindingInvalid"}, false))
	}
	var locks ext.ContainerAllocationList
	inventoryErr := r.Client.List(ctx, &locks, client.InNamespace(cn.Namespace))
	var release *ext.AgentRelease
	if cc.Spec.TargetAgentRelease != "" {
		release = &ext.AgentRelease{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: cc.Spec.TargetAgentRelease}, release); err != nil {
			release = nil
			if !apierrors.IsNotFound(err) {
				inventoryErr = errors.Join(inventoryErr, err)
			}
		}
	}
	view := noderesources.Evaluate(&cn, &nn, cc, &nc, locks.Items, inventoryErr == nil, noderesources.ReleaseReady(&cn, cc, release), r.now())
	return result, errors.Join(inventoryErr, r.patchView(ctx, &nn, &cn, view, true))
}

func (r ResourcesReconciler) patchView(ctx context.Context, nn *native.Node, cn *ext.ContainerNode, view noderesources.View, bindingValid bool) error {
	before := nn.DeepCopy()
	if nn.Annotations == nil {
		nn.Annotations = map[string]string{}
	}
	nn.Annotations[noderesources.ProjectionAnnotation] = string(cn.UID)
	if bindingValid && view.Capacity != nil && cn.Status.ResourceObservedAt != nil {
		nn.Annotations[noderesources.ObservedAtAnnotation] = cn.Status.ResourceObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if !reflect.DeepEqual(before.Annotations, nn.Annotations) {
		if err := r.Client.Patch(ctx, nn, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("patch Raw resource ownership: %w", err)
		}
	}
	before = nn.DeepCopy()
	if bindingValid && view.Capacity != nil {
		nn.Status.Capacity = view.Capacity.DeepCopy()
		nn.Status.Allocatable = view.Capacity.DeepCopy()
		if view.AccountingKnown {
			nn.Status.Used = view.Used.DeepCopy()
		}
	}
	// System metadata does not grant resources or scheduling eligibility.
	if bindingValid && cn.Status.AgentCredentialID != "" && cn.Status.AgentCredentialID == cn.Annotations[ext.AgentCredentialAnnotation] && cn.Status.ObservedGeneration == cn.Generation {
		nn.Status.NodeInfo = native.NodeInfo{AgentVersion: cn.Status.AgentVersion, Incarnation: cn.Spec.InstanceID}
		if cn.Status.SystemInfo != nil {
			nn.Status.NodeInfo.OperatingSystem = cn.Status.SystemInfo.OperatingSystem
			nn.Status.NodeInfo.Architecture = cn.Status.SystemInfo.Architecture
			nn.Status.NodeInfo.KernelVersion = cn.Status.SystemInfo.KernelVersion
		}
		nn.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: cn.Spec.PrivateAddress}}
	}
	now := metav1.NewTime(r.now())
	for kind, ready := range map[string]bool{noderesources.ResourcesReady: view.ResourcesValid, noderesources.SchedulingReady: view.Schedulable} {
		status := metav1.ConditionFalse
		if ready {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&nn.Status.Conditions, metav1.Condition{Type: kind, Status: status, ObservedGeneration: nn.Generation, Reason: view.Reason, Message: view.Message(), LastTransitionTime: now})
	}
	if reflect.DeepEqual(before.Status, nn.Status) {
		return nil
	}
	if err := r.Client.Status().Patch(ctx, nn, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch Raw resources: %w", err)
	}
	return nil
}
