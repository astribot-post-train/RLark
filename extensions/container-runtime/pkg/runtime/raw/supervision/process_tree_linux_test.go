//go:build linux

package supervision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"golang.org/x/sys/unix"
)

// TestTreeWorker runs in a separate process, never as a subreaper in go test.
func TestTreeWorker(t *testing.T) {
	if os.Args[len(os.Args)-1] != "--process-tree-worker" {
		return
	}
	if err := RunProcessTreeWorker(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func treeFixture(t *testing.T) (*Service, rt.TaskIdentity, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root process-tree execution test; also run on the target cloud container")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(t.TempDir(), "worker")
	// The test binary is invoked through an explicit script, without importing
	// test-helper dispatch into the production package or inheriting test env.
	if err := os.WriteFile(worker, []byte(fmt.Sprintf("#!/bin/sh\nexec '%s' -test.run='^TestTreeWorker$' -- \"$@\"\n", strings.ReplaceAll(bin, "'", "'\\''"))), 0700); err != nil {
		t.Fatal(err)
	}
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	config := Config{Backend: "process-tree", WorkerExecutable: worker, StateDirectory: t.TempDir(), Executables: []string{shell}, WorkDirectories: []string{work}, LogBytes: 4096}
	if err := os.Chmod(config.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.mu.Lock()
		running, done := s.process != nil, s.done
		var tasks []rt.TaskIdentity
		for _, r := range s.records {
			if !r.Cleaned {
				tasks = append(tasks, r.Task)
			}
		}
		s.mu.Unlock()
		if running {
			ctx := context.Background()
			for _, task := range tasks {
				_, _ = s.Stop(ctx, task)
			}
			select {
			case <-done:
			case <-time.After(35 * time.Second):
				t.Error("test worker did not exit")
			}
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "root"}, ObjectUID: "root-task", Generation: 1}, work
}

func TestTreeCancellationSurvivesSupervisorRestart(t *testing.T) {
	s, task, work := treeFixture(t)
	if result, err := s.Stop(context.Background(), task); err != nil || !result.Cleaned || !result.Deleted {
		t.Fatalf("cancel absent Task: %+v %v", result, err)
	}
	config := s.config
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	spec := raw.ProcessSpec{Argv: []string{config.Executables[0], "-c", "exit 0"}, Directory: work}
	if _, err := reopened.Ensure(context.Background(), task, spec); !fault.Is(err, fault.Conflict) {
		t.Fatalf("restart forgot cancellation: %v", err)
	}
	if status, err := reopened.Status(context.Background()); err != nil || status.Busy {
		t.Fatalf("cancelled absence became occupied after restart: %+v %v", status, err)
	}
}

func waitTree(t *testing.T, s *Service, task rt.TaskIdentity) raw.Record {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("process-tree worker did not complete")
	}
	r, err := s.Observe(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTreeDetachedDescendantCleanupAndUnrelatedProcess(t *testing.T) {
	s, task, work := treeFixture(t)
	unrelated := exec.Command("/bin/sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "setsid sh -c 'sleep 60 & echo $! > child.pid; wait' >/dev/null 2>&1 & while [ ! -s child.pid ]; do sleep .01; done; exit 0"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
	if _, err := s.Ensure(context.Background(), task, spec); err != nil {
		t.Fatal(err)
	}
	r := waitTree(t, s, task)
	if r.Phase != "Succeeded" || !r.Cleaned {
		t.Fatalf("not cleaned: %+v", r)
	}
	data, err := os.ReadFile(filepath.Join(work, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("descendant remains: %d %v", pid, err)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process affected: %v", err)
	}
	// Repeating Ensure recovers the completed result without starting it again.
	if again, err := s.Ensure(context.Background(), task, spec); err != nil || !again.Cleaned {
		t.Fatalf("duplicate ensure: %+v %v", again, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if r, err := reopened.Observe(context.Background(), task); err != nil || !r.Cleaned {
		t.Fatalf("recovery: %+v %v", r, err)
	}
}

func TestTreeStopAndUnknownWorkerOutcome(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		s, task, work := treeFixture(t)
		spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "sleep 60 & wait"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
		if _, err := s.Ensure(context.Background(), task, spec); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := s.Stop(context.Background(), task); err != nil {
				t.Fatal(err)
			}
		}
		r := waitTree(t, s, task)
		if !r.Cleaned || r.Phase != "Stopped" {
			t.Fatalf("stop: %+v", r)
		}
		if r, err := s.Stop(context.Background(), task); err != nil || !r.Deleted {
			t.Fatalf("tombstone: %+v %v", r, err)
		}
	})
	t.Run("missing-proof", func(t *testing.T) {
		s, task, work := treeFixture(t)
		// A worker that exits without an outcome cannot prove no workload ran.
		s.config.WorkerExecutable, _ = filepath.EvalSymlinks("/bin/true")
		spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "exit 0"}, Directory: work}
		if _, err := s.Ensure(context.Background(), task, spec); err != nil {
			t.Fatal(err)
		}
		r := waitTree(t, s, task)
		if r.Cleaned || r.Phase != "Unknown" {
			t.Fatalf("lost worker released: %+v", r)
		}
		other := task
		other.ObjectUID = "replacement"
		if _, err := s.Ensure(context.Background(), other, spec); !fault.Is(err, fault.Conflict) {
			t.Fatalf("occupied node accepted: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(s.config)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.Ensure(context.Background(), other, spec); !fault.Is(err, fault.Conflict) {
			t.Fatalf("restart released unknown execution: %v", err)
		}
	})
}

func TestSupervisionBackendValidation(t *testing.T) {
	for _, backend := range []string{"unknown", "process-tree"} {
		if s, err := Open(Config{Backend: backend}); err == nil {
			_ = s.Close()
			t.Fatalf("accepted invalid backend config: %s", backend)
		}
	}
}

func TestTreeResultPersistenceFailureKeepsNodeOccupied(t *testing.T) {
	s, task, work := treeFixture(t)
	// The workload waits for a test-controlled file so that the result cannot
	// race the injected storage failure. No persistence hooks bypass real I/O.
	spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "while [ ! -f release ]; do sleep .01; done"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
	if _, err := s.Ensure(context.Background(), task, spec); err != nil {
		t.Fatal(err)
	}
	state := s.config.StateDirectory
	moved := state + "-retained"
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	// Replacing the state directory with a file makes persistence fail even
	// under root. The previously durable intent remains in moved.
	if err := os.WriteFile(state, []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(moved); err == nil {
			_ = os.Remove(state)
			_ = os.Rename(moved, state)
		}
	})
	if err := os.WriteFile(filepath.Join(work, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	r := waitTree(t, s, task)
	if r.Phase != "Unknown" || r.Cleaned {
		t.Fatalf("unpersisted result advertised cleanup: %+v", r)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, state); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Stop(context.Background(), task); err != nil || r.Cleaned || r.Deleted {
		t.Fatalf("stop released uncertain result after storage recovery: %+v %v", r, err)
	}
	other := task
	other.ObjectUID = "replacement"
	if _, err := s.Ensure(context.Background(), other, spec); !fault.Is(err, fault.Conflict) {
		t.Fatalf("unpersisted result released capacity: %v", err)
	}
}

func TestTreeLiveSocketReconnect(t *testing.T) {
	s, task, work := treeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	socket := filepath.Join(s.config.StateDirectory, "supervisor.sock")
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor socket unavailable")
		}
		time.Sleep(time.Millisecond)
	}
	spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "sleep 60 & wait"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
	for i := 0; i < 2; i++ {
		c, err := NewClient(socket)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Ensure(context.Background(), task, spec)
		c.http.CloseIdleConnections()
		if err != nil || r.Task != task || r.Phase != "Running" {
			t.Fatalf("reconnect %d: %+v %v", i, r, err)
		}
	}
	c, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.http.CloseIdleConnections()
	if _, err := c.Stop(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if r := waitTree(t, s, task); !r.Cleaned || r.Phase != "Stopped" {
		t.Fatalf("reattached stop: %+v", r)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("socket shutdown timed out")
	}
}

func TestTreeStopEscalatesWhenMainIgnoresTERM(t *testing.T) {
	s, task, work := treeFixture(t)
	spec := raw.ProcessSpec{Argv: []string{s.config.Executables[0], "-c", "trap '' TERM; echo ready > ready; while :; do sleep 1; done"}, Directory: work, Env: []string{"PATH=/usr/bin:/bin"}}
	if _, err := s.Ensure(context.Background(), task, spec); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(work, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("main did not install TERM handler")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	if _, err := s.Stop(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(treeGrace + 10*time.Second):
		t.Fatal("SIGKILL escalation did not complete")
	}
	if time.Since(started) < treeGrace {
		t.Fatal("graceful shutdown deadline was not respected")
	}
	if r := waitTree(t, s, task); !r.Cleaned || r.Phase != "Stopped" {
		t.Fatalf("forced stop: %+v", r)
	}
}
