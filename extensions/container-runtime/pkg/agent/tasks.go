package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	jsonpatch "github.com/evanphx/json-patch/v5"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Assignment is the exact durable allocation and its current native Task. A
// suspended/deleting Task is still returned so its owner can prove cleanup.
type Assignment struct {
	Allocation ext.ContainerAllocation `json:"allocation"`
	Task       native.Task             `json:"task"`
}

// TaskRequest is the versioned node-scoped protocol. The certificate supplies
// identity; no action accepts an arbitrary Kubernetes resource or namespace.
type TaskRequest struct {
	Action          string                    `json:"action"`
	Allocation      ext.NativeObjectReference `json:"allocation,omitempty"`
	TaskUID         string                    `json:"taskUID,omitempty"`
	ResourceVersion string                    `json:"resourceVersion,omitempty"`
	Patch           json.RawMessage           `json:"patch,omitempty"`
	Status          *host.StatusIntent        `json:"status,omitempty"`
}

// TaskService mediates native Host access with current, uncached state. It
// never issues native kubeconfigs or permits general Kubernetes proxy access.
type TaskService struct{ Reporter Reporter }

func objectRef(o metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}

// nodeForCleanup proves retained identity without requiring readiness, a fresh
// heartbeat or non-deleting resources. Revoked/replaced credentials still fail.
func (s TaskService) nodeForCleanup(ctx context.Context, id Identity) (*ext.ContainerNode, error) {
	r := s.Reporter
	if r.Client == nil || id.UID == "" || id.CredentialID == "" || id.InstanceID == "" || id.TenantID == "" {
		return nil, ErrIdentity
	}
	var n ext.ContainerNode
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &n); err != nil {
		return nil, err
	}
	if string(n.UID) != id.UID || tenant.IDOf(&n) != id.TenantID || n.Annotations[CredentialAnnotation] != id.CredentialID || n.Spec.InstanceID != id.InstanceID || n.Spec.Cluster.UID != id.ClusterUID || n.Spec.Runtime != "Raw" {
		return nil, ErrIdentity
	}
	var cluster native.Cluster
	var nn native.Node
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Cluster.Namespace, Name: n.Spec.Cluster.Name}, &cluster); err != nil {
		return nil, err
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn); err != nil {
		return nil, err
	}
	if ownership.MatchesNative(n.Spec.Cluster, &cluster) != nil || ownership.MatchesNative(n.Spec.Node, &nn) != nil || cluster.Spec.AgentNamespace != n.Namespace || nn.Namespace != n.Namespace || tenant.IDOf(&cluster) != id.TenantID || tenant.IDOf(&nn) != id.TenantID || nn.Spec.AgentType != native.AgentTypeRaw || nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 {
		return nil, ErrIdentity
	}
	return &n, nil
}

func (s TaskService) assignment(ctx context.Context, id Identity) (*Assignment, error) {
	n, err := s.nodeForCleanup(ctx, id)
	if err != nil {
		return nil, err
	}
	var a ext.ContainerAllocation
	if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: n.Namespace, Name: companion.Name(n.Spec.Node.UID)}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if a.UID == "" || a.Generation < 1 || a.Spec.Cluster != n.Spec.Cluster || a.Spec.Node != n.Spec.Node || a.Spec.NodeRecordUID != string(n.UID) || a.Spec.NodeInstanceID != n.Spec.InstanceID || a.Spec.PrivateAddress != n.Spec.PrivateAddress || a.Spec.NetworkScope != n.Spec.NetworkScope || a.Spec.TenantID != id.TenantID || tenant.IDOf(&a) != id.TenantID || !sameGPUSet(a.Spec.GPUUUIDs, n.Spec.AuthorizedGPUUUIDs) || !controllerutil.ContainsFinalizer(&a, companion.Finalizer) {
		return nil, ErrIdentity
	}
	var task native.Task
	if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: a.Spec.Task.Namespace, Name: a.Spec.Task.Name}, &task); err != nil {
		return nil, err
	}
	target, err := companion.Target(&task)
	if err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(&task)
	allocationOwner := metav1.GetControllerOf(&a)
	if objectRef(&task) != a.Spec.Task || target == nil || !reflect.DeepEqual(*target, a.Spec) || task.Namespace != n.Namespace || tenant.IDOf(&task) != id.TenantID || a.Spec.ExecutionID != string(task.UID) || owner == nil || owner.UID != types.UID(a.Spec.Job.UID) || owner.Name != a.Spec.Job.Name || owner.Kind != "Job" || owner.APIVersion != native.GroupVersion.String() || allocationOwner == nil || allocationOwner.UID != task.UID || allocationOwner.Name != task.Name || allocationOwner.Kind != "Task" || allocationOwner.APIVersion != native.GroupVersion.String() || !controllerutil.ContainsFinalizer(&task, companion.TaskFinalizer) {
		return nil, ErrIdentity
	}
	claimant := task.Annotations[host.TaskClaimantAnnotation]
	if claimant != "" && claimant != n.Namespace+"/Raw/"+n.Spec.Node.UID {
		return nil, ErrIdentity
	}
	return &Assignment{Allocation: a, Task: task}, nil
}

func sameGPUSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return len(x) > 0 && slices.Equal(x, y)
}

func (s TaskService) authorize(ctx context.Context, id Identity, a *Assignment) error {
	n, err := s.nodeForCleanup(ctx, id)
	if err != nil {
		return err
	}
	r := s.Reporter
	if !n.DeletionTimestamp.IsZero() {
		return ErrIdentity
	}
	if err := r.validateBindings(ctx, n, id); err != nil {
		return err
	}
	t := &a.Task
	if !a.Allocation.DeletionTimestamp.IsZero() || a.Allocation.Status.CleanupConfirmed || !t.DeletionTimestamp.IsZero() || t.Spec.Suspended || t.Generation != a.Allocation.Spec.Generation || taskcontract.HasResult(t) {
		return ErrUnavailable
	}
	if _, err := (allocatedEligibility{assignment: a}).decode(t); err != nil {
		return err
	}
	var job native.Job
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: a.Allocation.Spec.Job.Namespace, Name: a.Allocation.Spec.Job.Name}, &job); err != nil {
		return err
	}
	if objectRef(&job) != a.Allocation.Spec.Job || len(job.Spec.Tasks) != 1 || job.Spec.SSHPublicKey != "" {
		return ErrIdentity
	}
	template := &job.Spec.Tasks[0]
	desired := template.TaskSpec.DeepCopy()
	desired.Tags = append(slices.Clone(job.Spec.Tags), desired.Tags...)
	if template.Head || template.Suspended || t.Name != utils.ChildName(job.Name, template.Name) || !taskcontract.EqualSpec(t.Spec, *desired) || t.Spec.DownstreamName != "" || t.Annotations[utils.ParentUIDAnnotation] != string(job.UID) || t.Annotations[utils.ChildTemplateAnnotation] != template.Name || t.Annotations[nativejob.RestartedAtAnnotation] != job.Annotations[nativejob.RestartedAtAnnotation] {
		return ErrIdentity
	}
	now := r.now()
	ready := meta.FindStatusCondition(n.Status.Conditions, "Ready")
	if n.Status.AgentCredentialID != id.CredentialID || n.Status.ObservedGeneration != n.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != n.Generation || n.Status.LastHeartbeatTime == nil || n.Status.LastHeartbeatTime.After(now) || now.Sub(n.Status.LastHeartbeatTime.Time) > 45*time.Second {
		return ErrUnavailable
	}
	_, releaseReady, err := r.releaseFence(ctx, n, n.Status.AgentVersion)
	if err != nil {
		return err
	}
	if !releaseReady {
		return ErrUnavailable
	}
	var cluster native.Cluster
	var nn native.Node
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Cluster.Namespace, Name: n.Spec.Cluster.Name}, &cluster); err != nil {
		return err
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn); err != nil {
		return err
	}
	if cluster.Spec.Cordoned || cluster.Spec.Draining || nn.Spec.Unschedulable {
		return ErrUnavailable
	}
	var bindings ext.ContainerJobList
	if err := r.Client.List(ctx, &bindings); err != nil {
		return err
	}
	var binding *ext.ContainerJob
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.Job.Namespace == a.Allocation.Spec.Job.Namespace && b.Spec.Job.Name == a.Allocation.Spec.Job.Name {
			if binding != nil {
				return ErrIdentity
			}
			binding = b
		}
	}
	if binding == nil || string(binding.UID) != a.Allocation.Spec.JobBindingUID {
		return ErrIdentity
	}
	recovered, err := (companion.Manager{Client: r.Client, Now: r.Now}).ValidateExisting(ctx, companion.Request{Binding: client.ObjectKeyFromObject(binding), Node: client.ObjectKeyFromObject(n), Task: client.ObjectKeyFromObject(t)})
	if err != nil {
		return err
	}
	if recovered.UID != a.Allocation.UID || !reflect.DeepEqual(recovered.Spec, a.Allocation.Spec) || recovered.Status.CleanupConfirmed {
		return ErrIdentity
	}
	return nil
}

