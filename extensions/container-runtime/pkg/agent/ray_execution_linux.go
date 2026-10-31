//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	ray "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type rayStartAdmission struct{ scoped ScopedTasks }

func (s rayStartAdmission) Authorize(ctx context.Context, t rt.TaskIdentity) error {
	if t.ObjectUID != s.scoped.Task.UID || t.Logical.Namespace != s.scoped.Task.Namespace || t.Logical.Name != s.scoped.Task.Name {
		return ErrIdentity
	}
	a, err := s.scoped.Client.Task(ctx, s.scoped.request("authorize-start"))
	if err != nil {
		return err
	}
	if s.scoped.accept(a) != nil || a.Task.Generation != t.Generation {
		return ErrIdentity
	}
	return nil
}

func (o DockerExecutionObserver) reconcileRay(ctx context.Context, a *Assignment, provider *dockerruntime.ScopedProvider, scoped ScopedTasks, identity rt.TaskIdentity, allowStart bool) error {
	task := &a.Task
	if !ray.AllocationMatches(a.Allocation.Spec) {
		return ErrIdentity
	}
	stopping := a.GroupPhase == "Stopping" || a.GroupPhase == "Succeeded" || a.GroupPhase == "Failed" || task.Spec.Suspended || !task.DeletionTimestamp.IsZero() || taskcontract.Observed(task) && taskcontract.HasResult(task)
	if stopping {
		if _, err := provider.Cleanup(ctx, identity); err != nil {
			return err
		}
		// For natural results preserve the immutable phase while confirming cleanup.
		if !task.Spec.Suspended && task.DeletionTimestamp.IsZero() && taskcontract.HasResult(task) && !taskcontract.Completed(task) {
			if err := scoped.WriteStatus(ctx, task, host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: string(task.Status.Phase), Conditions: []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionCleaned", Status: metav1.ConditionTrue, Reason: "DockerRemovalConfirmed", ObservedGeneration: task.Generation}}}}); err != nil {
				return err
			}
		}
		_, err := o.Client.Task(ctx, scoped.request("cleanup"))
		return err
	}
	if !allowStart || a.GroupPhase != "Preparing" && a.GroupPhase != "Starting" {
		return nil
	}
	for _, gpu := range a.Allocation.Spec.GPUUUIDs {
		if !slices.Contains(o.GPUUUIDs, gpu) {
			return ErrCapability
		}
	}
	if err := claimRay(ctx, scoped, task, a.Allocation.Spec.Node.UID); err != nil {
		return err
	}
	plan, err := ray.CompileAllocation(task, a.Allocation.Spec)
	if err != nil {
		return err
	}
	if len(plan.Objects) != 1 {
		return ErrCapability
	}
	req := rt.ApplyRequest{Task: identity, Desired: plan.Objects[0]}
	var result rt.ApplyResult
	if a.Allocation.Status.Prepared == nil {
		result, err = provider.Prepare(ctx, req)
		if err != nil {
			return err
		}
		receipt := &ext.ContainerPreparedReceipt{GroupID: a.Allocation.Spec.Group.ID, GroupDigest: a.Allocation.Spec.Group.Digest, AllocationUID: string(a.Allocation.UID), TaskUID: string(task.UID), Generation: task.Generation, EngineID: o.EngineID, ContainerID: result.Ref.ID, ManifestDigest: plan.CompiledHash}
		request := scoped.request("prepared")
		request.ResourceVersion = task.ResourceVersion
		request.Prepared = receipt
		_, err = o.Client.Task(ctx, request)
		return err
	}
	if !ray.PreparedMatches(&a.Allocation) {
		return ErrIdentity
	}
	if a.GroupPhase != "Starting" {
		return nil
	}
	provider.StartAdmission = rayStartAdmission{scoped: scoped}
	preparedID := a.Allocation.Status.Prepared.ContainerID
	req.Current = &rt.ObjectRef{RuntimeID: dockerruntime.DefaultID, ID: preparedID, UID: preparedID, Namespace: task.Namespace, Name: task.Name, LogicalTask: identity.Logical}
	result, err = provider.StartPrepared(ctx, req)
	if err != nil {
		if fault.Is(err, fault.FailedPrecondition) || fault.Is(err, fault.Conflict) || fault.Is(err, fault.InvalidArgument) {
			// A proven prepared instance cannot be replaced. Publish a bounded
			// terminal failure so the controller fences and cleans every sibling.
			return errors.Join(err, scoped.WriteStatus(ctx, task, host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: "Failed", MessageSet: true, Message: "Exact prepared Ray container could not be recovered; replacement is forbidden", Conditions: []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionKnown", Status: metav1.ConditionTrue, Reason: "PreparedContainerLost", ObservedGeneration: task.Generation}}}}))
		}
		return err
	}
	instances, err := provider.ListInstances(ctx, rt.InstanceQuery{Task: identity, Workloads: []rt.ObjectRef{result.Ref}})
	if err != nil {
		return err
	}
	if len(instances.Instances) != 1 {
		return errors.New("prepared Ray container observation is absent or ambiguous")
	}
	instance := instances.Instances[0]
	phase := string(instance.Phase)
	if phase != "Pending" && phase != "Running" && phase != "Succeeded" && phase != "Failed" {
		return errors.New("ray container phase is unknown")
	}
	return scoped.WriteStatus(ctx, task, host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: phase, MessageSet: true, Message: instance.Message, Conditions: []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionKnown", Status: metav1.ConditionTrue, Reason: "RayContainerObserved", ObservedGeneration: task.Generation}}, {Condition: metav1.Condition{Type: "ExecutionCleaned", Status: metav1.ConditionFalse, Reason: "RayAllocationRetained", ObservedGeneration: task.Generation}}}})
}

func claimRay(ctx context.Context, scoped ScopedTasks, t *native.Task, nodeUID string) error {
	desired := t.DeepCopy()
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	values := []string{t.Namespace + "/Docker/" + nodeUID, string(t.Spec.Workload.Kind), t.Spec.Workload.Version, t.Spec.Workload.Carrier.Type, t.Spec.Workload.Carrier.Version}
	for i, k := range claimAnnotations {
		if old := desired.Annotations[k]; old != "" && old != values[i] {
			return ErrIdentity
		}
		desired.Annotations[k] = values[i]
	}
	controllerutil.AddFinalizer(desired, host.TaskFinalizer)
	data, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/resourceVersion", "value": t.ResourceVersion}, {"op": "add", "path": "/metadata/annotations", "value": desired.Annotations}, {"op": "add", "path": "/metadata/finalizers", "value": desired.Finalizers}})
	if err != nil {
		return err
	}
	return scoped.Patch(ctx, t, client.RawPatch(types.JSONPatchType, data))
}
