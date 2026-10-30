package docker

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
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
		Identity:        runtime.TaskIdentity{Logical: runtime.LogicalTaskID{Namespace: "agent", Name: "task"}, ObjectUID: "uid", Generation: 1},
		RuntimeSelector: coordination.RuntimeSelector{Kind: dockerruntime.RuntimeKind}, WorkloadSelector: coordination.WorkloadSelector{Type: taskapi.WorkloadTypeRay, Version: taskapi.WorkloadVersionRayV1}, CarrierSelector: coordination.CarrierSelector{Type: dockerruntime.CarrierType, Version: dockerruntime.CarrierVersion},
		Configuration: coordination.WorkloadConfiguration{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Image: "ray:2.56.1"}}}}, Parameters: raw},
	}
}
func fixture(t *testing.T, head bool) (Adapter, workload.CompileRequest) {
	t.Helper()
	params := taskapi.RayExecutionParametersV1{Role: taskapi.RayRoleWorker}
	rank := int32(1)
	if head {
		params.Role = taskapi.RayRoleHead
		params.Run = &taskapi.InlineScript{Inline: "printf training"}
		rank = 0
	}
	snap := dockerSnapshot(t, params)
	return Adapter{Assignment: Assignment{Task: snap.Identity, Addresses: []string{"10.0.0.12", "10.0.0.13"}, Rank: rank, Ports: PortPlan{Base: 24000, WorkerCount: 32}, GPUUUIDs: []string{"GPU-a"}}}, workload.CompileRequest{Task: snap, Runtime: runtime.Info{Kind: dockerruntime.RuntimeKind}}
}
func manifest(t *testing.T, a Adapter, r workload.CompileRequest) dockerruntime.Manifest {
	t.Helper()
	plan, err := a.Compile(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Objects) != 1 {
		t.Fatalf("objects=%d", len(plan.Objects))
	}
	var m dockerruntime.Manifest
	if err := json.Unmarshal(plan.Objects[0].Manifest.Object, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestAdapterCompilesOnlyExplicitExactAssignment(t *testing.T) {
	for _, head := range []bool{true, false} {
		a, r := fixture(t, head)
		m := manifest(t, a, r)
		if !slices.Equal(m.Spec.GPUUUIDs, a.Assignment.GPUUUIDs) || m.Spec.GPUCount != 0 || !slices.Equal(m.Spec.RayPorts, a.Assignment.Ports.TCP()) {
			t.Fatalf("allocation material lost: %+v", m.Spec)
		}
		if !slices.Equal(m.Spec.Entrypoint[:3], []string{"python3", "-u", "-c"}) || len(m.Spec.Command) != 1 {
			t.Fatal("launcher is not explicit executable material")
		}
		var config launchConfig
		if err := json.Unmarshal([]byte(m.Spec.Command[0]), &config); err != nil {
			t.Fatal(err)
		}
		if config.Head != head || config.Address != "10.0.0.12:24000" || !slices.Equal(config.Members, a.Assignment.Addresses) {
			t.Fatalf("config=%+v", config)
		}
		for _, flag := range []string{"--block", "--node-ip-address=", "--object-manager-port=", "--node-manager-port=", "--dashboard-agent-listen-port=", "--dashboard-agent-grpc-port=", "--runtime-env-agent-port=", "--metrics-export-port=", "--min-worker-port=", "--max-worker-port=", "--num-gpus=1"} {
			if !strings.Contains(strings.Join(config.Start, " "), flag) {
				t.Fatalf("missing %s: %v", flag, config.Start)
			}
		}
		if head {
			for _, flag := range []string{"--head", "--port=24000", "--ray-client-server-port=24003", "--dashboard-port=24004"} {
				if !slices.Contains(config.Start, flag) {
					t.Fatalf("missing %s", flag)
				}
			}
		}
		second := manifest(t, a, r)
		if !slices.Equal(m.Spec.Command, second.Spec.Command) {
			t.Fatal("recompile changed immutable launcher")
		}
	}
}
func TestAdapterRejectsMissingForeignOrMalformedAssignmentAndFields(t *testing.T) {
	tests := map[string]func(*Adapter, *workload.CompileRequest){
		"missing assignment": func(a *Adapter, _ *workload.CompileRequest) { a.Assignment = Assignment{} },
		"uid":                func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Task.ObjectUID = "foreign" },
		"generation":         func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Task.Generation++ },
		"duplicate IP":       func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Addresses[1] = a.Assignment.Addresses[0] },
		"public IP":          func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Addresses[1] = "8.8.8.8" },
		"hostname":           func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Addresses[1] = "worker.internal" },
		"rank":               func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Rank = 2 },
		"port overflow":      func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Ports.Base = 65530 },
		"worker bound":       func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Ports.WorkerCount = math.MaxInt32 },
		"GPU missing":        func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.GPUUUIDs = nil },
		"GPU duplicate":      func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.GPUUUIDs = []string{"GPU-a", "GPU-a"} },
		"user resolved": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Parameters = []byte(`{"role":"head","headAddress":"10.0.0.12"}`)
		},
		"unknown parameter": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Parameters = []byte(`{"role":"head","unknown":true}`)
		},
		"role": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Parameters = []byte(`{"role":"unknown"}`)
		},
		"worker role": func(a *Adapter, _ *workload.CompileRequest) { a.Assignment.Rank = 1 },
		"carrier replicas": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Carrier.Parameters = []byte(`{"replicas":2}`)
		},
		"pod security": func(_ *Adapter, r *workload.CompileRequest) { r.Task.Configuration.Template.Spec.HostPID = true },
		"template label": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Labels = map[string]string{"ignored": "field"}
		},
		"container ports": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80}}
		},
		"GPU env": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"}}
		},
		"RLark env": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "RLARK_HEAD_ADDRESS", Value: "foreign"}}
		},
		"resource count": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}
		},
		"fraction GPU": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0.5")}
		},
		"overflow CPU": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("9223372036854775807")}
		},
		"differing requests": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
		},
		"run and argv": func(_ *Adapter, r *workload.CompileRequest) {
			r.Task.Configuration.Template.Spec.Containers[0].Command = []string{"ignored"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a, r := fixture(t, true)
			mutate(&a, &r)
			if _, err := a.Compile(context.Background(), r); !fault.Is(err, fault.InvalidArgument) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	a, r := fixture(t, false)
	r.Task.Configuration.Parameters = []byte(`{"role":"worker","run":{"inline":"echo forbidden"}}`)
	if _, err := a.Compile(context.Background(), r); err == nil {
		t.Fatal("worker training accepted")
	}
}
func TestPortPlanBoundaryAndLiteralArguments(t *testing.T) {
	for _, base := range []int32{1024, 65271} {
		p := PortPlan{Base: base, WorkerCount: 256}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		ports := p.TCP()
		if len(ports) != 265 || ports[len(ports)-1] > 65535 {
			t.Fatal(ports)
		}
	}
	for _, p := range []PortPlan{{Base: 1023, WorkerCount: 1}, {Base: 65535, WorkerCount: 1}, {Base: 20000}, {Base: 20000, WorkerCount: 257}, {Base: math.MaxInt32, WorkerCount: 1}} {
		if p.Validate() == nil || p.TCP() != nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	a, r := fixture(t, true)
	r.Task.Configuration.Parameters = []byte(`{"role":"head"}`)
	argv := []string{"python3", "space and ' quote", "$(touch /tmp/unwanted)", "`command`", "$HOME", ""}
	r.Task.Configuration.Template.Spec.Containers[0].Command = argv
	m := manifest(t, a, r)
	var c launchConfig
	if err := json.Unmarshal([]byte(m.Spec.Command[0]), &c); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Command, argv) {
		t.Fatalf("argv changed: %q", c.Command)
	}
}

func TestAdapterResourceLimitsAndMalformedScripts(t *testing.T) {
	a, r := fixture(t, true)
	r.Task.Configuration.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("20"), corev1.ResourceMemory: resource.MustParse("236Gi"), "nvidia.com/gpu": resource.MustParse("1"),
	}
	m := manifest(t, a, r)
	if m.Spec.CPU != 20000 || m.Spec.MemoryBytes != int64(236)<<30 || len(m.Spec.GPUUUIDs) != 1 {
		t.Fatalf("resources lost: %+v", m.Spec)
	}
	r.Task.Configuration.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("0.5")
	if _, err := a.Compile(context.Background(), r); err == nil {
		t.Fatal("fractional Ray CPU count accepted")
	}
	for _, params := range []string{`{"role":"head","prepare":{"inline":"bad\u0000script"},"run":{"inline":"train"}}`, `{"role":"head","run":{"inline":"bad\u0000script"}}`} {
		a, r := fixture(t, true)
		r.Task.Configuration.Parameters = []byte(params)
		if _, err := a.Compile(context.Background(), r); err == nil {
			t.Fatal("NUL script accepted")
		}
	}
	a, r = fixture(t, true)
	r.Task.Configuration.Parameters = []byte(`{"role":"head"}`)
	r.Task.Configuration.Template.Spec.Containers[0].Command = []string{"invalid\x00command"}
	if _, err := a.Compile(context.Background(), r); err == nil {
		t.Fatal("NUL argv accepted")
	}
}
