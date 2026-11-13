package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const nodeOperationFinalizer = "station.rlark.io/node-operation-cleanup"

// NodeOperationExecutor must observe the requested effect before taking action.
// It returns done only after the effect has been independently verified.
type NodeOperationExecutor interface {
	Execute(context.Context, *stationv1alpha1.NodeOperation, string) (done bool, nodeUID string, err error)
}

type NodeOperationReconciler struct {
	client        client.Client
	reader        client.Reader
	registrations ClusterResolver
	executor      NodeOperationExecutor
}

func NewNodeOperationReconciler(writer client.Client, reader client.Reader, registrations ClusterResolver, executor NodeOperationExecutor) (*NodeOperationReconciler, error) {
	if writer == nil || reader == nil || registrations == nil || executor == nil {
		return nil, errors.New("writer, reader, cluster resolver and node executor are required")
	}
	return &NodeOperationReconciler{client: writer, reader: reader, registrations: registrations, executor: executor}, nil
}

var nodeOperationStages = map[string][]string{
	"Join":            {"Preflight", "ResetExisting", "Join", "VerifyNode", "ConfigureAgentRuntime", "VerifyRLark", "CleanupCredential"},
	"Remove":          {"Preflight", "Drain", "RemovePeer", "Reset", "DeleteNode", "VerifyRemoval", "CleanupCredential"},
	"EnableWireGuard": {"Preflight", "BindStation", "AllocateAddress", "ApplyPeer", "VerifyWireGuard"},
}

func (r *NodeOperationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	op := &stationv1alpha1.NodeOperation{}
	if err := r.reader.Get(ctx, req.NamespacedName, op); apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("read node operation: %w", err)
	}
	if op.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(op, nodeOperationFinalizer) {
			return reconcile.Result{}, nil
		}
		registration, err := r.registrations.ResolveNamespace(ctx, op.Namespace)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("resolve deleting operation cluster: %w", err)
		}
		if registration.Namespace != op.Namespace || registration.ClusterID != op.Spec.ClusterID || op.UID == "" {
			return reconcile.Result{}, ErrInvalidRegistration
		}
		cleaner, ok := r.executor.(NodeOperationCleanupExecutor)
		if !ok {
			return reconcile.Result{}, errors.New("node operation cleanup is not available")
		}
		done, err := cleaner.CleanupOperation(ctx, op.DeepCopy())
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("clean deleting node operation: %w", err)
		}
		if !done {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
		}
		updated := op.DeepCopy()
		controllerutil.RemoveFinalizer(updated, nodeOperationFinalizer)
		if err := r.client.Patch(ctx, updated, client.MergeFromWithOptions(op, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("remove node operation finalizer: %w", err)
		}
		return reconcile.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(op, nodeOperationFinalizer) {
		updated := op.DeepCopy()
		controllerutil.AddFinalizer(updated, nodeOperationFinalizer)
		if err := r.client.Patch(ctx, updated, client.MergeFromWithOptions(op, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("add node operation finalizer: %w", err)
		}
		return reconcile.Result{Requeue: true}, nil
	}
	if op.Status.Phase == "Succeeded" || op.Status.Phase == "Failed" {
		return reconcile.Result{}, nil
	}
	registration, err := r.registrations.ResolveNamespace(ctx, op.Namespace)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("resolve cluster: %w", err)
	}
	if registration.Namespace != op.Namespace || registration.ClusterID != op.Spec.ClusterID || op.UID == "" {
		return reconcile.Result{}, ErrInvalidRegistration
	}
	stages, ok := nodeOperationStages[op.Spec.Action]
	if !ok {
		return reconcile.Result{}, errors.New("unsupported node operation action")
	}
	next := 0
	if op.Status.Stage != "" {
		found := false
		for i, stage := range stages {
			if stage == op.Status.Stage {
				next, found = i+1, true
				break
			}
		}
		if !found {
			return reconcile.Result{}, errors.New("unknown node operation stage")
		}
	}
	if next == len(stages) {
		updated := op.DeepCopy()
		updated.Status.Phase = "Succeeded"
		updated.Status.ObservedGeneration = op.Generation
		now := metav1.Now()
		updated.Status.CompletedAt = &now
		if err := r.client.Status().Patch(ctx, updated, client.MergeFromWithOptions(op, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("complete node operation: %w", err)
		}
		return reconcile.Result{}, nil
	}
	done, uid, err := r.executor.Execute(ctx, op.DeepCopy(), stages[next])
	if err != nil {
		if ctx.Err() != nil {
			return reconcile.Result{}, ctx.Err()
		}
		updated := op.DeepCopy()
		updated.Status.Phase = "Blocked"
		updated.Status.ObservedGeneration = op.Generation
		reason, message := nodeOperationFailureCondition(stages[next], err)
		apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{Type: "Progressing", Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: op.Generation})
		if patchErr := r.client.Status().Patch(ctx, updated, client.MergeFromWithOptions(op, client.MergeFromWithOptimisticLock{})); patchErr != nil {
			return reconcile.Result{}, fmt.Errorf("record node operation failure: %w", patchErr)
		}
		return reconcile.Result{}, fmt.Errorf("node operation %s: %w", stages[next], err)
	}
	if !done {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if op.Spec.Action == "Join" && stages[next] == "Join" && uid == "" {
		return reconcile.Result{}, errors.New("verified join must return the member Node UID")
	}
	if op.Spec.NodeUID != "" && uid != "" && op.Spec.NodeUID != uid {
		return reconcile.Result{}, ErrNodeIdentityChanged
	}
	if op.Status.NodeUID != "" && uid != "" && op.Status.NodeUID != uid {
		return reconcile.Result{}, ErrNodeIdentityChanged
	}
	updated := op.DeepCopy()
	updated.Status.Phase = "Running"
	updated.Status.Stage = stages[next]
	updated.Status.ObservedGeneration = op.Generation
	apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "StageCompleted", Message: "Verified node operation stage " + stages[next], ObservedGeneration: op.Generation})
	if uid != "" {
		updated.Status.NodeUID = uid
	}
	if err := r.client.Status().Patch(ctx, updated, client.MergeFromWithOptions(op, client.MergeFromWithOptimisticLock{})); err != nil {
		return reconcile.Result{}, fmt.Errorf("record node operation stage: %w", err)
	}
	return reconcile.Result{Requeue: true}, nil
}

