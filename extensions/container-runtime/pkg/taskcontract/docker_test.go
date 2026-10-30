package taskcontract

import (
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestDockerContractRejectsUnsupportedInput(t *testing.T) {
	valid := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job"}, Spec: native.JobSpec{Tasks: []native.JobTaskTemplate{{Name: "main", TaskSpec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Docker"}, Placement: native.TaskPlacementSpec{Environment: "host"}, Workload: native.TaskWorkloadSpec{Kind: "docker.rlark.io/container", Version: "v1", Carrier: native.TaskWorkloadCarrier{Type: "docker.rlark.io/container", Version: "v1"}, Parameters: extensions.JSON{Raw: []byte(`{"image":"image","hostNetwork":true}`)}}}}}}}
	if err := ValidateJob(valid); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{"image":"image","hostNetwork":true,"gpuUUIDs":["GPU-a"]}`, `{"image":"image","hostNetwork":true,"gpuCount":1}`, `{"image":"image","hostNetwork":true,"labels":{"rlark.io/task-uid":"foreign"}}`, `{"image":"image","hostNetwork":true,"env":["NVIDIA_VISIBLE_DEVICES=all"]}`, `{"image":"image","hostNetwork":true,"env":["RLARK_RAY_PORT=30000"]}`, `{"image":"image","hostNetwork":true,"rayPort":30000}`, `{"image":"image","hostNetwork":true,"privileged":true}`, `{"image":"image","hostNetwork":false}`, `{"image":"image","hostNetwork":true} {}`, `{"image":"image","hostNetwork":true,"cpuMilli":9223372036854775807}`} {
		t.Run(input, func(t *testing.T) {
			job := valid.DeepCopy()
			job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(input)
			if err := ValidateJob(job); err == nil {
				t.Fatal("unsupported contract accepted")
			}
		})
	}
	multi := valid.DeepCopy()
	multi.Spec.Tasks = append(multi.Spec.Tasks, *multi.Spec.Tasks[0].DeepCopy())
	multi.Spec.Tasks[1].Name = "second"
	if ValidateJob(multi) == nil {
		t.Fatal("multi-member group accepted")
	}
}
