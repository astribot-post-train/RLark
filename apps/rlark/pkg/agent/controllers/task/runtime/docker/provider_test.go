package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

type admissionFunc func(context.Context, rt.TaskIdentity) error

func (f admissionFunc) Authorize(ctx context.Context, id rt.TaskIdentity) error { return f(ctx, id) }

type lifecycleEngine struct {
	observerEngine
	current                         *types.ContainerJSON
	starts, creates, stops, removes int
	removeErr                       error
}

func (e *lifecycleEngine) ContainerInspect(context.Context, string) (types.ContainerJSON, error) {
	if e.current == nil {
		return types.ContainerJSON{}, errdefs.NotFound(errors.New("absent"))
	}
	return *e.current, nil
}
func (e *lifecycleEngine) ImageInspectWithRaw(context.Context, string) (types.ImageInspect, []byte, error) {
	return types.ImageInspect{}, nil, nil
}
func (e *lifecycleEngine) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *specs.Platform, name string) (container.CreateResponse, error) {
	e.creates++
	e.current = &types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: "container-id", Name: "/" + name, State: &types.ContainerState{Status: "created"}, HostConfig: host}, Config: cfg}
	return container.CreateResponse{ID: "container-id"}, nil
}
func (e *lifecycleEngine) ContainerStart(context.Context, string, types.ContainerStartOptions) error {
	e.starts++
	e.current.State.Status = "running"
	e.current.State.Running = true
	return nil
}
func (e *lifecycleEngine) ContainerStop(context.Context, string, container.StopOptions) error {
	e.stops++
	e.current.State.Status = "exited"
	e.current.State.Running = false
	return nil
}
func (e *lifecycleEngine) ContainerRemove(context.Context, string, types.ContainerRemoveOptions) error {
	e.removes++
	if e.removeErr != nil {
		return e.removeErr
	}
	e.current = nil
	return nil
}

func providerFixture(t *testing.T) (*Provider, *lifecycleEngine, rt.ApplyRequest) {
	t.Helper()
	e := &lifecycleEngine{}
	p := &Provider{Client: e, Admission: admissionFunc(func(context.Context, rt.TaskIdentity) error { return nil })}
	task := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "head"}, ObjectUID: "task-uid", Generation: 1}
	desired, err := DesiredContainer(task, ContainerSpec{Image: "busybox:latest", Entrypoint: []string{"/bin/sh"}, Command: []string{"-c", "sleep 100"}, HostNetwork: true, CPU: 1000, MemoryBytes: 1 << 28})
	if err != nil {
		t.Fatal(err)
	}
	return p, e, rt.ApplyRequest{Task: task, Desired: desired}
}

func TestProviderRecoversCreationObservesExitAndRetriesCleanup(t *testing.T) {
	p, e, req := providerFixture(t)
	ctx := context.Background()
	first, err := p.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref.UID != "container-id" || first.Ref.Name != req.Task.Logical.Name {
		t.Fatalf("invalid ref: %#v", first.Ref)
	}
	// A separate Provider recovers a create whose response was lost.
	e.current.Config.WorkingDir = "/image-default"
	recovered := *p
	second, err := recovered.Apply(ctx, req)
	if err != nil || second.Ref != first.Ref || e.creates != 1 || e.starts != 1 {
		t.Fatalf("recovery result=%#v creates=%d starts=%d err=%v", second, e.creates, e.starts, err)
	}
	instances, err := p.ListInstances(ctx, rt.InstanceQuery{Task: req.Task, Workloads: []rt.ObjectRef{first.Ref}})
	if err != nil || len(instances.Instances) != 1 || instances.Instances[0].Phase != "Running" {
		t.Fatalf("instances=%#v err=%v", instances, err)
	}
	e.current.State.Status = "exited"
	e.current.State.Running = false
	if _, err := p.Apply(ctx, req); err != nil {
		t.Fatal(err)
	}
	if e.starts != 1 {
		t.Fatal("terminal execution was restarted")
	}
	e.removeErr = errors.New("daemon unavailable")
	deletion := rt.DeleteRequest{Task: req.Task, Ref: first.Ref}
	if err := p.Delete(ctx, deletion); err == nil {
		t.Fatal("cleanup failure was lost")
	}
	e.removeErr = nil
	if err := p.Delete(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, deletion); err != nil {
		t.Fatal("absence is successful cleanup", err)
	}
}

func TestProviderRejectsForeignOwnershipImmutableDriftAndMissingAdmission(t *testing.T) {
	p, e, req := providerFixture(t)
	ctx := context.Background()
	applied, err := p.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	e.current.Config.Labels[TaskLabel] = "foreign"
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("ownership err=%v", err)
	}
	if err := p.Delete(ctx, rt.DeleteRequest{Task: req.Task, Ref: applied.Ref}); !fault.Is(err, fault.Conflict) {
		t.Fatalf("delete err=%v", err)
	}
	if e.removes != 0 {
		t.Fatal("foreign container deleted")
	}
	e.current.Config.Labels[TaskLabel] = req.Task.ObjectUID
	e.current.Config.Cmd = []string{"changed"}
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("immutable err=%v", err)
	}
	e.current = nil
	p.Admission = nil
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("missing admission err=%v", err)
	}
	if e.creates != 1 {
		t.Fatal("container created without admission")
	}
}
