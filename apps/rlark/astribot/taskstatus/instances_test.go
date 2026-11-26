package taskstatus

import (
	"reflect"
	"testing"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	agentruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

func TestPerNodeInstancesAreSortedByNodeAndKeepTheMainImage(t *testing.T) {
	instances := agentruntime.InstanceSnapshot{Instances: []agentruntime.Instance{
		{Name: "task-z", Phase: "Pending", Conditions: []agentruntime.InstanceCondition{{Scope: "instance", Subject: "task-z", Reason: "FailedScheduling"}, {Scope: "image", Subject: "main", Message: "station:v2"}}},
		{Name: "task-y", Phase: "Running", Conditions: []agentruntime.InstanceCondition{{Scope: "node", Subject: "a"}, {Scope: "image", Subject: "sidecar", Message: "tools:v1"}, {Scope: "image", Subject: "main", Message: "station:v1"}, {Scope: "instance", Subject: "task-y", Reason: "Ready"}}},
	}}
	got := PerNodeInstances(instances)
	want := []rlarkv1alpha1.TaskInstanceStatus{
		{Name: "task-z", Phase: rlarkv1alpha1.TaskInstancePending, Image: "station:v2", Message: "FailedScheduling"},
		{Node: "a", Name: "task-y", Phase: rlarkv1alpha1.TaskInstanceRunning, Image: "station:v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("instances=%#v, want %#v", got, want)
	}
}