func nodeOperationFailureCondition(stage string, err error) (string, string) {
	switch {
	case errors.Is(err, ErrStationRebindRequired):
		return "StationRebindRequired", "A Station is still bound to another Kubernetes Node UID. Rebind it to a fresh, verified observation before retrying this Join."
	case errors.Is(err, ErrNodeIdentityChanged):
		return "NodeIdentityChanged", "Node UID changed or the target Node is deleting; no different Node was modified."
	case errors.Is(err, ErrNodeDrainBlocked):
		return "DrainBlocked", "Drain is blocked by a protected Node, workload, local emptyDir data or a disruption budget."
	case errors.Is(err, ErrWireGuardPeerOwnershipUnknown):
		return "WireGuardOwnershipUnknown", "WireGuard peer ownership could not be proven for this Node UID; removal stopped before drain."
	case errors.Is(err, ErrNodeStageUnavailable):
		return "PrerequisiteUnavailable", "A required node-operation integration or permission is not configured."
	case errors.Is(err, ErrNodeAlreadyConfigured):
		return "NodeAlreadyConfigured", "The target already has Kubernetes configuration; automatic adoption is not allowed."
	default:
		return "StageFailed", "Node operation stage " + stage + " failed; inspect controller logs and current Node state before retrying."
	}
}

// SetupWithManager is called only when a complete executor is configured.
func (r *NodeOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return errors.New("manager is required")
	}
	return ctrl.NewControllerManagedBy(mgr).For(&stationv1alpha1.NodeOperation{}).Complete(r)
}
