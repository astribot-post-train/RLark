package agent

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrUpgradeIdentity rejects an upgrade outside the certificate-bound node.
	ErrUpgradeIdentity = errors.New("container Agent upgrade identity conflict")
	// ErrUpgradeConflict rejects competing attempts and conflicting results.
	ErrUpgradeConflict = errors.New("container Agent upgrade operation conflict")
	// ErrUpgradeUnavailable means prerequisites cannot currently be proven.
	ErrUpgradeUnavailable = errors.New("container Agent upgrade unavailable")
)

// UpgradeRequest is the only wire request accepted by the node-scoped upgrade
// endpoint. The certificate supplies node identity; these references fence a
// replay against one operation and allocation incarnation.
type UpgradeRequest struct {
	Action          string                    `json:"action"`
	Operation       ext.NativeObjectReference `json:"operation"`
	Allocation      ext.NativeObjectReference `json:"allocation,omitempty"`
	AttemptID       string                    `json:"attemptID,omitempty"`
	ResourceVersion string                    `json:"resourceVersion,omitempty"`
	Result          string                    `json:"result,omitempty"`
	Version         string                    `json:"version,omitempty"`
	Idle            *bool                     `json:"idle,omitempty"`
}

// UpgradeRelease omits the server artifact path and exposes only immutable
// release identity needed by the updater.
type UpgradeRelease struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Protocol     string `json:"protocol"`
	SHA256       string `json:"sha256"`
}

// UpgradeResponse is a bounded assignment/operation view. It is not an
// installation grant until claim succeeds.
type UpgradeResponse struct {
	Granted               bool                        `json:"granted"`
	Operation             ext.NativeObjectReference   `json:"operation"`
	Allocation            *ext.NativeObjectReference  `json:"allocation,omitempty"`
	Phase                 string                      `json:"phase"`
	AttemptID             string                      `json:"attemptID,omitempty"`
	ResourceVersion       string                      `json:"resourceVersion"`
	StartedAt             *metav1.Time                `json:"startedAt,omitempty"`
	Release               UpgradeRelease              `json:"release"`
	FromVersion           string                      `json:"fromVersion"`
	Completion            *ext.AgentUpgradeCompletion `json:"completion,omitempty"`
	AcknowledgedAttemptID string                      `json:"acknowledgedAttemptID,omitempty"`
}

func upgradeRef(o metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}

func sameUpgradeRef(a, b ext.NativeObjectReference) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name && a.UID != "" && a.UID == b.UID
}

func upgradeRelease(spec ext.AgentReleaseSpec, ref ext.NativeObjectReference) UpgradeRelease {
	return UpgradeRelease{Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID, Version: spec.Version, OS: spec.OS, Architecture: spec.Architecture, Protocol: spec.Protocol, SHA256: spec.SHA256}
}

func (r Reporter) upgradeState(ctx context.Context, id Identity, request UpgradeRequest) (*ext.AgentUpgrade, *ext.ContainerNode, *ext.ContainerAllocation, error) {
	op, node, allocation, err := (companion.Manager{Client: r.Client, Now: r.Now, HeartbeatTimeout: 45 * time.Second}).ObserveUpgrade(ctx, companion.UpgradeRequest{Operation: client.ObjectKey{Namespace: request.Operation.Namespace, Name: request.Operation.Name}})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil, ErrUpgradeIdentity
		}
		return nil, nil, nil, err
	}
	if !sameUpgradeRef(request.Operation, upgradeRef(op)) || string(node.UID) != id.UID || node.Name != id.Name || node.Namespace != id.Namespace || node.Spec.InstanceID != id.InstanceID || node.Spec.Cluster.UID != id.ClusterUID || node.Annotations[CredentialAnnotation] != id.CredentialID || op.Spec.CredentialID != id.CredentialID || op.Spec.TenantID != id.TenantID || tenant.IDOf(node) != id.TenantID || !op.DeletionTimestamp.IsZero() && request.Action == "claim" {
		return nil, nil, nil, ErrUpgradeIdentity
	}
	if allocation != nil && request.Allocation.UID != "" && !sameUpgradeRef(request.Allocation, upgradeRef(allocation)) {
		return nil, nil, nil, ErrUpgradeIdentity
	}
	return op, node, allocation, nil
}

