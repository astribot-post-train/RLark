package docker

import (
	"context"
	"errors"
	"fmt"
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	executionplan "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/plan"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"k8s.io/apimachinery/pkg/api/resource"
	"slices"
)

// Fits validates requested CPU, memory and exact whole-GPU entitlement before
// placement. Missing or malformed observed capacity is unavailable, never zero.
func Fits(task *native.Task, node *ext.ContainerNode) error {
	if task == nil || node == nil || task.Spec.Workload.Template == nil || len(task.Spec.Workload.Template.Spec.Containers) != 1 {
		return errors.New("ray container resources are absent")
	}
	cpu, memory, gpu, err := resources(task.Spec.Workload.Template.Spec.Containers[0].Resources)
	if err != nil {
		return err
	}
	availableCPU, err := resource.ParseQuantity(node.Status.Capacity["cpu"])
	if err != nil {
		return fmt.Errorf("node CPU capacity: %w", err)
	}
	availableMemory, err := resource.ParseQuantity(node.Status.Capacity["memory"])
	if err != nil {
		return fmt.Errorf("node memory capacity: %w", err)
	}
	if availableCPU.Cmp(*resource.NewMilliQuantity(cpu, resource.DecimalSI)) < 0 || availableMemory.Cmp(*resource.NewQuantity(memory, resource.BinarySI)) < 0 || len(node.Spec.AuthorizedGPUUUIDs) == 0 || gpu != 0 && gpu != int64(len(node.Spec.AuthorizedGPUUUIDs)) {
		return errors.New("ray request does not fit complete host entitlement")
	}
	return nil
}

// AssignmentFor derives trusted runtime material from one exact allocation.
func AssignmentFor(task *native.Task, a ext.ContainerAllocationSpec) (Assignment, error) {
	if task == nil || !AllocationMatches(a) || a.Task.Name != task.Name || a.Task.Namespace != task.Namespace || a.Task.UID != string(task.UID) || a.Generation != task.Generation {
		return Assignment{}, errors.New("ray task does not match group allocation")
	}
	snapshot, err := dockerruntime.DecodeTask(task)
	if err != nil {
		return Assignment{}, err
	}
	addresses := make([]string, len(a.Group.Plan.Members))
	for i, m := range a.Group.Plan.Members {
		addresses[i] = m.PrivateAddr
	}
	return Assignment{Task: snapshot.Identity, Addresses: addresses, Rank: a.Group.MemberIndex, Ports: PortPlan{Base: a.Group.Plan.Ports.Base, WorkerCount: a.Group.Plan.Ports.WorkerCount}, GPUUUIDs: slices.Clone(a.GPUUUIDs)}, nil
}

// CompileAllocation uses only exact allocation input and the public Task spec.
func CompileAllocation(task *native.Task, a ext.ContainerAllocationSpec) (executionplan.Plan, error) {
	assignment, err := AssignmentFor(task, a)
	if err != nil {
		return executionplan.Plan{}, err
	}
	snapshot, err := dockerruntime.DecodeTask(task)
	if err != nil {
		return executionplan.Plan{}, err
	}
	return (Adapter{Assignment: assignment}).Compile(context.Background(), workload.CompileRequest{Task: snapshot, Runtime: rt.Info{Kind: dockerruntime.RuntimeKind}})
}
