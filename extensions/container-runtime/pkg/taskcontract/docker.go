package taskcontract

import (
	"errors"
	"strings"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateDockerJob is the fail-closed single-container preview shared by the
// scheduler and execution API. Ray groups and multi-template dispatch remain
// unsupported until their admission barrier and port contract are implemented.
func ValidateDockerJob(job *native.Job) error {
	if job == nil || len(job.Spec.Tasks) != 1 || job.Spec.SSHPublicKey != "" {
		return errors.New("docker preview requires exactly one container template")
	}
	t := &job.Spec.Tasks[0]
	if t.Name == "" || len(validation.IsDNS1123Subdomain(utils.ChildName(job.Name, t.Name))) != 0 || t.Head || t.Suspended {
		return errors.New("invalid Docker template")
	}
	return ValidateDockerTask(t.TaskSpec)
}

// ValidateDockerTask validates user fields before allocation GPU injection.
func ValidateDockerTask(t native.TaskSpec) error {
	if t.Runtime.Kind != "Docker" || t.Runtime.Name != "" || t.Placement.Environment == "" || t.Placement.Binding != nil || t.Placement.DockerBinding != nil || t.DownstreamName != "" || len(t.Environments) != 0 || t.Workload.Template != nil || string(t.Workload.Kind) != dockerruntime.CarrierType || t.Workload.Version != "v1" || t.Workload.Carrier.Type != dockerruntime.CarrierType || t.Workload.Carrier.Version != "v1" || len(t.Workload.Carrier.Parameters.Raw) != 0 {
		return errors.New("unsupported Docker template fields")
	}
	if _, err := labels.ValidatedSelectorFromSet(t.Placement.NodeSelector); err != nil {
		return err
	}
	spec, err := dockerruntime.DecodeContainer(t.Workload.Parameters.Raw)
	if err != nil {
		return err
	}
	if len(spec.GPUUUIDs) != 0 || spec.GPUCount != 0 || spec.RayPort != 0 {
		return errors.New("docker devices and ports are allocation-owned; Ray dispatch is unsupported")
	}
	for _, env := range spec.Env {
		name, _, _ := strings.Cut(env, "=")
		if name == "CUDA_VISIBLE_DEVICES" || name == "NVIDIA_VISIBLE_DEVICES" || strings.HasPrefix(name, "RLARK_") {
			return errors.New("docker GPU visibility and RLark variables are reserved")
		}
	}
	return nil
}

// ValidateJob selects only implemented, explicitly versioned task contracts.
func ValidateJob(job *native.Job) error {
	if job != nil && len(job.Spec.Tasks) != 0 && job.Spec.Tasks[0].Runtime.Kind == "Docker" {
		return ValidateDockerJob(job)
	}
	return ValidateRawJob(job)
}
