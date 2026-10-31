package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

var ContainerKind = rt.ObjectKind{Group: "docker.rlark.io", Version: "v1alpha1", Kind: "Container"}

const (
	RuntimeKind      rt.Kind = "Docker"
	DefaultID        rt.ID   = "docker"
	CarrierType              = "docker.rlark.io/container"
	CarrierVersion           = "v1"
	TaskLabel                = "rlark.io/task-uid"
	NamespaceLabel           = "rlark.io/task-namespace"
	NameLabel                = "rlark.io/task-name"
	RuntimeLabel             = "rlark.io/runtime"
	IncarnationLabel         = "rlark.io/task-incarnation"
)

type ContainerSpec struct {
	Image       string            `json:"image"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Env         []string          `json:"env,omitempty"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	GPUCount    int64             `json:"gpuCount,omitempty"`
	GPUUUIDs    []string          `json:"gpuUUIDs,omitempty"`
	CPU         int64             `json:"cpuMilli,omitempty"`
	MemoryBytes int64             `json:"memoryBytes,omitempty"`
	HostNetwork bool              `json:"hostNetwork"`
	RayPort     int32             `json:"rayPort,omitempty"`
	// RayPorts is internal compiled execution material. User task contracts
	// cannot set it; a future group scheduler must inject the exact plan.
	RayPorts []int32 `json:"rayPorts,omitempty"`
}

type Manifest struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       ContainerSpec `json:"spec"`
}

type Metadata struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func manifestFor(task rt.TaskIdentity, spec ContainerSpec) Manifest {
	return Manifest{APIVersion: ContainerKind.Group + "/" + ContainerKind.Version, Kind: ContainerKind.Kind, Metadata: Metadata{Namespace: task.Logical.Namespace, Name: task.Logical.Name, UID: task.ObjectUID}, Spec: spec}
}

func decodeManifest(data json.RawMessage) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("decode Docker manifest: %w", err)
	}
	if m.APIVersion != ContainerKind.Group+"/"+ContainerKind.Version || m.Kind != ContainerKind.Kind || m.Metadata.Namespace == "" || m.Metadata.Name == "" || m.Metadata.UID == "" {
		return m, errors.New("complete Docker container identity is required")
	}
	if err := m.Spec.Validate(); err != nil {
		return m, err
	}
	return m, nil
}

func (s ContainerSpec) Validate() error {
	if strings.TrimSpace(s.Image) == "" {
		return errors.New("Docker image is required")
	}
	if !s.HostNetwork {
		return errors.New("Docker workloads require host network mode")
	}
	if s.GPUCount < 0 || s.CPU < 0 || s.CPU > math.MaxInt64/1_000_000 || s.MemoryBytes < 0 {
		return errors.New("docker resource requests must be nonnegative and CPU must fit NanoCPUs")
	}
	if err := ValidateHostPortPlan(s.RayPorts); err != nil {
		return err
	}
	if len(s.GPUUUIDs) != 0 && s.GPUCount != 0 {
		return errors.New("docker GPU count and device UUIDs are mutually exclusive")
	}
	gpus := map[string]bool{}
	for _, id := range s.GPUUUIDs {
		if !strings.HasPrefix(id, "GPU-") || strings.ContainsAny(id, ", \t\r\n\x00") || gpus[id] {
			return errors.New("docker GPU UUIDs must be unique whole devices")
		}
		gpus[id] = true
	}
	for key := range s.Labels {
		if strings.HasPrefix(key, "rlark.io/") || strings.HasPrefix(key, "containers.rlark.io/") {
			return errors.New("docker ownership labels are reserved")
		}
	}
	for _, arg := range append(append(append([]string{s.Image, s.WorkingDir}, s.Entrypoint...), s.Command...), s.Env...) {
		if strings.ContainsRune(arg, 0) {
			return errors.New("NUL in Docker configuration")
		}
	}
	seen := map[string]struct{}{}
	for _, env := range s.Env {
		name, _, ok := strings.Cut(env, "=")
		if !ok || name == "" || strings.ContainsRune(env, 0) {
			return errors.New("Docker environment entries must be NAME=value")
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate Docker environment %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func objectFor(task rt.TaskIdentity, spec ContainerSpec) (rt.DesiredObject, error) {
	if err := task.Logical.Validate(); err != nil {
		return rt.DesiredObject{}, err
	}
	if task.ObjectUID == "" {
		return rt.DesiredObject{}, errors.New("Task UID is required")
	}
	if err := spec.Validate(); err != nil {
		return rt.DesiredObject{}, err
	}
	data, err := json.Marshal(manifestFor(task, spec))
	if err != nil {
		return rt.DesiredObject{}, err
	}
	return rt.DesiredObject{LogicalName: "container", Kind: ContainerKind, Manifest: rt.TypedManifest{Type: ContainerKind, Object: data}, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup, UpdateMode: rt.UpdateUnsupported}, nil
}

// DesiredContainer constructs the immutable single-container declaration used
// by Docker workload adapters.
func DesiredContainer(task rt.TaskIdentity, spec ContainerSpec) (rt.DesiredObject, error) {
	return objectFor(task, spec)
}

func containerName(task rt.TaskIdentity) string {
	uid := task.ObjectUID
	if len(uid) > 48 {
		uid = uid[:48]
	}
	return "rlark-task-" + uid
}

func labels(task rt.TaskIdentity) map[string]string {
	return map[string]string{TaskLabel: task.ObjectUID, NamespaceLabel: task.Logical.Namespace, NameLabel: task.Logical.Name, RuntimeLabel: string(RuntimeKind), IncarnationLabel: task.ObjectUID}
}
