package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/errdefs"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// Provider owns only containers whose full Task identity is present in labels.
// It never shells out to Docker and never exposes the daemon socket to a task.
type Provider struct {
	ID        rt.ID
	Client    EngineClient
	Admission interface {
		Authorize(context.Context, rt.TaskIdentity) error
	}
}

var _ rt.Provider = (*Provider)(nil)
var _ rt.InventoryProvider = (*Provider)(nil)

func (p *Provider) Info(ctx context.Context) (rt.Info, error) {
	if p == nil || p.Client == nil {
		return rt.Info{}, fault.New(fault.FailedPrecondition, "Docker Engine client is required")
	}
	if _, err := p.Client.ServerVersion(ctx); err != nil {
		return rt.Info{}, fault.Wrap(fault.Unavailable, "connect to Docker Engine", err)
	}
	id := p.ID
	if id == "" {
		id = DefaultID
	}
	return rt.Info{ID: id, Kind: RuntimeKind, Capabilities: []rt.Capability{rt.CapabilityCandidateInventory, rt.CapabilityWorkloadInventory}}, nil
}

func (p *Provider) ValidateDesired(d rt.DesiredObject) error {
	if d.Kind != ContainerKind || d.Manifest.Type != ContainerKind || d.Ownership.Scope != rt.IdentityTaskIncarnation || d.Ownership.OwnerKey == "" || d.Lifecycle != rt.LifecycleDeleteOnTaskCleanup || d.UpdateMode != rt.UpdateUnsupported || len(d.DependsOn) != 0 || len(d.Units) != 0 || len(d.OwnedFields) != 0 {
		return fault.New(fault.InvalidArgument, "unsupported Docker container contract")
	}
	m, err := decodeManifest(d.Manifest.Object)
	if err != nil {
		return fault.Wrap(fault.InvalidArgument, "validate Docker manifest", err)
	}
	if d.Ownership.OwnerKey != m.Metadata.UID {
		return fault.New(fault.PermissionDenied, "Docker manifest ownership conflicts with Task UID")
	}
	return nil
}

func (p *Provider) Lower(ctx context.Context, request rt.UnitLoweringRequest) (rt.DesiredObject, error) {
	if err := ctx.Err(); err != nil {
		return rt.DesiredObject{}, err
	}
	if request.Runtime.Kind != RuntimeKind || request.Runtime.ID != p.runtimeID() {
		return rt.DesiredObject{}, fault.New(fault.InvalidArgument, "incorrect Docker Runtime selection")
	}
	if err := p.ValidateDesired(request.Object); err != nil {
		return rt.DesiredObject{}, err
	}
	return request.Object, nil
}

func (p *Provider) runtimeID() rt.ID {
	if p.ID == "" {
		return DefaultID
	}
	return p.ID
}

func (p *Provider) ref(task rt.TaskIdentity, id, name string) rt.ObjectRef {
	return rt.ObjectRef{RuntimeID: p.runtimeID(), ID: id, UID: id, Namespace: task.Logical.Namespace, Name: name, LogicalTask: task.Logical}
}

func validateTask(task rt.TaskIdentity) error {
	if err := task.Logical.Validate(); err != nil {
		return err
	}
	if task.ObjectUID == "" {
		return errors.New("Task UID is required")
	}
	return nil
}

func (p *Provider) inspectOwned(ctx context.Context, task rt.TaskIdentity, id string) (types.ContainerJSON, error) {
	if err := validateTask(task); err != nil {
		return types.ContainerJSON{}, fault.Wrap(fault.InvalidArgument, "validate Docker Task identity", err)
	}
	c, err := p.Client.ContainerInspect(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return types.ContainerJSON{}, fault.Wrap(fault.NotFound, "Docker container not found", err)
		}
		return types.ContainerJSON{}, fault.Wrap(fault.Unavailable, "inspect Docker container", err)
	}
	if !owned(c, task) {
		return types.ContainerJSON{}, fault.New(fault.Conflict, "Docker container ownership conflict")
	}
	return c, nil
}

func owned(c types.ContainerJSON, task rt.TaskIdentity) bool {
	if c.Config == nil {
		return false
	}
	l := c.Config.Labels
	return l[TaskLabel] == task.ObjectUID && l[NamespaceLabel] == task.Logical.Namespace && l[NameLabel] == task.Logical.Name && l[RuntimeLabel] == string(RuntimeKind) && l[IncarnationLabel] == task.ObjectUID
}

