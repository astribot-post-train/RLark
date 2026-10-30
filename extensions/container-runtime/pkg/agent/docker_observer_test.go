//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
)

type observationEngine struct {
	info       system.Info
	version    types.Version
	containers []types.Container
	err        error
}

func (e observationEngine) Info(context.Context) (system.Info, error) { return e.info, e.err }
func (e observationEngine) ServerVersion(context.Context) (types.Version, error) {
	return e.version, e.err
}
func (e observationEngine) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return e.containers, e.err
}

func TestDockerObserverReportsBoundedEngineIdentityAndRetainedWork(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' GPU-d7711468-9cea-865b-0793-c1c66b6d2c40\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := observationEngine{info: system.Info{ID: "engine-1", NCPU: 20, MemTotal: 236 << 30, OSType: "linux", Runtimes: map[string]system.RuntimeWithStatus{"nvidia": {}}}, version: types.Version{Version: "27.0", APIVersion: "1.44"}, containers: []types.Container{{ID: "old", State: "exited"}}}
	obs, err := (DockerObserver{Version: "preview", PrivateAddress: "10.0.3.85", Engine: e, NvidiaSMI: bin}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if obs.Runtime != "Docker" || obs.InstanceID != "engine-1" || !obs.Busy || obs.Capacity["cpu"] != "20" || obs.Capacity["memory"] == "" || len(obs.VisibleGPUUUIDs) != 1 {
		t.Fatalf("unexpected Docker observation: %#v", obs)
	}
	if obs.Capabilities[0] != "docker-observation-v1" {
		t.Fatalf("missing observation capability: %#v", obs.Capabilities)
	}
}

func TestDockerObserverRejectsIncompleteDaemonAndDoesNotInventCapacity(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := observationEngine{info: system.Info{ID: "engine-1", NCPU: 20, MemTotal: 236 << 30, OSType: "linux"}, version: types.Version{Version: "27.0", APIVersion: "1.44"}}
	if _, err := (DockerObserver{Version: "preview", PrivateAddress: "10.0.3.85", Engine: e, NvidiaSMI: bin}).Observe(context.Background()); err == nil {
		t.Fatal("accepted daemon without NVIDIA runtime")
	}
	if _, err := (DockerObserver{Version: "", PrivateAddress: "10.0.3.85", Engine: e}).Observe(context.Background()); err == nil {
		t.Fatal("accepted incomplete Agent configuration")
	}
}
