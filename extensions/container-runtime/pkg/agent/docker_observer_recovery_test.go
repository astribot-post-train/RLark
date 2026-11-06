//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
)

type failingObservationEngine struct {
	observationEngine
	fail  string
	calls int
	t     *testing.T
}

func (e *failingObservationEngine) check(ctx context.Context, call string) error {
	e.t.Helper()
	e.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second {
		e.t.Fatal("unbounded Engine call")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.fail == call {
		return errors.New("daemon unavailable")
	}
	return nil
}
func (e *failingObservationEngine) Info(ctx context.Context) (system.Info, error) {
	return e.info, e.check(ctx, "info")
}
func (e *failingObservationEngine) ServerVersion(ctx context.Context) (types.Version, error) {
	return e.version, e.check(ctx, "version")
}
func (e *failingObservationEngine) ContainerList(ctx context.Context, opts container.ListOptions) ([]types.Container, error) {
	if !opts.All || len(opts.Filters.Get("label")) != 1 || opts.Filters.Get("label")[0] != dockerruntime.RuntimeLabel+"=Docker" {
		e.t.Fatalf("unsafe inventory request: %#v", opts)
	}
	return e.containers, e.check(ctx, "list")
}
func dockerObservationFixture(t *testing.T) (DockerObserver, *failingObservationEngine) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'GPU-d7711468-9cea-865b-0793-c1c66b6d2c40\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &failingObservationEngine{t: t, observationEngine: observationEngine{info: system.Info{ID: "engine-1", NCPU: 20, MemTotal: 236 << 30, OSType: "linux", Runtimes: map[string]system.RuntimeWithStatus{"nvidia": {}}}, version: types.Version{Version: "27", APIVersion: "1.44"}}}
	return DockerObserver{Version: "v1", PrivateAddress: "10.0.3.85", Engine: e, NvidiaSMI: bin}, e
}
func TestDockerObserverOutageRecoveryReReadsCurrentState(t *testing.T) {
	for _, call := range []string{"info", "version", "list"} {
		t.Run(call, func(t *testing.T) {
			observer, e := dockerObservationFixture(t)
			first, err := observer.Observe(context.Background())
			if err != nil || first.Busy {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			e.fail = call
			failed, err := observer.Observe(context.Background())
			if err == nil || failed.Version != "" {
				t.Fatalf("outage replayed a successful observation: %#v %v", failed, err)
			}
			e.fail = ""
			e.info.ID = "engine-replaced"
			e.containers = []types.Container{{State: "exited"}}
			recovered, err := observer.Observe(context.Background())
			if err != nil || !recovered.Busy || recovered.InstanceID != "engine-replaced" {
				t.Fatalf("recovery=%#v err=%v", recovered, err)
			}
		})
	}
}
func TestDockerObserverRejectsUnsupportedInformationAndGPUFailure(t *testing.T) {
	for _, failure := range []string{"old-api", "malformed-api", "other-api", "no-id", "id-newline", "id-limit", "cpu", "memory", "os", "version", "nvidia", "inventory-limit", "gpu"} {
		t.Run(failure, func(t *testing.T) {
			o, e := dockerObservationFixture(t)
			switch failure {
			case "old-api":
				e.version.APIVersion = "1.41"
			case "malformed-api":
				e.version.APIVersion = "1.044"
			case "other-api":
				e.version.APIVersion = "2.0"
			case "no-id":
				e.info.ID = ""
			case "id-newline":
				e.info.ID = "engine\n"
			case "id-limit":
				e.info.ID = strings.Repeat("a", 257)
			case "cpu":
				e.info.NCPU = 0
			case "memory":
				e.info.MemTotal = 0
			case "os":
				e.info.OSType = "windows"
			case "version":
				e.version.Version = ""
			case "nvidia":
				e.info.Runtimes = nil
			case "inventory-limit":
				e.containers = make([]types.Container, 257)
			case "gpu":
				o.NvidiaSMI = "/missing/nvidia-smi"
			}
			obs, err := o.Observe(context.Background())
			if err == nil || obs.Version != "" {
				t.Fatalf("accepted invalid observation: %#v err=%v", obs, err)
			}
		})
	}
}
func TestDockerObserverHonorsCancellation(t *testing.T) {
	o, e := dockerObservationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := o.Observe(ctx)
	if !errors.Is(err, context.Canceled) || e.calls != 1 {
		t.Fatalf("calls=%d err=%v", e.calls, err)
	}
}
func TestDockerObserverOfficialSDKUnixSocketRecovery(t *testing.T) {
	o, _ := dockerObservationFixture(t)
	// Use a short path to fit Linux's Unix socket pathname limit.
	dir, err := os.MkdirTemp("", "rlark-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "engine.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var outage atomic.Bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.44")
			return
		}
		if outage.Load() {
			http.Error(w, "outage", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1.44/info":
			_, _ = w.Write([]byte(`{"ID":"unix-engine","NCPU":4,"MemTotal":1024,"OSType":"linux","Runtimes":{"nvidia":{}}}`))
		case "/v1.44/version":
			_, _ = w.Write([]byte(`{"Version":"27.0","ApiVersion":"1.44"}`))
		case "/v1.44/containers/json":
			var filters map[string]map[string]bool
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil || r.URL.Query().Get("all") != "1" || !filters["label"][dockerruntime.RuntimeLabel+"=Docker"] {
				t.Errorf("unsafe query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"Id":"retained","State":"exited"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	if err := srv.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	engine, err := dockerruntime.NewSDKClient("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	o.Engine = engine
	for _, down := range []bool{false, true, false} {
		outage.Store(down)
		obs, err := o.Observe(context.Background())
		if down {
			if err == nil || obs.Version != "" {
				t.Fatalf("stale outage report: %#v %v", obs, err)
			}
		} else if err != nil || obs.InstanceID != "unix-engine" || !obs.Busy {
			t.Fatalf("obs=%#v err=%v", obs, err)
		}
	}
}
