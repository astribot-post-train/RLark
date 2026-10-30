package docker

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type observerEngine struct {
	info     types.Info
	infoErr  error
	version  types.Version
	gpuCount int64
	gpuErr   error
}

func (e observerEngine) ServerVersion(context.Context) (types.Version, error) { return e.version, nil }
func (e observerEngine) Info(context.Context) (types.Info, error)             { return e.info, e.infoErr }
func (e observerEngine) ContainerInspect(context.Context, string) (types.ContainerJSON, error) {
	return types.ContainerJSON{}, errors.New("unused")
}
func (e observerEngine) ContainerList(context.Context, types.ContainerListOptions) ([]types.Container, error) {
	return nil, errors.New("unused")
}
func (e observerEngine) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *specs.Platform, string) (container.CreateResponse, error) {
	return container.CreateResponse{}, errors.New("unused")
}
func (e observerEngine) ContainerStart(context.Context, string, types.ContainerStartOptions) error {
	return errors.New("unused")
}
func (e observerEngine) ContainerStop(context.Context, string, container.StopOptions) error {
	return errors.New("unused")
}
func (e observerEngine) ContainerKill(context.Context, string, string) error {
	return errors.New("unused")
}
func (e observerEngine) ContainerRemove(context.Context, string, types.ContainerRemoveOptions) error {
	return errors.New("unused")
}
func (e observerEngine) ContainerLogs(context.Context, string, types.ContainerLogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (e observerEngine) ContainerStatsOneShot(context.Context, string) (types.ContainerStats, error) {
	return types.ContainerStats{}, errors.New("unused")
}
func (e observerEngine) ImageInspectWithRaw(context.Context, string) (types.ImageInspect, []byte, error) {
	return types.ImageInspect{}, nil, errors.New("unused")
}
func (e observerEngine) ImagePull(context.Context, string, types.ImagePullOptions) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func TestNodeObserverPreservesReservationsAndProjectsCapacity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &api.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-1", Namespace: "agents", UID: "node-uid", Labels: map[string]string{api.LabelAgentType: string(api.AgentTypeDocker)}},
		Spec:       api.NodeSpec{AgentType: api.AgentTypeDocker},
		Status:     api.NodeStatus{DockerReservations: []api.DockerNodeReservation{{TaskUID: "task-1", CPUMilli: 500, GPUCount: 1}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(node).Build()
	o := &NodeObserver{Client: c, Engine: observerEngine{info: types.Info{ID: "engine-1", Name: "host-1", NCPU: 8, MemTotal: 16 << 30}, version: types.Version{Version: "24.0"}}, Node: client.ObjectKeyFromObject(node), NodeUID: "node-uid", NodeIP: "10.0.0.7", GPUDiscoverer: func(context.Context) (int64, error) { return 2, nil }}
	if err := o.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	var current api.Node
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.DockerReservations) != 1 || current.Status.DockerReservations[0].TaskUID != "task-1" {
		t.Fatalf("reservations were overwritten: %#v", current.Status.DockerReservations)
	}
	if current.Status.Phase != api.NodeOnline || current.Status.NodeInfo.Incarnation != "engine-1" {
		t.Fatalf("unexpected status: %#v", current.Status)
	}
	cpu := current.Status.Capacity[corev1.ResourceCPU]
	if got := cpu.Value(); got != 8 {
		t.Fatalf("cpu capacity = %d", got)
	}
	gpu := current.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
	if got := gpu.Value(); got != 2 {
		t.Fatalf("gpu capacity = %d", got)
	}
	if len(current.Status.Addresses) != 1 || current.Status.Addresses[0].Address != "10.0.0.7" {
		t.Fatalf("unexpected addresses: %#v", current.Status.Addresses)
	}
}

func TestNodeObserverReturnsDockerOutageAndToleratesGPUDiscoveryFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "docker-1", Namespace: "agents", UID: "node-uid", Labels: map[string]string{api.LabelAgentType: string(api.AgentTypeDocker)}}, Spec: api.NodeSpec{AgentType: api.AgentTypeDocker}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(node).Build()
	outage := &NodeObserver{Client: c, Engine: observerEngine{infoErr: errors.New("daemon down")}, Node: client.ObjectKeyFromObject(node), NodeUID: "node-uid"}
	if err := outage.observe(context.Background()); err == nil {
		t.Fatal("expected Docker outage")
	}
	degraded := &NodeObserver{Client: c, Engine: observerEngine{info: types.Info{ID: "engine-1", NCPU: 1, MemTotal: 1}, version: types.Version{Version: "24.0"}}, Node: client.ObjectKeyFromObject(node), NodeUID: "node-uid", GPUDiscoverer: func(context.Context) (int64, error) { return 0, errors.New("nvidia unavailable") }}
	if err := degraded.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
}
