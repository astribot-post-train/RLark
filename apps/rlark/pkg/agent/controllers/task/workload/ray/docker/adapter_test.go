package docker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	dockerruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/docker"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
)

func dockerSnapshot(t *testing.T, params taskapi.RayExecutionParametersV1) coordination.TaskSnapshot {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return coordination.TaskSnapshot{
		Identity:         runtime.TaskIdentity{Logical: runtime.LogicalTaskID{Namespace: "agent", Name: "task"}, ObjectUID: "uid"},
		RuntimeSelector:  coordination.RuntimeSelector{Kind: dockerruntime.RuntimeKind},
		WorkloadSelector: coordination.WorkloadSelector{Type: taskapi.WorkloadTypeRay, Version: taskapi.WorkloadVersionRayV1},
		CarrierSelector:  coordination.CarrierSelector{Type: dockerruntime.CarrierType, Version: dockerruntime.CarrierVersion},
		Configuration: coordination.WorkloadConfiguration{
			Template:   corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Image: "ray:latest"}}}},
			Parameters: raw,
		},
	}
}

func TestAdapterUsesResolvedHeadAddressAndStablePortKey(t *testing.T) {
	adapter := Adapter{}
	head, err := adapter.Compile(context.Background(), workload.CompileRequest{Task: dockerSnapshot(t, taskapi.RayExecutionParametersV1{Role: taskapi.RayRoleHead, HeadTaskName: "job-head"}), Runtime: runtime.Info{Kind: dockerruntime.RuntimeKind}})
	if err != nil {
		t.Fatal(err)
	}
	workerParams := taskapi.RayExecutionParametersV1{Role: taskapi.RayRoleWorker, HeadTaskName: "job-head", HeadAddress: "10.0.0.12"}
	worker, err := adapter.Compile(context.Background(), workload.CompileRequest{Task: dockerSnapshot(t, workerParams), Runtime: runtime.Info{Kind: dockerruntime.RuntimeKind}})
	if err != nil {
		t.Fatal(err)
	}
	if len(head.Objects) != 1 || len(worker.Objects) != 1 {
		t.Fatalf("objects: head=%d worker=%d", len(head.Objects), len(worker.Objects))
	}
	if head.CompiledHash == worker.CompiledHash {
		t.Fatal("head and worker plans unexpectedly identical")
	}
	var workerManifest map[string]any
	if err := json.Unmarshal(worker.Objects[0].Manifest.Object, &workerManifest); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(workerManifest)
	if !strings.Contains(string(encoded), "10.0.0.12") || !strings.Contains(string(encoded), "RLARK_RAY_PORT") {
		t.Fatalf("worker manifest does not contain resolved address and port: %s", encoded)
	}
}

func TestAdapterRejectsWorkerWithoutHeadAddress(t *testing.T) {
	_, err := (Adapter{}).Compile(context.Background(), workload.CompileRequest{Task: dockerSnapshot(t, taskapi.RayExecutionParametersV1{Role: taskapi.RayRoleWorker, HeadTaskName: "logical-head"}), Runtime: runtime.Info{Kind: dockerruntime.RuntimeKind}})
	if err == nil {
		t.Fatal("worker with logical-only head name was accepted")
	}
}
