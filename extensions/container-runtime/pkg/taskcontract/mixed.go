package taskcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"

	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateMixedJob accepts Raw processes and Kubernetes Ray workers in one
// externally owned Job. It grants no placement or connectivity authority.
// In particular it never creates a second Ray head.
func ValidateMixedJob(job *native.Job) error {
	if job == nil || len(job.Spec.Tasks) < 2 || len(job.Spec.Tasks) > 16 || job.Spec.SSHPublicKey != "" {
		return fmt.Errorf("mixed preview requires two to sixteen templates without a Job SSH key")
	}
	seen := map[string]bool{}
	var headAddress, headTaskName string
	var totalNodes int32
	ranks := map[int32]bool{}
	raw, kubernetes := false, false
	for _, t := range job.Spec.Tasks {
		if t.Name == "" || seen[t.Name] || len(validation.IsDNS1123Subdomain(utils.ChildName(job.Name, t.Name))) != 0 || t.Head || t.Suspended {
			return fmt.Errorf("invalid or duplicate mixed template %q", t.Name)
		}
		seen[t.Name] = true
		switch t.Runtime.Kind {
		case "Raw":
			one := job.DeepCopy()
			one.Spec.Tasks = []native.JobTaskTemplate{t}
			if err := ValidateRawJob(one); err != nil {
				return err
			}
			raw = true
		case "Kubernetes":
			if err := validateKubernetesWorker(t.TaskSpec); err != nil {
				return fmt.Errorf("Kubernetes template %q: %w", t.Name, err)
			}
			var ray taskapi.RayExecutionParametersV1
			if err := decodeStrict(t.Workload.Parameters.Raw, &ray); err != nil {
				return fmt.Errorf("Kubernetes template %q: %w", t.Name, err)
			}
			if headAddress == "" {
				headAddress, headTaskName, totalNodes = ray.HeadAddress, ray.HeadTaskName, ray.TotalNodes
			} else if ray.HeadAddress != headAddress || ray.HeadTaskName != headTaskName || ray.TotalNodes != totalNodes {
				return fmt.Errorf("Kubernetes templates must use one Ray head and total node count")
			}
			if ranks[ray.NodeRankStart] {
				return fmt.Errorf("Kubernetes templates reuse Ray node rank %d", ray.NodeRankStart)
			}
			ranks[ray.NodeRankStart] = true
			kubernetes = true
		default:
			return fmt.Errorf("unsupported mixed runtime %q", t.Runtime.Kind)
		}
	}
	if !raw || !kubernetes {
		return fmt.Errorf("mixed preview requires both Raw and Kubernetes templates")
	}
	if int32(len(ranks)) > totalNodes {
		return fmt.Errorf("Kubernetes worker count exceeds Ray total node count")
	}
	return nil
}

func validateKubernetesWorker(t native.TaskSpec) error {
	if t.Runtime.Name != "" || t.Placement.Binding != nil || t.Placement.DockerBinding != nil || t.Placement.Environment != "" || t.DownstreamName != "" || len(t.Environments) != 0 || t.Workload.Template == nil || len(t.Workload.Template.Spec.Containers) == 0 || t.Workload.Kind != taskapi.WorkloadTypeRay || t.Workload.Version != "v1" || t.Workload.Carrier.Type != taskapi.CarrierTypeKubernetesDeployment || t.Workload.Carrier.Version != "v1" {
		return fmt.Errorf("requires a native Ray Deployment worker Pod template without runtime bindings or environments")
	}
	if _, err := labels.ValidatedSelectorFromSet(t.Placement.NodeSelector); err != nil {
		return err
	}
	var ray taskapi.RayExecutionParametersV1
	if err := decodeStrict(t.Workload.Parameters.Raw, &ray); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(ray.HeadAddress)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ray.Role != taskapi.RayRoleWorker || ray.HeadTaskName == "" || ray.TotalNodes < 1 || ray.NodeRankStart < 0 {
		return fmt.Errorf("requires worker role and explicit unicast IPv4 Ray head address and identity")
	}
	if len(t.Workload.Carrier.Parameters.Raw) != 0 {
		var carrier taskapi.KubernetesReplicatedCarrierParametersV1
		if err := decodeStrict(t.Workload.Carrier.Parameters.Raw, &carrier); err != nil {
			return err
		}
		if carrier.Replicas != nil && *carrier.Replicas != 1 {
			return fmt.Errorf("mixed preview requires one replica per named template")
		}
	}
	return nil
}

func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("decode mixed workload parameters: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("mixed workload parameters contain trailing JSON")
	}
	return nil
}
