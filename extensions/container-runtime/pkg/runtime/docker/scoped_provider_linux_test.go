//go:build linux

package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

type journalEngine struct {
	lifecycleEngine
	createErr                     error
	lostCreate                    bool
	imageErr                      error
	createEntered, createContinue chan struct{}
}

func (e *journalEngine) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	if e.current == nil {
		return nil, nil
	}
	return []types.Container{{ID: e.current.ID}}, nil
}
func (e *journalEngine) ImageInspectWithRaw(context.Context, string) (types.ImageInspect, []byte, error) {
	return types.ImageInspect{}, nil, e.imageErr
}
func (e *journalEngine) ContainerCreate(ctx context.Context, c *container.Config, h *container.HostConfig, n *network.NetworkingConfig, p *specs.Platform, name string) (container.CreateResponse, error) {
	if e.createEntered != nil {
		close(e.createEntered)
		select {
		case <-e.createContinue:
		case <-ctx.Done():
			return container.CreateResponse{}, ctx.Err()
		}
	}
	if e.createErr != nil {
		return container.CreateResponse{}, e.createErr
	}
	r, err := e.lifecycleEngine.ContainerCreate(ctx, c, h, n, p, name)
	if e.lostCreate {
		return container.CreateResponse{}, errors.New("lost create response")
	}
	return r, err
}
func scopedFixture(t *testing.T) (*ScopedProvider, *journalEngine, rt.ApplyRequest, string) {
	t.Helper()
	base, _, req := providerFixture(t)
	e := &journalEngine{}
	e.info.ID = "engine"
	base.Client = e
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	spec := ContainerSpec{Image: "image", Entrypoint: []string{"/bin/true"}, HostNetwork: true, GPUUUIDs: []string{"GPU-a"}}
	req.Desired, err = DesiredContainer(req.Task, spec)
	if err != nil {
		t.Fatal(err)
	}
	return &ScopedProvider{Provider: base, Journal: j, EngineID: "engine", GPUUUIDs: []string{"GPU-a"}}, e, req, path
}
func reopen(t *testing.T, p *ScopedProvider, path string) {
	t.Helper()
	if err := p.Journal.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Journal = j
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
}
func TestScopedCancellationPersistsBeforeCreateAndAcrossRestart(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
	reopen(t, p, path)
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("late create: %v", err)
	}
	if e.creates != 0 || e.starts != 0 {
		t.Fatal("cancelled task started")
	}
	record, err := p.record(ctx, req.Task)
	if err != nil || !record.Cancelled || !record.Cleaned {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}