func (p *Provider) object(task rt.TaskIdentity, c types.ContainerJSON) (rt.Object, error) {
	if !owned(c, task) {
		return rt.Object{}, fault.New(fault.Conflict, "Docker container ownership conflict")
	}
	spec := ContainerSpec{HostNetwork: c.HostConfig != nil && c.HostConfig.NetworkMode.IsHost()}
	if c.Config != nil {
		spec.Image, spec.Env, spec.WorkingDir = c.Config.Image, append([]string(nil), c.Config.Env...), c.Config.WorkingDir
		spec.Entrypoint = append([]string(nil), c.Config.Entrypoint...)
		spec.Command = append([]string(nil), c.Config.Cmd...)
	}
	if c.HostConfig != nil {
		spec.CPU = c.HostConfig.NanoCPUs / 1_000_000
		spec.MemoryBytes = c.HostConfig.Memory
		for _, request := range c.HostConfig.DeviceRequests {
			spec.GPUCount += int64(request.Count)
		}
	}
	for _, value := range spec.Env {
		if port, ok := strings.CutPrefix(value, "RLARK_RAY_PORT="); ok {
			parsed, err := strconv.ParseInt(port, 10, 32)
			if err != nil {
				return rt.Object{}, err
			}
			spec.RayPort = int32(parsed)
		}
	}
	data, err := json.Marshal(manifestFor(task, spec))
	if err != nil {
		return rt.Object{}, err
	}
	name := task.Logical.Name
	return rt.Object{Ref: p.ref(task, c.ID, name), Manifest: rt.TypedManifest{Type: ContainerKind, Object: data}}, nil
}

func (p *Provider) Get(ctx context.Context, ref rt.ObjectRef) (rt.Object, error) {
	if ref.RuntimeID != p.runtimeID() || ref.ID == "" || ref.UID != ref.ID || ref.LogicalTask.Validate() != nil {
		return rt.Object{}, fault.New(fault.InvalidArgument, "invalid Docker object reference")
	}
	c, err := p.Client.ContainerInspect(ctx, ref.ID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return rt.Object{}, fault.Wrap(fault.NotFound, "Docker container not found", err)
		}
		return rt.Object{}, fault.Wrap(fault.Unavailable, "inspect Docker container", err)
	}
	if c.Config == nil || c.Config.Labels[NamespaceLabel] != ref.LogicalTask.Namespace || c.Config.Labels[NameLabel] != ref.LogicalTask.Name {
		return rt.Object{}, fault.New(fault.PermissionDenied, "Docker object reference is outside Task ownership")
	}
	task := rt.TaskIdentity{Logical: ref.LogicalTask, ObjectUID: c.Config.Labels[TaskLabel]}
	if err := validateTask(task); err != nil {
		return rt.Object{}, fault.Wrap(fault.Conflict, "Docker container has incomplete Task identity", err)
	}
	if !owned(c, task) {
		return rt.Object{}, fault.New(fault.Conflict, "Docker container ownership conflict")
	}
	return p.object(task, c)
}

