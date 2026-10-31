package controller

import (
	"context"
	"errors"
	"reflect"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// AgentUpgradeReconciler reserves an explicit operation's shared node lock and
// cancels it safely on deletion and consumes authenticated completion receipts.
// The API owns application grants/reports; this controller owns lock cleanup.
type AgentUpgradeReconciler struct {
	Client client.Client
	Now    func() time.Time
}

// SetupWithManager installs the opt-in upgrade reservation controller.
func (r *AgentUpgradeReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&ext.AgentUpgrade{}).Owns(&ext.ContainerAllocation{}).Complete(r)
}

func (r AgentUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, errors.New("uncached management client required")
	}
	var op ext.AgentUpgrade
	if err := r.Client.Get(ctx, req.NamespacedName, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	manager := companion.Manager{Client: r.Client, Now: r.Now}
	request := companion.UpgradeRequest{Operation: client.ObjectKeyFromObject(&op)}
	if op.Status.Completion != nil {
		return r.complete(ctx, &op, manager, request)
	}
	if !op.DeletionTimestamp.IsZero() {
		return r.cancel(ctx, &op, manager, request)
	}
	if op.Status.Phase == "Applying" || op.Status.Phase == "Succeeded" || op.Status.Phase == "RolledBack" || op.Status.Phase == "Failed" || op.Status.Phase == "Cancelled" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !controllerutil.ContainsFinalizer(&op, companion.UpgradeFinalizer) {
		if err := manager.ValidateUpgradeIntent(ctx, request); err != nil {
			// Invalid or foreign operations are not adopted or protected.
			return ctrl.Result{}, err
		}
		before := op.DeepCopy()
		controllerutil.AddFinalizer(&op, companion.UpgradeFinalizer)
		if err := r.Client.Patch(ctx, &op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	allocation, err := manager.ReserveUpgrade(ctx, request)
	if err != nil {
		before := op.DeepCopy()
		op.Status.ObservedGeneration = op.Generation
		// Preserve previously recorded lock identity, including after an outage.
		if op.Status.Allocation == nil {
			op.Status.Phase = "Pending"
		}
		condition(&op.Status.Conditions, op.Generation, "Reserved", false, upgradeReason(err), "The exact node lock is unavailable; no Agent installation is authorized")
		result, patchErr := r.persist(ctx, before, &op)
		if patchErr != nil {
			return result, patchErr
		}
		if errors.Is(err, companion.ErrUnavailable) || errors.Is(err, companion.ErrIdentity) || apierrors.IsNotFound(err) {
			return result, nil
		}
		return result, err
	}
	if allocation.UID == "" {
		return ctrl.Result{}, companion.ErrUnavailable
	}
	ref := ext.NativeObjectReference{Namespace: allocation.Namespace, Name: allocation.Name, UID: string(allocation.UID)}
	if op.Status.Allocation != nil && *op.Status.Allocation != ref {
		return ctrl.Result{}, companion.ErrIdentity
	}
	before := op.DeepCopy()
	op.Status.ObservedGeneration = op.Generation
	op.Status.Phase = "Reserved"
	op.Status.Allocation = &ref
	condition(&op.Status.Conditions, op.Generation, "Reserved", true, "NodeLockReserved", "The shared node lock is reserved; a separate scoped grant is required before installation")
	return r.persist(ctx, before, &op)
}

func (r AgentUpgradeReconciler) complete(ctx context.Context, op *ext.AgentUpgrade, manager companion.Manager, req companion.UpgradeRequest) (ctrl.Result, error) {
	// Deletion can advance a CR generation in some Kubernetes-compatible API
	// servers after the terminal status was durably written. Once the controller
	// has recorded the authenticated completion and exact allocation release,
	// finalizer removal must remain recoverable even when that deletion-time
	// generation no longer matches the receipt's observed generation.
	if !op.DeletionTimestamp.IsZero() && op.Status.ObservedGeneration > 0 && op.Generation == op.Status.ObservedGeneration+1 && controllerutil.ContainsFinalizer(op, companion.UpgradeFinalizer) {
		// Spec is immutable by admission. Validate the full saved receipt at its
		// original generation without rewriting either Spec or observed status.
		completed := op.DeepCopy()
		completed.Generation = completed.Status.ObservedGeneration
		if companion.ValidUpgradeCompletion(completed) && completed.Status.Phase == completed.Status.Completion.Result && currentCondition(completed.Status.Conditions, completed.Generation, "AllocationReleased") {
			return r.removeFinalizer(ctx, op)
		}
	}
	if !companion.ValidUpgradeCompletion(op) || !controllerutil.ContainsFinalizer(op, companion.UpgradeFinalizer) || op.Status.Phase != "Applying" && op.Status.Phase != op.Status.Completion.Result {
		return ctrl.Result{}, companion.ErrIdentity
	}
	// The current-generation terminal proof was written only after observing
	// this operation's allocation disappear. A later workload may now hold the
	// same node key. Never inspect or mutate that replacement while finalizing
	// this already-completed operation.
	if op.Status.Phase == op.Status.Completion.Result && currentCondition(op.Status.Conditions, op.Generation, "AllocationReleased") {
		if !op.DeletionTimestamp.IsZero() {
			return r.removeFinalizer(ctx, op)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	// ObserveUpgrade deliberately returns a nil allocation for a replacement at
	// the deterministic name. That is safe for Agent reads, but a controller
	// must never treat a replacement as deletion of its witnessed lock.
	if op.Status.Allocation != nil {
		var witnessed ext.ContainerAllocation
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: op.Status.Allocation.Namespace, Name: op.Status.Allocation.Name}, &witnessed)
		if err == nil {
			if string(witnessed.UID) != op.Status.Allocation.UID {
				return ctrl.Result{}, companion.ErrIdentity
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	current, _, allocation, err := manager.ObserveUpgrade(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current.ResourceVersion != op.ResourceVersion {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if allocation != nil {
		before := allocation.DeepCopy()
		allocation.Status.ObservedGeneration = allocation.Generation
		allocation.Status.CleanupConfirmed = true
		allocation.Status.Phase = op.Status.Completion.Result
		condition(&allocation.Status.Conditions, allocation.Generation, "CleanupConfirmed", true, "UpgradeCompletionConfirmed", "The exact granted updater reported a fresh idle result for the pinned version")
		if !reflect.DeepEqual(before.Status, allocation.Status) {
			if err := r.Client.Status().Patch(ctx, allocation, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := manager.ReleaseUpgrade(ctx, client.ObjectKeyFromObject(allocation), string(allocation.UID), string(op.UID)); err != nil {
			if errors.Is(err, companion.ErrUnavailable) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	before := op.DeepCopy()
	op.Status.Phase = op.Status.Completion.Result
	op.Status.ObservedGeneration = op.Generation
	condition(&op.Status.Conditions, op.Generation, "AllocationReleased", true, "UpgradeAllocationDeleted", "Deletion of the exact upgrade allocation was observed; the operation is retained for administrator review")
	if !reflect.DeepEqual(before.Status, op.Status) {
		return r.persist(ctx, before, op)
	}
	if !op.DeletionTimestamp.IsZero() {
		return r.removeFinalizer(ctx, op)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r AgentUpgradeReconciler) persist(ctx context.Context, before, after *ext.AgentUpgrade) (ctrl.Result, error) {
	if !reflect.DeepEqual(before.Status, after.Status) {
		if err := r.Client.Status().Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r AgentUpgradeReconciler) cancel(ctx context.Context, op *ext.AgentUpgrade, manager companion.Manager, req companion.UpgradeRequest) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(op, companion.UpgradeFinalizer) {
		return ctrl.Result{}, nil
	}
	// Never infer that an applying/uncertain operation finished from deletion,
	// timeout, heartbeat loss or absence of an allocation.
	if op.Status.Phase != "" && op.Status.Phase != "Pending" && op.Status.Phase != "Reserved" && op.Status.Phase != "Cancelled" {
		before := op.DeepCopy()
		condition(&op.Status.Conditions, op.Generation, "DeletionBlocked", true, "ApplicationUnconfirmed", "The owning updater must prove completion or rollback before this operation can be removed")
		return r.persist(ctx, before, op)
	}
	if op.Status.Phase != "Cancelled" || op.Status.ObservedGeneration != op.Generation {
		before := op.DeepCopy()
		op.Status.Phase = "Cancelled"
		op.Status.ObservedGeneration = op.Generation
		condition(&op.Status.Conditions, op.Generation, "Cancelled", true, "CancelledBeforeApplication", "The operation was fenced before any installation grant")
		// CAS wins against a concurrent future Applying transition.
		return r.persist(ctx, before, op)
	}
	// A saved witness permits recovery from a lost finalizer/delete response.
	if op.Status.Allocation != nil {
		var current ext.ContainerAllocation
		key := client.ObjectKey{Namespace: op.Status.Allocation.Namespace, Name: op.Status.Allocation.Name}
		err := r.Client.Get(ctx, key, &current)
		if apierrors.IsNotFound(err) {
			return r.removeFinalizer(ctx, op)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if string(current.UID) != op.Status.Allocation.UID {
			return ctrl.Result{}, companion.ErrIdentity
		}
	}
	allocation, err := manager.ReserveCancelledUpgrade(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	ref := ext.NativeObjectReference{Namespace: allocation.Namespace, Name: allocation.Name, UID: string(allocation.UID)}
	if ref.UID == "" {
		return ctrl.Result{}, companion.ErrUnavailable
	}
	if op.Status.Allocation == nil {
		before := op.DeepCopy()
		op.Status.Allocation = &ref
		return r.persist(ctx, before, op)
	}
	if *op.Status.Allocation != ref {
		return ctrl.Result{}, companion.ErrIdentity
	}
	before := allocation.DeepCopy()
	allocation.Status.ObservedGeneration = allocation.Generation
	allocation.Status.CleanupConfirmed = true
	allocation.Status.Phase = "Cancelled"
	condition(&allocation.Status.Conditions, allocation.Generation, "CleanupConfirmed", true, "UpgradeCancelledBeforeApplication", "The exact operation was durably cancelled before application; no workload process is owned")
	if !reflect.DeepEqual(before.Status, allocation.Status) {
		if err := r.Client.Status().Patch(ctx, allocation, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := manager.ReleaseUpgrade(ctx, client.ObjectKeyFromObject(allocation), string(allocation.UID), string(op.UID)); err != nil {
		return ctrl.Result{}, err
	}
	return r.removeFinalizer(ctx, op)
}

func (r AgentUpgradeReconciler) removeFinalizer(ctx context.Context, op *ext.AgentUpgrade) (ctrl.Result, error) {
	before := op.DeepCopy()
	controllerutil.RemoveFinalizer(op, companion.UpgradeFinalizer)
	return ctrl.Result{}, r.Client.Patch(ctx, op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func upgradeReason(err error) string {
	if errors.Is(err, companion.ErrIdentity) {
		return "IdentityConflict"
	}
	if errors.Is(err, companion.ErrUnavailable) {
		return "NodeLockUnavailable"
	}
	return "DependencyUnavailable"
}
