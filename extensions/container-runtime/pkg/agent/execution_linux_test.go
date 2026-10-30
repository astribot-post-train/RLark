//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	rawruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw/supervision"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestScopedExecutionRealSupervisor runs a CPU-only trusted root process. The
// management API is fake; mTLS transport, Unix transport and process effects are
// real. GPU-a is an injected test value and is never treated as entitlement.
func TestScopedExecutionRealSupervisor(t *testing.T) {
	scopedExecutionRealSupervisor(t, false)
}

// TestScopedTerminalExecutionRealSupervisor verifies natural completion through
// Job aggregation/release and rejects a delayed Ensure after durable cleanup.
func TestScopedTerminalExecutionRealSupervisor(t *testing.T) {
	scopedExecutionRealSupervisor(t, true)
}

func scopedExecutionRealSupervisor(t *testing.T, natural bool) {
	t.Helper()
	worker := os.Getenv("RLARK_TEST_SUPERVISOR_BIN")
	if worker == "" || os.Geteuid() != 0 {
		t.Skip("requires root and RLARK_TEST_SUPERVISOR_BIN pointing to the built supervisor")
	}
	worker, err := filepath.EvalSymlinks(worker)
	must(t, err)
	shell, err := filepath.EvalSymlinks("/bin/sh")
	must(t, err)
	work, state := t.TempDir(), t.TempDir()
	must(t, os.Chmod(state, 0700))
	service, err := supervision.Open(supervision.Config{Backend: "process-tree", WorkerExecutable: worker, StateDirectory: state, Executables: []string{shell}, WorkDirectories: []string{work}, LogBytes: 4096})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- service.Serve(ctx) }()
	identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "job-worker"}, ObjectUID: "task", Generation: 1}
	t.Cleanup(func() {
		deadline := time.Now().Add(35 * time.Second)
		for {
			r, err := service.Stop(context.Background(), identity)
			if err != nil || r.Cleaned || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("supervisor transport did not stop")
		}
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(state, "supervisor.sock")
	for deadline := time.Now().Add(3 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private supervisor socket not ready")
		}
		time.Sleep(time.Millisecond)
	}
	supervisor, err := supervision.NewClient(socket)
	must(t, err)
	defer supervisor.CloseIdleConnections()
	command := `printf '%s' "$CUDA_VISIBLE_DEVICES" > gpu-scope; sleep 60 & wait`
	if natural {
		command = `printf '%s' "$CUDA_VISIBLE_DEVICES" > gpu-scope; exit 0`
	}
	parameters, err := json.Marshal(rawruntime.ProcessSpec{Argv: []string{shell, "-c", command}, Env: []string{"PATH=/usr/bin:/bin"}, Directory: work})
	must(t, err)
	s, id, a := taskFixtureWithParameters(t, parameters)
	c := transport(t, s, id)
	for range 2 {
		must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		gpu, err := os.ReadFile(filepath.Join(work, "gpu-scope"))
		if err == nil {
			if string(gpu) != "GPU-a" {
				t.Fatal("process did not receive exact allocation GPU scope")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workload did not start")
		}
		time.Sleep(time.Millisecond)
	}
	var task native.Task
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &task))
	if !natural {
		if task.Status.Phase != native.TaskPhaseRunning {
			t.Fatalf("missing Running observation: %+v", task.Status)
		}
		task.Spec.Suspended = true
		task.Generation++
		must(t, s.Reporter.Client.Update(ctx, &task))
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		// Stop may still be in progress. Cleanup proof is the completion signal.
		_ = (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx)
		var allocation ext.ContainerAllocation
		must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &allocation))
		if allocation.Status.CleanupConfirmed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real process cleanup was not confirmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, err := supervisor.Observe(ctx, identity)
	must(t, err)
	if !record.Cleaned || !record.Deleted {
		t.Fatal("allocation released without durable supervisor cancellation")
	}
	if natural {
		r := controller.JobReservationReconciler{Client: s.Reporter.Client}
		for range 3 {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "job"}})
			must(t, err)
		}
		var job native.Job
		must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &job))
		if job.Status.Phase != native.JobPhaseSucceeded {
			t.Fatalf("natural completion not aggregated: %+v", job.Status)
		}
		if err := s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
			t.Fatalf("allocation not released: %v", err)
		}
		if _, err := supervisor.Ensure(ctx, identity, record.Spec); err == nil {
			t.Fatal("delayed Ensure restarted a released terminal execution")
		}
	}
}
