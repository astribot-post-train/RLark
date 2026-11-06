//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/system"
)

// DockerObservationEngine is the read-only daemon surface used for heartbeats.
type DockerObservationEngine interface {
	Info(context.Context) (system.Info, error)
	ServerVersion(context.Context) (types.Version, error)
	ContainerList(context.Context, container.ListOptions) ([]types.Container, error)
}

// DockerObserver reports capacity and liveness from the host Docker daemon.
// It only observes containers owned by this extension and never creates or
// adopts a container. Task execution is a separate, explicitly scoped path.
type DockerObserver struct {
	Version        string
	PrivateAddress string
	Engine         DockerObservationEngine
	NvidiaSMI      string
	EnableUpgrades bool
}

func (o DockerObserver) Observe(ctx context.Context) (Observation, error) {
	if strings.TrimSpace(o.Version) == "" || strings.TrimSpace(o.PrivateAddress) == "" || o.Engine == nil {
		return Observation{}, errors.New("agent version, private address and Docker Engine are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := o.Engine.Info(ctx)
	if err != nil {
		return Observation{}, fmt.Errorf("observe Docker Engine info: %w", err)
	}
	version, err := o.Engine.ServerVersion(ctx)
	if err != nil {
		return Observation{}, fmt.Errorf("observe Docker Engine version: %w", err)
	}
	if strings.TrimSpace(info.ID) == "" || len(info.ID) > 256 || strings.ContainsAny(info.ID, "\x00\r\n") || info.NCPU <= 0 || info.MemTotal <= 0 || info.OSType != "linux" || strings.TrimSpace(version.Version) == "" || !validDockerAPIVersion(version.APIVersion) {
		return Observation{}, errors.New("docker Engine returned incomplete or unsupported host information")
	}
	if _, ok := info.Runtimes["nvidia"]; !ok {
		return Observation{}, errors.New("docker Engine NVIDIA runtime is not configured")
	}
	containers, err := o.Engine.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", dockerruntime.RuntimeLabel+"="+string(dockerruntime.RuntimeKind))),
	})
	if err != nil {
		return Observation{}, fmt.Errorf("observe managed Docker containers: %w", err)
	}
	if len(containers) > 256 {
		return Observation{}, errors.New("managed Docker container inventory exceeds limit")
	}
	gpus, err := visibleGPUs(ctx, o.NvidiaSMI)
	if err != nil {
		return Observation{}, err
	}
	capabilities := []string{"docker-observation-v1", "docker-engine/api/" + version.APIVersion, "agent-platform/" + runtime.GOOS + "/" + runtime.GOARCH}
	if o.EnableUpgrades {
		capabilities = append(capabilities, UpgradeCapability)
	}
	return Observation{
		Runtime:        "Docker",
		InstanceID:     info.ID,
		Version:        o.Version,
		PrivateAddress: o.PrivateAddress,
		Capacity: map[string]string{
			"cpu":    strconv.Itoa(info.NCPU),
			"memory": fmt.Sprintf("%d", info.MemTotal),
		},
		Capabilities:    capabilities,
		VisibleGPUUUIDs: gpus,
		// A terminal container still requires owner-confirmed removal. Never
		// present its retained execution as an idle reusable host.
		Busy: len(containers) != 0,
	}, nil
}

func validDockerAPIVersion(version string) bool {
	major, minor, ok := strings.Cut(version, ".")
	if !ok || major != "1" {
		return false
	}
	n, err := strconv.Atoi(minor)
	return err == nil && n >= 42 && strconv.Itoa(n) == minor
}
