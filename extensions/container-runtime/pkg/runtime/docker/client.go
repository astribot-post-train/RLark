package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// EngineClient is the small Docker Engine surface used by the Runtime. Keeping
// it narrow makes lifecycle and ownership behavior testable without a daemon.
type EngineClient interface {
	ServerVersion(context.Context) (types.Version, error)
	Info(context.Context) (types.Info, error)
	ContainerInspect(context.Context, string) (types.ContainerJSON, error)
	ContainerList(context.Context, types.ContainerListOptions) ([]types.Container, error)
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *specs.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, types.ContainerStartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerKill(context.Context, string, string) error
	ContainerRemove(context.Context, string, types.ContainerRemoveOptions) error
	ContainerLogs(context.Context, string, types.ContainerLogsOptions) (io.ReadCloser, error)
	ContainerStatsOneShot(context.Context, string) (types.ContainerStats, error)
	ImageInspectWithRaw(context.Context, string) (types.ImageInspect, []byte, error)
	ImagePull(context.Context, string, types.ImagePullOptions) (io.ReadCloser, error)
}

// SDKClient is the concrete EngineClient adapter for the official Moby SDK.
// The SDK's platform parameter is represented by the OpenContainers type in
// the actual implementation; this alias keeps the interface fake-friendly.
type SDKClient struct{ *client.Client }

// NewSDKClient connects to the host Docker daemon. host may be a unix socket,
// npipe, or an Engine TCP endpoint; the default is the local root-owned socket.
func NewSDKClient(host string) (*client.Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	return client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
}
