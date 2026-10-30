package taskcontract

import (
	"fmt"
	"os"
	"testing"

	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"sigs.k8s.io/yaml"
)

func TestRawExampleAndTemplateContract(t *testing.T) {
	data, err := os.ReadFile("../../config/examples/raw-processes-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var example native.Job
	if err := yaml.UnmarshalStrict(data, &example); err != nil {
		t.Fatal(err)
	}
	if example.APIVersion != native.GroupVersion.String() || example.Kind != "Job" {
		t.Fatal("example has an unregistered native API")
	}
	if err := ValidateRawJob(&example); err != nil {
		t.Fatalf("example invalid: %v", err)
	}
	for _, mode := range []string{"nil", "empty", "limit", "over limit", "duplicate", "empty name", "invalid name", "missing environment", "bad selector", "unsupported sibling", "gpu env"} {
		t.Run(mode, func(t *testing.T) {
			j := example.DeepCopy()
			valid := false
			switch mode {
			case "nil":
				j = nil
			case "empty":
				j.Spec.Tasks = nil
			case "limit", "over limit":
				n := 16
				if mode == "over limit" {
					n++
				} else {
					valid = true
				}
				j.Spec.Tasks = nil
				for i := 0; i < n; i++ {
					task := *example.Spec.Tasks[0].DeepCopy()
					task.Name = fmt.Sprintf("worker-%d", i)
					j.Spec.Tasks = append(j.Spec.Tasks, task)
				}
			case "duplicate":
				j.Spec.Tasks[1].Name = j.Spec.Tasks[0].Name
			case "empty name":
				j.Spec.Tasks[0].Name = ""
			case "invalid name":
				j.Spec.Tasks[0].Name = "Invalid name"
			case "missing environment":
				j.Spec.Tasks[1].Placement.Environment = ""
			case "bad selector":
				j.Spec.Tasks[1].Placement.NodeSelector = map[string]string{"invalid label!": "bad"}
			case "unsupported sibling":
				j.Spec.Tasks[1].Head = true
			case "gpu env":
				j.Spec.Tasks[1].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3.12"],"env":["CUDA_VISIBLE_DEVICES=all"],"directory":"/workspace/training"}`)
			}
			if err := ValidateRawJob(j); (err == nil) != valid {
				t.Fatalf("valid=%v, got %v", valid, err)
			}
		})
	}
}