func (r Reporter) upgradeResponse(op *ext.AgentUpgrade, allocation *ext.ContainerAllocation) *UpgradeResponse {
	response := &UpgradeResponse{Operation: upgradeRef(op), Phase: op.Status.Phase, AttemptID: op.Status.AttemptID, ResourceVersion: op.ResourceVersion, StartedAt: op.Status.StartedAt, Release: upgradeRelease(op.Spec.ReleaseSpec, op.Spec.Release), FromVersion: op.Spec.FromVersion, Completion: op.Status.Completion, AcknowledgedAttemptID: op.Status.AcknowledgedAttemptID}
	if allocation != nil {
		ref := upgradeRef(allocation)
		response.Allocation = &ref
	}
	return response
}

func (r Reporter) freshUpgradeObservation(node *ext.ContainerNode, version string, after *metav1.Time) error {
	if node.Spec.Runtime == "Docker" && !slices.Contains(node.Status.Capabilities, UpgradeCapability) {
		return ErrUpgradeUnavailable
	}
	if node.Status.AgentCredentialID == "" || node.Status.AgentCredentialID != node.Annotations[CredentialAnnotation] || node.Status.ObservedGeneration != node.Generation || node.Status.Busy || node.Status.AgentVersion != version || node.Status.LastHeartbeatTime == nil || node.Status.LastHeartbeatTime.After(r.now()) || r.now().Sub(node.Status.LastHeartbeatTime.Time) > 45*time.Second {
		return ErrUpgradeUnavailable
	}
	ready := meta.FindStatusCondition(node.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != node.Generation {
		return ErrUpgradeUnavailable
	}
	if after != nil && !node.Status.LastHeartbeatTime.After(after.Time) {
		return ErrUpgradeUnavailable
	}
	return nil
}

// Upgrade executes one node-scoped action. It never writes Spec or creates an
// operation/allocation and never removes a reservation finalizer.
func (r Reporter) Upgrade(ctx context.Context, id Identity, request UpgradeRequest) (*UpgradeResponse, error) {
	if err := validateUpgradeRequest(request); err != nil {
		return nil, err
	}
	if _, err := (TaskService{Reporter: r}).nodeForCleanup(ctx, id); err != nil {
		return nil, err
	}
	if request.Action == "assignment" {
		var op ext.AgentUpgrade
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: companion.UpgradeName(id.ClusterUID)}, &op); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		// A Cluster-wide slot may currently belong to a different node.
		if op.Spec.NodeRecord.UID != id.UID || op.Status.Allocation == nil || op.Status.Phase == "Pending" || op.Status.Phase == "" {
			return nil, nil
		}
		request.Operation = upgradeRef(&op)
	}
	op, node, allocation, err := r.upgradeState(ctx, id, request)
	if err != nil {
		return nil, err
	}
	if request.Action == "assignment" || request.Action == "get" {
		if request.Action == "get" && (op.Status.Allocation == nil || request.Allocation != *op.Status.Allocation) {
			return nil, ErrUpgradeIdentity
		}
		return r.upgradeResponse(op, allocation), nil
	}
	if request.Action == "complete" && companion.ValidUpgradeCompletion(op) && request.Allocation == *op.Status.Allocation && request.AttemptID == op.Status.AttemptID && request.Result == op.Status.Completion.Result && request.Version == op.Status.Completion.Version {
		return r.upgradeResponse(op, allocation), nil
	}
	if request.Action == "acknowledge" {
		if !companion.ValidUpgradeCompletion(op) || request.Allocation != *op.Status.Allocation || request.AttemptID != op.Status.AttemptID || request.Result != op.Status.Completion.Result || request.Version != op.Status.Completion.Version {
			return nil, ErrUpgradeConflict
		}
		if err := r.freshUpgradeObservation(node, request.Version, op.Status.StartedAt); err != nil {
			return nil, err
		}
		if op.Status.AcknowledgedAttemptID != "" && op.Status.AcknowledgedAttemptID != request.AttemptID {
			return nil, ErrUpgradeConflict
		}
		if op.Status.AcknowledgedAttemptID == "" {
			before := op.DeepCopy()
			op.Status.AcknowledgedAttemptID = request.AttemptID
			if err := r.Client.Status().Patch(ctx, op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, err
			}
		}
		return r.upgradeResponse(op, allocation), nil
	}
	if allocation == nil || op.Status.Allocation == nil || !sameUpgradeRef(*op.Status.Allocation, upgradeRef(allocation)) || !sameUpgradeRef(request.Allocation, upgradeRef(allocation)) {
		return nil, ErrUpgradeIdentity
	}
	switch request.Action {
	case "claim":
		return r.claimUpgrade(ctx, id, request, op, node, allocation)
	case "complete":
		return r.completeUpgrade(ctx, id, request, op, node, allocation)
	default:
		return nil, ErrUpgradeIdentity
	}
}