// Handle implements only assignment, get, claim, status, authorize and cleanup.
// Every operation re-proves its certificate and exact allocation identity.
func (s TaskService) Handle(ctx context.Context, id Identity, req TaskRequest) (*Assignment, error) {
	a, err := s.assignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Action == "assignment" {
		if req.Allocation != (ext.NativeObjectReference{}) || req.TaskUID != "" || req.ResourceVersion != "" || len(req.Patch) != 0 || req.Status != nil {
			return nil, ErrCapability
		}
		return a, nil
	}
	if a == nil || req.Allocation != objectRef(&a.Allocation) || req.TaskUID != string(a.Task.UID) {
		return nil, ErrIdentity
	}
	if req.Action != "claim" && len(req.Patch) != 0 || req.Action != "status" && req.Status != nil {
		return nil, ErrCapability
	}
	t := &a.Task
	switch req.Action {
	case "get":
	case "authorize":
		if err := s.authorize(ctx, id, a); err != nil {
			return nil, err
		}
	case "claim":
		if err := s.authorize(ctx, id, a); err != nil {
			return nil, err
		}
		if req.ResourceVersion == "" || req.ResourceVersion != t.ResourceVersion {
			return nil, taskConflict(t)
		}
		if err := s.claim(ctx, id, a, req.Patch); err != nil {
			return nil, err
		}
	case "status":
		if t.Spec.Suspended || !t.DeletionTimestamp.IsZero() || t.Generation != a.Allocation.Spec.Generation || a.Allocation.Status.CleanupConfirmed {
			return nil, ErrUnavailable
		}
		if req.ResourceVersion == "" || req.ResourceVersion != t.ResourceVersion {
			return nil, taskConflict(t)
		}
		if !validStatus(t, req.Status) {
			return nil, ErrCapability
		}
		if taskcontract.Completed(t) {
			// Freeze the observed result until the controller replaces this Task.
			// A late running/pending observation cannot erase a cleanup trigger.
			return nil, ErrUnavailable
		}
		if err := (host.KubernetesStatusWriter{Client: s.Reporter.Client}).WriteStatus(ctx, t, *req.Status); err != nil {
			return nil, err
		}
	case "cleanup":
		if !t.Spec.Suspended && t.DeletionTimestamp.IsZero() && (!taskcontract.Completed(t) || t.Generation != a.Allocation.Spec.Generation) {
			return nil, ErrUnavailable
		}
		if err := s.cleanup(ctx, a); err != nil {
			return nil, err
		}
	default:
		return nil, ErrCapability
	}
	return a, nil
}

func taskConflict(t *native.Task) error {
	return apierrors.NewConflict(native.GroupVersion.WithResource("tasks").GroupResource(), t.Name, errors.New("Task changed; reread current state"))
}

var claimAnnotations = []string{host.TaskClaimantAnnotation, host.TaskClaimedWorkloadTypeAnnotation, host.TaskClaimedWorkloadVersionAnnotation, host.TaskClaimedCarrierTypeAnnotation, host.TaskClaimedCarrierVersionAnnotation}