func TestScopedLostResponseRecoveryAndCleanupFailure(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	e.lostCreate = true
	if _, err := p.Apply(ctx, req); err == nil {
		t.Fatal("lost response hidden")
	}
	reopen(t, p, path)
	e.lostCreate = false
	if _, err := p.Apply(ctx, req); err != nil {
		t.Fatal(err)
	}
	if e.creates != 1 || e.starts != 1 {
		t.Fatalf("duplicate create/start: %d/%d", e.creates, e.starts)
	}
	requests := e.current.HostConfig.DeviceRequests
	if len(requests) != 1 || requests[0].Count != 0 || requests[0].Driver != "nvidia" || len(requests[0].DeviceIDs) != 1 || requests[0].DeviceIDs[0] != "GPU-a" {
		t.Fatalf("devices=%+v", requests)
	}
	e.removeErr = errors.New("offline")
	if _, err := p.Cleanup(ctx, req.Task); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("cancelled during cleanup: %v", err)
	}
	e.removeErr = nil
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
	if e.current != nil {
		t.Fatal("cleanup did not remove container")
	}
}
func TestScopedUncertainCreateRetainsCancellationUntilContainerAppears(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	e.createErr = context.DeadlineExceeded
	if _, err := p.Apply(ctx, req); err == nil {
		t.Fatal("timeout hidden")
	}
	reopen(t, p, path)
	if _, err := p.Cleanup(ctx, req.Task); !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("uncertain absence released: %v", err)
	}
	cfg, h := dockerConfig(ContainerSpec{Image: "image", HostNetwork: true, GPUUUIDs: []string{"GPU-a"}}, req.Task)
	_, err := e.lifecycleEngine.ContainerCreate(ctx, cfg, h, nil, nil, containerName(req.Task))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
	if e.starts != 0 || e.current != nil {
		t.Fatal("late create was started or retained")
	}
}
func TestScopedPreCreateFailureRetriesAndEngineReplacementFails(t *testing.T) {
	p, e, req, _ := scopedFixture(t)
	ctx := context.Background()
	e.imageErr = errors.New("registry unavailable")
	if _, err := p.Apply(ctx, req); err == nil {
		t.Fatal("image failure hidden")
	}
	record, err := p.record(ctx, req.Task)
	if err != nil || record.Creating {
		t.Fatalf("premature create intent: %+v %v", record, err)
	}
	e.imageErr = nil
	e.info.ID = "replacement"
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.PermissionDenied) {
		t.Fatalf("Engine replacement: %v", err)
	}
	e.info.ID = "engine"
	if _, err := p.Apply(ctx, req); err != nil {
		t.Fatal(err)
	}
}
func TestScopedInventoryRejectsLabelsWithoutJournalAndIDReplacement(t *testing.T) {
	p, e, req, _ := scopedFixture(t)
	ctx := context.Background()
	if _, err := p.Provider.Apply(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListManaged(ctx, req.Task); !fault.Is(err, fault.Conflict) {
		t.Fatalf("unrecorded inventory adopted: %v", err)
	}
	if _, err := p.Cleanup(ctx, req.Task); !fault.Is(err, fault.Conflict) {
		t.Fatalf("unrecorded cleanup adopted: %v", err)
	}
	if e.removes != 0 {
		t.Fatal("unrecorded container removed")
	}
}
func TestScopedConcurrentStopWaitsForCreateAndCancelsLateRetry(t *testing.T) {
	p, e, req, _ := scopedFixture(t)
	ctx := context.Background()
	e.createEntered = make(chan struct{})
	e.createContinue = make(chan struct{})
	applied := make(chan error, 1)
	go func() { _, err := p.Apply(ctx, req); applied <- err }()
	<-e.createEntered
	stopped := make(chan error, 1)
	go func() { _, err := p.Cleanup(ctx, req.Task); stopped <- err }()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Cleanup(cancelled, req.Task); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(e.createContinue)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("late retry: %v", err)
	}
	if e.current != nil || e.creates != 1 {
		t.Fatal("concurrent cleanup failed")
	}
}
func TestJournalRefusesOtherProcessAndUnsafeStorage(t *testing.T) {
	if path := os.Getenv("RLARK_DOCKER_LOCK_CHILD"); path != "" {
		if j, err := OpenJournal(path); err == nil {
			_ = j.Close()
			os.Exit(3)
		}
		return
	}
	p, _, req, path := scopedFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournalRefusesOtherProcessAndUnsafeStorage$")
	cmd.Env = append(os.Environ(), "RLARK_DOCKER_LOCK_CHILD="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("other process acquired journal: %v %s", err, output)
	}
	if _, err := OpenJournal(path); err == nil {
		t.Fatal("second owner accepted")
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background(), req); err == nil {
		t.Fatal("unsafe directory accepted")
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.Journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "broken.json"), []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	if j, err := OpenJournal(path); err == nil {
		_ = j.Close()
		t.Fatal("corrupt record accepted")
	}
}
func TestJournalWaitHonorsContext(t *testing.T) {
	p, _, _, _ := scopedFixture(t)
	unlock, err := p.Journal.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Journal.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestJournalMissingRecordOrReplacedLockFailsClosed(t *testing.T) {
	for _, mode := range []string{"record", "lock", "index"} {
		t.Run(mode, func(t *testing.T) {
			p, _, req, path := scopedFixture(t)
			ctx := context.Background()
			if _, err := p.Cleanup(ctx, req.Task); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "record":
				if err := os.Remove(filepath.Join(path, journalKey(req.Task)+".json")); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.Rename(filepath.Join(path, ".lock"), filepath.Join(path, "old-lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, ".lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "index":
				if err := os.Rename(filepath.Join(path, ".index"), filepath.Join(path, "old-index")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, ".index"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Apply(ctx, req); err == nil {
				t.Fatal("lost cancellation storage accepted")
			}
			if mode == "record" {
				if err := p.Journal.Close(); err != nil {
					t.Fatal(err)
				}
				if j, err := OpenJournal(path); err == nil {
					_ = j.Close()
					t.Fatal("missing record accepted after restart")
				}
			}
		})
	}
}
func TestScopedEngineOutageStillPersistsCancellation(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	e.infoErr = errors.New("Engine unavailable")
	if _, err := p.Cleanup(ctx, req.Task); err == nil {
		t.Fatal("Engine outage reported clean")
	}
	record, err := p.record(ctx, req.Task)
	if err != nil || !record.Cancelled || record.Cleaned {
		t.Fatalf("cancellation not durable: %+v %v", record, err)
	}
	reopen(t, p, path)
	e.infoErr = nil
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("outage cancellation lost: %v", err)
	}
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
}