func (r Reporter) claimUpgrade(ctx context.Context, id Identity, request UpgradeRequest, op *ext.AgentUpgrade, node *ext.ContainerNode, allocation *ext.ContainerAllocation) (*UpgradeResponse, error) {
	if node.Spec.Runtime == "Docker" && !slices.Contains(node.Status.Capabilities, "agent-platform/"+op.Spec.ReleaseSpec.OS+"/"+op.Spec.ReleaseSpec.Architecture) {
		return nil, ErrUpgradeUnavailable
	}
	if !companion.ValidUpgradeAttempt(request.AttemptID) {
		return nil, ErrUpgradeIdentity
	}
	retry := op.Status.Phase == "Applying" && op.Status.AttemptID == request.AttemptID && op.Status.StartedAt != nil
	if !retry && (op.Status.Phase != "Reserved" || request.ResourceVersion != op.ResourceVersion || op.Status.AttemptID != "") || op.Status.Completion != nil {
		return nil, ErrUpgradeConflict
	}
	if op.Status.ObservedGeneration != op.Generation || !allocation.DeletionTimestamp.IsZero() || allocation.Status.CleanupConfirmed {
		return nil, ErrUpgradeUnavailable
	}
	if err := r.freshUpgradeObservation(node, op.Spec.FromVersion, nil); err != nil {
		return nil, err
	}
	if r.ReleaseStore == nil {
		return nil, ErrUpgradeUnavailable
	}
	manifest, _, err := r.Release(ctx, id, ReleaseRequest{Action: "manifest", OS: op.Spec.ReleaseSpec.OS, Architecture: op.Spec.ReleaseSpec.Architecture}, r.ReleaseStore)
	if err != nil {
		return nil, errors.Join(ErrUpgradeUnavailable, err)
	}
	if manifest == nil || manifest.UID != op.Spec.Release.UID || manifest.SHA256 != op.Spec.ReleaseSpec.SHA256 || manifest.Version != op.Spec.ReleaseSpec.Version {
		return nil, ErrUpgradeUnavailable
	}
	current, currentNode, currentAllocation, err := r.upgradeState(ctx, id, request)
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != op.ResourceVersion || currentAllocation == nil || currentAllocation.ResourceVersion != allocation.ResourceVersion || !reflect.DeepEqual(currentNode.Spec, node.Spec) {
		return nil, ErrUpgradeConflict
	}
	if err := r.freshUpgradeObservation(currentNode, op.Spec.FromVersion, nil); err != nil {
		return nil, err
	}
	if retry {
		return r.finishUpgradeGrant(ctx, id, request)
	}
	before := op.DeepCopy()
	now := metav1.NewTime(r.now())
	op.Status.Phase = "Applying"
	op.Status.AttemptID = request.AttemptID
	op.Status.StartedAt = &now
	op.Status.ObservedGeneration = op.Generation
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: "ApplicationGranted", Status: metav1.ConditionTrue, Reason: "ScopedUpgradeClaimed", Message: "The exact Agent upgrade attempt may install the pinned release", ObservedGeneration: op.Generation, LastTransitionTime: now})
	if err := r.Client.Status().Patch(ctx, op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, err
	}
	return r.finishUpgradeGrant(ctx, id, request)
}

