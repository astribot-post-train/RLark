package taskcontract

import (
	"fmt"
	"strings"

	rawruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TaskSetRetiringCondition fences starts until the complete previous child set
// is observed absent. It remains meaningful across Job generations.
const TaskSetRetiringCondition = "TaskSetRetiring"

// ValidateRawJob validates the runtime-neutral Raw process contract shared by
// the reservation controller and the node Agent. Keeping this check at both
// boundaries prevents a task admitted by one component from being interpreted
// differently by the other after a restart or an API race.
func ValidateRawJob(job *native.Job) error {
	if job == nil || len(job.Spec.Tasks) == 0 || len(job.Spec.Tasks) > 16 || job.Spec.SSHPublicKey != "" {
		return fmt.Errorf("raw preview accepts one to sixteen independent process templates")
	}
	seen := make(map[string]struct{}, len(job.Spec.Tasks))
	for i := range job.Spec.Tasks {
		t := &job.Spec.Tasks[i]
		if t.Name == "" || len(validation.IsDNS1123Subdomain(utils.ChildName(job.Name, t.Name))) != 0 {
			return fmt.Errorf("raw process template %q has an invalid name", t.Name)
		}
		if _, ok := seen[t.Name]; ok {
			return fmt.Errorf("raw process template names must be unique")
		}
		seen[t.Name] = struct{}{}
		if t.Head || t.Suspended || t.Runtime.Kind != "Raw" || t.Runtime.Name != "" || t.Placement.Environment == "" || t.Placement.Binding != nil || t.Placement.DockerBinding != nil || t.DownstreamName != "" || len(t.Environments) != 0 || t.Workload.Template != nil || string(t.Workload.Kind) != string(rawruntime.ProcessWorkload) || t.Workload.Version != "v1" || t.Workload.Carrier.Type != string(rawruntime.ProcessWorkload) || t.Workload.Carrier.Version != "v1" || len(t.Workload.Carrier.Parameters.Raw) != 0 {
			return fmt.Errorf("raw process template %q uses an unsupported field", t.Name)
		}
		spec, err := rawruntime.DecodeProcess(t.Workload.Parameters.Raw)
		if err != nil {
			return fmt.Errorf("raw process template %q has invalid parameters: %w", t.Name, err)
		}
		if _, err := labels.ValidatedSelectorFromSet(t.Placement.NodeSelector); err != nil {
			return fmt.Errorf("raw process template %q has an invalid node selector: %w", t.Name, err)
		}
		for _, env := range spec.Env {
			name, _, _ := strings.Cut(env, "=")
			if name == "CUDA_VISIBLE_DEVICES" || name == "NVIDIA_VISIBLE_DEVICES" {
				return fmt.Errorf("GPU visibility is assigned by allocation")
			}
		}
	}
	return nil
}
