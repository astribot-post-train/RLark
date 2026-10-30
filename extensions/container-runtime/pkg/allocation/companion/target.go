package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// TargetAnnotation stores controller-owned scheduling intent. It grants no
// execution authority without a matching, current ContainerAllocation.
const TargetAnnotation = "containers.rlark.io/allocation-target"

// TaskFinalizer retains the Task identity through allocation and cleanup, even
// if deletion races an accepted allocation Create with a lost response.
const TaskFinalizer = "containers.rlark.io/allocation-protection"

// Target reads the canonical typed scheduling intent. Absence is distinct from
// malformed or ambiguous intent; callers must never silently retarget either.
func Target(task *native.Task) (*ext.ContainerAllocationSpec, error) {
	data, present := task.Annotations[TargetAnnotation]
	if !present {
		return nil, nil
	}
	var target ext.ContainerAllocationSpec
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&target); err != nil {
		return nil, fmt.Errorf("%w: invalid allocation target", ErrIdentity)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || target.Task != reference(task) || target.Node.UID == "" || target.NodeRecordUID == "" || target.NodeInstanceID == "" || target.Cluster.UID == "" || target.Job.UID == "" || target.JobBindingUID == "" || target.Generation < 1 || target.JobGeneration < 1 || target.TenantID == "" || target.ExecutionID != string(task.UID) {
		return nil, fmt.Errorf("%w: incomplete or stale allocation target", ErrIdentity)
	}
	return &target, nil
}

// pinTarget uses one Task resourceVersion as the single-target decision point.
// A failed/uncertain patch is returned to reconciliation, never followed by a
// reservation Create or a fallback node. The persisted target survives release.
func (m Manager) pinTarget(ctx context.Context, task *native.Task, wanted ext.ContainerAllocationSpec) error {
	target, err := Target(task)
	if err != nil {
		return err
	}
	if target != nil {
		if !reflect.DeepEqual(*target, wanted) || !controllerutil.ContainsFinalizer(task, TaskFinalizer) {
			return fmt.Errorf("%w: Task already has a different allocation target or lost its protection", ErrIdentity)
		}
		return nil
	}
	data, err := json.Marshal(wanted)
	if err != nil {
		return fmt.Errorf("encode allocation target: %w", err)
	}
	before := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[TargetAnnotation] = string(data)
	controllerutil.AddFinalizer(task, TaskFinalizer)
	if err := m.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persist Task allocation target: %w", err)
	}
	return nil
}