func (r Reporter) finishUpgradeGrant(ctx context.Context, id Identity, request UpgradeRequest) (*UpgradeResponse, error) {
	op, node, allocation, err := r.upgradeState(ctx, id, request)
	if err != nil {
		return nil, err
	}
	_, release, err := r.targetRelease(ctx, id)
	if err != nil {
		return nil, err
	}
	if release.Spec != op.Spec.ReleaseSpec || upgradeRef(release) != op.Spec.Release || op.Status.Phase != "Applying" || op.Status.AttemptID != request.AttemptID || op.Status.Completion != nil || allocation == nil || !allocation.DeletionTimestamp.IsZero() || allocation.Status.CleanupConfirmed {
		return nil, ErrUpgradeConflict
	}
	if err := r.freshUpgradeObservation(node, op.Spec.FromVersion, nil); err != nil {
		return nil, err
	}
	response := r.upgradeResponse(op, allocation)
	response.Granted = true
	return response, nil
}

func (r Reporter) completeUpgrade(ctx context.Context, id Identity, request UpgradeRequest, op *ext.AgentUpgrade, node *ext.ContainerNode, allocation *ext.ContainerAllocation) (*UpgradeResponse, error) {
	if op.Status.Completion != nil {
		return nil, ErrUpgradeConflict
	}
	if op.Status.Phase != "Applying" || op.Status.AttemptID != request.AttemptID || op.Status.StartedAt == nil || !companion.ValidUpgradeAttempt(request.AttemptID) || request.Idle == nil || !*request.Idle {
		return nil, ErrUpgradeConflict
	}
	if request.Result != "Succeeded" && request.Result != "RolledBack" {
		return nil, ErrUpgradeIdentity
	}
	wantVersion := op.Spec.ReleaseSpec.Version
	if request.Result == "RolledBack" {
		wantVersion = op.Spec.FromVersion
	}
	if request.Version != wantVersion {
		return nil, ErrUpgradeIdentity
	}
	if err := r.freshUpgradeObservation(node, wantVersion, op.Status.StartedAt); err != nil {
		return nil, err
	}
	grant := meta.FindStatusCondition(op.Status.Conditions, "ApplicationGranted")
	if grant == nil || grant.Status != metav1.ConditionTrue || grant.ObservedGeneration != op.Generation {
		return nil, ErrUpgradeConflict
	}
	before := op.DeepCopy()
	now := metav1.NewTime(r.now())
	op.Status.ObservedGeneration = op.Generation
	op.Status.Completion = &ext.AgentUpgradeCompletion{AttemptID: request.AttemptID, Result: request.Result, Version: request.Version, HeartbeatAt: *node.Status.LastHeartbeatTime, CompletedAt: now}
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: "ApplicationComplete", Status: metav1.ConditionTrue, Reason: request.Result, Message: "The updater reported the exact version installed and the node idle", ObservedGeneration: op.Generation, LastTransitionTime: now})
	if err := r.Client.Status().Patch(ctx, op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, err
	}
	return r.upgradeResponse(op, allocation), nil
}

func validateUpgradeRequest(req UpgradeRequest) error {
	if req.Action == "assignment" {
		if req.Operation != (ext.NativeObjectReference{}) || req.Allocation != (ext.NativeObjectReference{}) || req.AttemptID != "" || req.ResourceVersion != "" || req.Result != "" || req.Version != "" || req.Idle != nil {
			return ErrCapability
		}
		return nil
	}
	if req.Operation.Namespace == "" || req.Operation.Name == "" || req.Operation.UID == "" {
		return ErrUpgradeIdentity
	}
	switch req.Action {
	case "get":
		if req.Allocation.Namespace == "" || req.Allocation.Name == "" || req.Allocation.UID == "" || req.AttemptID != "" || req.ResourceVersion != "" || req.Result != "" || req.Version != "" || req.Idle != nil {
			return ErrCapability
		}
	case "claim", "complete", "acknowledge":
		if !companion.ValidUpgradeAttempt(req.AttemptID) || req.Allocation.Namespace == "" || req.Allocation.Name == "" || req.Allocation.UID == "" {
			return ErrCapability
		}
		if req.Action == "claim" && (req.ResourceVersion == "" || req.Result != "" || req.Version != "" || req.Idle != nil) {
			return ErrCapability
		}
		if (req.Action == "complete" || req.Action == "acknowledge") && (req.ResourceVersion != "" || req.Idle == nil || !*req.Idle || req.Result != "Succeeded" && req.Result != "RolledBack") {
			return ErrCapability
		}
	default:
		return ErrCapability
	}
	return nil
}
