//go:build linux

package supervision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

func serviceFixture(t *testing.T) (*Service, rt.TaskIdentity) {
	t.Helper()
	dir := t.TempDir()
	lock, err := os.Create(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{config: Config{StateDirectory: dir, LogBytes: 16}, lock: lock, records: map[string]raw.Record{}}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "worker"}, ObjectUID: "uid"}
}

func TestPersistAndImmutableObservation(t *testing.T) {
	s, task := serviceFixture(t)
	r := raw.Record{Task: task, Spec: raw.ProcessSpec{Argv: []string{"/bin/true"}, Directory: "/work", Env: []string{"PATH=/bin"}}, Phase: "Running"}
	if err := s.persist(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.config.StateDirectory, key(task)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var recovered raw.Record
	if err := json.Unmarshal(data, &recovered); err != nil {
		t.Fatal(err)
	}
	s.records[key(task)] = recovered
	observed, err := s.Observe(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	observed.Spec.Argv[0] = "/bin/false"
	observed.Spec.Env[0] = "SECRET=bad"
	again, err := s.Observe(context.Background(), task)
	if err != nil || again.Spec.Argv[0] != "/bin/true" || again.Spec.Env[0] != "PATH=/bin" {
		t.Fatalf("mutable observation: %+v %v", again, err)
	}
	other := task
	other.ObjectUID = "replacement"
	if _, err := s.Observe(context.Background(), other); !fault.Is(err, fault.NotFound) {
		t.Fatalf("adopted replacement: %v", err)
	}
}

func TestUncertainCleanupNeverCreatesTombstone(t *testing.T) {
	s, task := serviceFixture(t)
	s.records[key(task)] = raw.Record{Task: task, Phase: "Unknown"}
	s.active = key(task)
	r, err := s.Stop(context.Background(), task)
	if err != nil || r.Cleaned || r.Deleted || s.active == "" {
		t.Fatalf("uncertain cleanup accepted: %+v %v", r, err)
	}
	r.Cleaned = true
	s.records[key(task)] = r
	r, err = s.Stop(context.Background(), task)
	if err != nil || !r.Deleted {
		t.Fatalf("confirmed cleanup failed: %+v %v", r, err)
	}
	r, err = s.Stop(context.Background(), task)
	if err != nil || !r.Deleted {
		t.Fatalf("duplicate stop failed: %+v %v", r, err)
	}
}

func TestLogsBoundedAndFullyDrained(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	w := &boundedLog{file: f, remaining: 8}
	for _, data := range []string{"first", "second", strings.Repeat("x", 10000)} {
		if n, err := w.Write([]byte(data)); err != nil || n != len(data) {
			t.Fatalf("drain failed: %d %v", n, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil || string(data) != "firstsec" {
		t.Fatalf("log bound: %q %v", data, err)
	}
}

func TestTransportRejectsPathsAndUnknownFields(t *testing.T) {
	s, task := serviceFixture(t)
	s.records[key(task)] = raw.Record{Task: task, Phase: "Unknown"}
	for _, body := range []string{`{"pid":1}`, `null`, `{}`, `{"task":{}}`, `{} {}`} {
		r := httptest.NewRequest(http.MethodPost, "/v1/stop", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handle(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	body, err := json.Marshal(request{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handle(w, httptest.NewRequest(http.MethodPost, "/arbitrary/path", bytes.NewReader(body)))
	if w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
}

func TestIndependentClientReconnectAndCancellation(t *testing.T) {
	s, task := serviceFixture(t)
	s.records[key(task)] = raw.Record{Task: task, Phase: "Running"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	path := filepath.Join(s.config.StateDirectory, "supervisor.sock")
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor socket did not appear")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		client, err := NewClient(path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := client.Observe(context.Background(), task)
		if err != nil || r.Task.ObjectUID != task.ObjectUID {
			t.Fatalf("reattach %d: %+v %v", i, r, err)
		}
		other := task
		other.ObjectUID = "other"
		if _, err := client.Observe(context.Background(), other); !fault.Is(err, fault.NotFound) {
			t.Fatalf("fault transport: %v", err)
		}
		canceled, stop := context.WithCancel(context.Background())
		stop()
		if _, err := client.Observe(canceled, task); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
		client.http.CloseIdleConnections()
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked")
	}
}

func TestNoCgroupOrUnprivilegedIsolationFailsClosed(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Fatal("accepted unisolated configuration")
	}
	if _, err := NewClient("relative.sock"); err == nil {
		t.Fatal("accepted relative socket")
	}
}

// This opt-in test needs a real writable delegated cgroup v2 subtree and root
// credential switching. Unit tests must never simulate cleanup proof by writing
// ordinary files that happen to have cgroup names.
func TestDelegatedCgroupExecutionRecoveryAndCleanup(t *testing.T) {
	group := os.Getenv("RLARK_TEST_CGROUP_DIRECTORY")
	if group == "" {
		t.Skip("set RLARK_TEST_CGROUP_DIRECTORY to an isolated delegated cgroup v2 subtree")
	}
	if os.Geteuid() != 0 {
		t.Fatal("credential isolation test requires root supervisor")
	}
	executable, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp("/tmp", "rlark-supervisor-work-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(work); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	config := Config{StateDirectory: state, Executables: []string{executable}, WorkDirectories: []string{work}, LogBytes: 1024, CgroupDirectory: group, WorkloadUID: 65534, WorkloadGID: 65534}
	service, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	task := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "process"}, ObjectUID: "real-cgroup-task"}
	spec := raw.ProcessSpec{Argv: []string{executable, "-c", "sleep 300 & wait"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
	ctx := context.Background()
	first, err := service.Ensure(ctx, task, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := service.Stop(ctx, task); err != nil {
			t.Error(err)
		}
		select {
		case <-service.done:
		case <-time.After(8 * time.Second):
			t.Error("failed to reap test workload")
		}
		if err := os.Remove(filepath.Join(group, key(task))); err != nil {
			t.Error(err)
		}
	})
	again, err := service.Ensure(ctx, task, spec)
	if err != nil || first.Task != again.Task {
		t.Fatalf("duplicate start: %+v %v", again, err)
	}
	if _, err := service.Stop(ctx, task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.done:
	case <-time.After(8 * time.Second):
		t.Fatal("cleanup deadline")
	}
	observed, err := service.Stop(ctx, task)
	if err != nil || !observed.Cleaned || !observed.Deleted || observed.Phase != "Stopped" {
		t.Fatalf("cleanup: %+v %v", observed, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	recovered, err := reopened.Observe(ctx, task)
	if err != nil || !recovered.Cleaned || !recovered.Deleted {
		t.Fatalf("durable result: %+v %v", recovered, err)
	}
}

func TestFailedAtomicPlacementPersistsFailureBeforeReleasingContainer(t *testing.T) {
	s, task := serviceFixture(t)
	executable, err := filepath.EvalSymlinks("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	s.config.Executables = []string{executable}
	s.config.WorkDirectories = []string{directory}
	// An ordinary directory cannot authorize atomic cgroup placement. The kernel
	// must reject the launch before any workload code can run.
	s.config.CgroupDirectory = t.TempDir()
	s.config.WorkloadUID, s.config.WorkloadGID = 10001, 10001
	record, err := s.Ensure(context.Background(), task, raw.ProcessSpec{Argv: []string{executable}, Directory: directory})
	if err == nil || record.Phase != "Failed" || !record.Cleaned || s.active != "" {
		t.Fatalf("failed start: %+v %v active=%q", record, err, s.active)
	}
	data, err := os.ReadFile(filepath.Join(s.config.StateDirectory, key(task)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored raw.Record
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Phase != "Failed" || !stored.Cleaned {
		t.Fatalf("missing durable failure: %+v", stored)
	}
}
