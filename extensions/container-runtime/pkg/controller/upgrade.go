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
// cancels it safely on deletion. It owns reservation/cancellation status only;
// no Agent application grant or binary installation is performed here.
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
