package taskcontract

import (
	"encoding/json"
	"os"
	"testing"

	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func mixedContract(t *testing.T) *native.Job {
	t.Helper()
	data, err := os.ReadFile("../../config/examples/raw-processes-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var job native.Job
	if err := yaml.UnmarshalStrict(data, &job); err != nil {
		t.Fatal(err)
	}
	job.Spec.Tasks = job.Spec.Tasks[:1]
	job.Spec.Tasks = append(job.Spec.Tasks, native.JobTaskTemplate{Name: "env", TaskSpec: native.TaskSpec{
		Runtime: native.TaskRuntimeSpec{Kind: "Kubernetes"},
		Workload: native.TaskWorkloadSpec{Kind: taskapi.WorkloadTypeRay, Version: "v1",
			Parameters: apiextensions.JSON{Raw: []byte(`{"role":"worker","headAddress":"10.1.3.42","headTaskName":"head","totalNodes":3,"nodeRankStart":1}`)},
			Carrier:    native.TaskWorkloadCarrier{Type: taskapi.CarrierTypeKubernetesDeployment, Version: "v1"},
			Template:   &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "env", Image: "test.invalid/env:v1"}}}}},
	}})
	return &job
}

func TestMixedContractOneRayHeadAndExplicitWorkerSemantics(t *testing.T) {
	for _, mode := range []string{"valid", "same-head", "other-head", "other-head-name", "other-total", "duplicate-rank", "unknown-field", "trailing-json", "head", "ipv6", "multicast", "port", "replicas", "raw-gpu-override"} {
		t.Run(mode, func(t *testing.T) {
			job := mixedContract(t)
			worker := &job.Spec.Tasks[1]
			valid := mode == "valid" || mode == "same-head"
			switch mode {
			case "same-head", "other-head", "other-head-name", "other-total", "duplicate-rank":
				second := *worker.DeepCopy()
				second.Name = "env-two"
				var parameters taskapi.RayExecutionParametersV1
				if err := json.Unmarshal(second.Workload.Parameters.Raw, &parameters); err != nil {
					t.Fatal(err)
				}
				parameters.NodeRankStart = 2
				switch mode {
				case "other-head":
					parameters.HeadAddress = "10.1.3.43"
				case "other-head-name":
					parameters.HeadTaskName = "other"
				case "other-total":
					parameters.TotalNodes = 4
				case "duplicate-rank":
					parameters.NodeRankStart = 1
				}
				data, err := json.Marshal(parameters)
				if err != nil {
					t.Fatal(err)
				}
				second.Workload.Parameters.Raw = data
				job.Spec.Tasks = append(job.Spec.Tasks, second)
			case "unknown-field":
				worker.Workload.Parameters.Raw = []byte(`{"role":"worker","unknown":1}`)
			case "trailing-json":
				worker.Workload.Parameters.Raw = append(worker.Workload.Parameters.Raw, []byte(` {}`)...)
			case "head":
				worker.Head = true
			case "ipv6", "multicast", "port":
				address := map[string]string{"ipv6": "::1", "multicast": "224.0.0.1", "port": "10.1.3.42:6379"}[mode]
				worker.Workload.Parameters.Raw = []byte(`{"role":"worker","headTaskName":"head","headAddress":"` + address + `","totalNodes":3}`)
			case "replicas":
				worker.Workload.Carrier.Parameters.Raw = []byte(`{"replicas":2}`)
			case "raw-gpu-override":
				job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3"],"env":["CUDA_VISIBLE_DEVICES=all"],"directory":"/work"}`)
			}
			if err := ValidateJob(job); (err == nil) != valid {
				t.Fatalf("valid=%v: %v", valid, err)
			}
		})
	}
}