func (p *Provider) ListManaged(ctx context.Context, task rt.TaskIdentity) (rt.ObjectSnapshot, error) {
	if err := validateTask(task); err != nil {
		return rt.ObjectSnapshot{}, err
	}
	cs, err := p.Client.ContainerList(ctx, types.ContainerListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", TaskLabel+"="+task.ObjectUID), filters.Arg("label", RuntimeLabel+"="+string(RuntimeKind)))})
	if err != nil {
		return rt.ObjectSnapshot{}, fault.Wrap(fault.Unavailable, "list Docker containers", err)
	}
	objects := make([]rt.Object, 0, len(cs))
	for _, summary := range cs {
		c, err := p.inspectOwned(ctx, task, summary.ID)
		if err != nil {
			return rt.ObjectSnapshot{}, err
		}
		o, err := p.object(task, c)
		if err != nil {
			return rt.ObjectSnapshot{}, err
		}
		objects = append(objects, o)
	}
	if len(objects) > 1 {
		return rt.ObjectSnapshot{}, fault.New(fault.Conflict, "multiple Docker containers claim one Task")
	}
	return rt.ObjectSnapshot{Revision: fmt.Sprintf("%d", len(objects)), Objects: objects}, nil
}

func (p *Provider) ListCandidates(ctx context.Context, q rt.ManagedObjectQuery) (rt.ObjectSnapshot, error) {
	if err := validateTask(q.Task); err != nil {
		return rt.ObjectSnapshot{}, err
	}
	for _, candidate := range q.Candidates {
		if candidate.Kind != ContainerKind || candidate.Namespace != q.Task.Logical.Namespace || candidate.Name != q.Task.Logical.Name || candidate.Ownership.Scope != rt.IdentityTaskIncarnation || candidate.Ownership.OwnerKey != q.Task.ObjectUID || candidate.Lifecycle != rt.LifecycleDeleteOnTaskCleanup {
			return rt.ObjectSnapshot{}, fault.New(fault.PermissionDenied, "Docker candidate outside Task ownership")
		}
	}
	return p.ListManaged(ctx, q.Task)
}

func (p *Provider) Apply(ctx context.Context, req rt.ApplyRequest) (rt.ApplyResult, error) {
	if _, err := p.Info(ctx); err != nil {
		return rt.ApplyResult{}, err
	}
	if err := validateTask(req.Task); err != nil {
		return rt.ApplyResult{}, err
	}
	if err := p.ValidateDesired(req.Desired); err != nil {
		return rt.ApplyResult{}, err
	}
	m, err := decodeManifest(req.Desired.Manifest.Object)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	if m.Metadata.UID != req.Task.ObjectUID || m.Metadata.Namespace != req.Task.Logical.Namespace || m.Metadata.Name != req.Task.Logical.Name {
		return rt.ApplyResult{}, fault.New(fault.PermissionDenied, "Docker manifest is outside Task ownership")
	}
	if req.Current != nil && (req.Current.RuntimeID != p.runtimeID() || req.Current.LogicalTask != req.Task.Logical) {
		return rt.ApplyResult{}, fault.New(fault.PermissionDenied, "Docker current reference is outside Task ownership")
	}
	if p.Admission == nil {
		return rt.ApplyResult{}, fault.New(fault.FailedPrecondition, "Docker node admission is required")
	}
	if err := p.Admission.Authorize(ctx, req.Task); err != nil {
		return rt.ApplyResult{}, err
	}
	name := containerName(req.Task)
	c, inspectErr := p.Client.ContainerInspect(ctx, name)
	if inspectErr == nil {
		if !owned(c, req.Task) {
			return rt.ApplyResult{}, fault.New(fault.Conflict, "existing Docker name is owned by another Task")
		}
		observed, err := p.object(req.Task, c)
		if err != nil {
			return rt.ApplyResult{}, err
		}
		equal, err := p.Equal(observed.Manifest, req.Desired)
		if err != nil {
			return rt.ApplyResult{}, err
		}
		if !equal {
			return rt.ApplyResult{}, fault.New(fault.Conflict, "Docker container configuration is immutable")
		}
		if c.State != nil && c.State.Status == "created" {
			if err := p.Client.ContainerStart(ctx, c.ID, types.ContainerStartOptions{}); err != nil {
				return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "start Docker container", err)
			}
			c, err = p.Client.ContainerInspect(ctx, c.ID)
			if err != nil {
				return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "reinspect Docker container", err)
			}
		}
		o, err := p.object(req.Task, c)
		if err != nil {
			return rt.ApplyResult{}, err
		}
		return rt.ApplyResult{Ref: o.Ref, Object: o}, nil
	}
	if !errdefs.IsNotFound(inspectErr) {
		return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "inspect Docker container by stable name", inspectErr)
	}
	if _, _, err := p.Client.ImageInspectWithRaw(ctx, m.Spec.Image); err != nil {
		if !errdefs.IsNotFound(err) {
			return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "inspect Docker image", err)
		}
		reader, pullErr := p.Client.ImagePull(ctx, m.Spec.Image, types.ImagePullOptions{})
		if pullErr != nil {
			return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "pull Docker image", pullErr)
		}
		if reader != nil {
			_, _ = io.Copy(io.Discard, reader)
			_ = reader.Close()
		}
	}
	cfg, host := dockerConfig(m.Spec, req.Task)
	created, err := p.Client.ContainerCreate(ctx, cfg, host, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "create Docker container", err)
	}
	if created.ID == "" {
		return rt.ApplyResult{}, fault.New(fault.Internal, "Docker returned an empty container ID")
	}
	if err := p.Client.ContainerStart(ctx, created.ID, types.ContainerStartOptions{}); err != nil {
		return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "start Docker container", err)
	}
	c, err = p.Client.ContainerInspect(ctx, created.ID)
	if err != nil {
		return rt.ApplyResult{}, fault.Wrap(fault.Unavailable, "inspect created Docker container", err)
	}
	o, err := p.object(req.Task, c)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	return rt.ApplyResult{Ref: o.Ref, Object: o}, nil
}