func (s TaskService) claim(ctx context.Context, _ Identity, a *Assignment, data []byte) error {
	t := &a.Task
	patch, err := jsonpatch.DecodePatch(data)
	if err != nil {
		return ErrCapability
	}
	original, err := json.Marshal(t)
	if err != nil {
		return err
	}
	changed, err := patch.Apply(original)
	if err != nil {
		return taskConflict(t)
	}
	var desired native.Task
	if err := json.Unmarshal(changed, &desired); err != nil {
		return ErrCapability
	}
	expected := t.DeepCopy()
	if expected.Annotations == nil {
		expected.Annotations = map[string]string{}
	}
	values := []string{t.Namespace + "/Raw/" + a.Allocation.Spec.Node.UID, string(t.Spec.Workload.Kind), t.Spec.Workload.Version, t.Spec.Workload.Carrier.Type, t.Spec.Workload.Carrier.Version}
	for i, key := range claimAnnotations {
		if old := expected.Annotations[key]; old != "" && old != values[i] {
			return ErrIdentity
		}
		expected.Annotations[key] = values[i]
	}
	controllerutil.AddFinalizer(expected, host.TaskFinalizer)
	if !reflect.DeepEqual(expected, &desired) {
		return ErrCapability
	}
	// Rebuild the patch from trusted fields. Arbitrary JSON patch operations never
	// reach the management API, including tests against secret or foreign fields.
	if reflect.DeepEqual(t, expected) {
		return nil
	}
	before := t.DeepCopy()
	*t = *expected
	return s.Reporter.Client.Patch(ctx, t, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func validStatus(t *native.Task, intent *host.StatusIntent) bool {
	if intent == nil || intent.TaskUID != string(t.UID) || intent.Generation != t.Generation || t.Annotations[host.TaskClaimantAnnotation] == "" || !controllerutil.ContainsFinalizer(t, host.TaskFinalizer) || intent.ObservedNodesSet || len(intent.ObservedNodes) != 0 || len(intent.Message) > 4096 || len(intent.Conditions) > 3 {
		return false
	}
	switch native.TaskPhase(intent.Phase) {
	case "", native.TaskPhasePending, native.TaskPhaseRunning, native.TaskPhaseSucceeded, native.TaskPhaseFailed, native.TaskPhaseStopped:
	default:
		return false
	}
	seen := map[string]bool{}
	for _, m := range intent.Conditions {
		c := m.Condition
		if c.Type != "ExecutionKnown" && c.Type != "ExecutionCleaned" || seen[c.Type] || c.ObservedGeneration != t.Generation || len(c.Message) > 4096 || len(c.Reason) == 0 || len(c.Reason) > 128 || c.Status != metav1.ConditionTrue && c.Status != metav1.ConditionFalse && c.Status != metav1.ConditionUnknown {
			return false
		}
		seen[c.Type] = true
	}
	if intent.Phase == string(native.TaskPhaseSucceeded) || intent.Phase == string(native.TaskPhaseFailed) {
		candidate := t.DeepCopy()
		candidate.Status.ObservedGeneration = t.Generation
		candidate.Status.Phase = native.TaskPhase(intent.Phase)
		for _, m := range intent.Conditions {
			if m.Remove {
				meta.RemoveStatusCondition(&candidate.Status.Conditions, m.Condition.Type)
			} else {
				meta.SetStatusCondition(&candidate.Status.Conditions, m.Condition)
			}
		}
		return taskcontract.Completed(candidate)
	}
	return true
}

func (s TaskService) cleanup(ctx context.Context, a *Assignment) error {
	t := &a.Task
	// The trusted Agent calls supervisor.Stop and validates its durable cancellation
	// proof first. Remove only Host-owned metadata before publishing allocation proof,
	// so a lost response or controller release cannot strand the Task finalizer.
	before := t.DeepCopy()
	controllerutil.RemoveFinalizer(t, host.TaskFinalizer)
	for _, key := range claimAnnotations {
		delete(t.Annotations, key)
	}
	if !reflect.DeepEqual(before, t) {
		if err := s.Reporter.Client.Patch(ctx, t, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	allocation := &a.Allocation
	previous := allocation.DeepCopy()
	allocation.Status.CleanupConfirmed = true
	allocation.Status.ObservedGeneration = allocation.Generation
	allocation.Status.Phase = "Cleaned"
	meta.SetStatusCondition(&allocation.Status.Conditions, metav1.Condition{Type: "CleanupConfirmed", Status: metav1.ConditionTrue, Reason: "SupervisorCancellationConfirmed", Message: "Owning Agent confirmed durable cancellation and process-tree cleanup", ObservedGeneration: allocation.Generation, LastTransitionTime: metav1.NewTime(s.Reporter.now())})
	if reflect.DeepEqual(previous.Status, allocation.Status) {
		return nil
	}
	return s.Reporter.Client.Status().Patch(ctx, allocation, client.MergeFromWithOptions(previous, client.MergeFromWithOptimisticLock{}))
}