func dockerConfig(spec ContainerSpec, task rt.TaskIdentity) (*container.Config, *container.HostConfig) {
	labels := labels(task)
	for k, v := range spec.Labels {
		labels[k] = v
	}
	cfg := &container.Config{Image: spec.Image, Entrypoint: strslice.StrSlice(spec.Entrypoint), Cmd: strslice.StrSlice(spec.Command), Env: append([]string(nil), spec.Env...), WorkingDir: spec.WorkingDir, Labels: labels}
	host := &container.HostConfig{NetworkMode: container.NetworkMode("host"), Resources: container.Resources{NanoCPUs: spec.CPU * 1_000_000, Memory: spec.MemoryBytes}}
	if spec.GPUCount > 0 {
		host.DeviceRequests = []container.DeviceRequest{{Count: int(spec.GPUCount), Capabilities: [][]string{{"gpu"}}}}
	}
	return cfg, host
}

func (p *Provider) Delete(ctx context.Context, req rt.DeleteRequest) error {
	if req.Ref.RuntimeID != p.runtimeID() || req.Ref.LogicalTask != req.Task.Logical || req.Ref.ID == "" || req.Ref.UID != req.Ref.ID {
		return fault.New(fault.PermissionDenied, "Docker cleanup reference is outside Task ownership")
	}
	c, err := p.inspectOwned(ctx, req.Task, req.Ref.ID)
	if fault.Is(err, fault.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.State != nil && c.State.Running {
		if err := p.Client.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil && !errdefs.IsNotFound(err) {
			return fault.Wrap(fault.Unavailable, "stop Docker container", err)
		}
	}
	if err := p.Client.ContainerRemove(ctx, c.ID, types.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fault.Wrap(fault.Unavailable, "remove Docker container", err)
	}
	return nil
}

func (p *Provider) ListInstances(ctx context.Context, q rt.InstanceQuery) (rt.InstanceSnapshot, error) {
	if len(q.Workloads) == 0 {
		return rt.InstanceSnapshot{}, nil
	}
	if len(q.Workloads) != 1 || q.Workloads[0].RuntimeID != p.runtimeID() || q.Workloads[0].LogicalTask != q.Task.Logical || q.Workloads[0].UID != q.Workloads[0].ID {
		return rt.InstanceSnapshot{}, fault.New(fault.PermissionDenied, "Docker instance query outside Task ownership")
	}
	c, err := p.inspectOwned(ctx, q.Task, q.Workloads[0].ID)
	if fault.Is(err, fault.NotFound) {
		return rt.InstanceSnapshot{}, nil
	}
	if err != nil {
		return rt.InstanceSnapshot{}, err
	}
	phase, reason, message := phase(c)
	return rt.InstanceSnapshot{Revision: string(phase), Instances: []rt.Instance{{Ref: rt.InstanceRef{RuntimeID: p.runtimeID(), ID: c.ID, UID: c.ID, Parent: q.Workloads[0]}, Task: q.Task, Name: strings.TrimPrefix(c.Name, "/"), Phase: phase, Reason: reason, Message: message}}}, nil
}

func phase(c types.ContainerJSON) (rt.InstancePhase, string, string) {
	if c.State == nil {
		return "Unknown", "StateMissing", "Docker did not return container state"
	}
	switch c.State.Status {
	case "created", "restarting":
		return "Pending", c.State.Status, c.State.Error
	case "running":
		return "Running", "Running", ""
	case "paused":
		return "Running", "Paused", "container is paused"
	case "exited", "dead":
		if c.State.ExitCode == 0 {
			return "Succeeded", "Exited", ""
		}
		return "Failed", "ExitCode", c.State.Error
	default:
		return "Unknown", c.State.Status, c.State.Error
	}
}

// Equal compares immutable container configuration while allowing environment
// defaults supplied by the image. It never treats exit state as desired drift.
func (p *Provider) Equal(current rt.TypedManifest, desired rt.DesiredObject) (bool, error) {
	actual, err := decodeManifest(current.Object)
	if err != nil {
		return false, err
	}
	wanted, err := decodeManifest(desired.Manifest.Object)
	if err != nil {
		return false, err
	}
	if actual.Metadata != wanted.Metadata {
		return false, nil
	}
	actualEnv := make(map[string]string)
	for _, entry := range actual.Spec.Env {
		key, value, _ := strings.Cut(entry, "=")
		actualEnv[key] = value
	}
	for _, entry := range wanted.Spec.Env {
		key, value, _ := strings.Cut(entry, "=")
		if got, ok := actualEnv[key]; !ok || got != value {
			return false, nil
		}
	}
	actual.Spec.Env, wanted.Spec.Env = nil, nil
	// An omitted working directory inherits the image's WORKDIR. Docker's
	// inspection returns that resolved default, so it is not desired drift.
	if wanted.Spec.WorkingDir == "" {
		actual.Spec.WorkingDir = ""
	}
	return reflect.DeepEqual(actual.Spec, wanted.Spec), nil
}
