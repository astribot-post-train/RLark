package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	rawruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ExecutionObserver adds one scoped reconciliation after each fresh local
// observation and heartbeat. Observation-only remains the command default.
type ExecutionObserver struct {
	Observer   Observer
	Client     *Client
	Supervisor rawruntime.Supervisor
}

// RunExecution keeps cleanup available even when a local observation fails.
// That path never starts processes or publishes a synthetic heartbeat.
func RunExecution(ctx context.Context, o ExecutionObserver, interval, retryMax time.Duration, logger *slog.Logger) error {
	if o.Observer == nil || o.Client == nil || o.Supervisor == nil {
		return errors.New("observer, scoped client and supervisor required")
	}
	return run(ctx, o, o, interval, retryMax, logger, func(ctx context.Context) error { return o.reconcile(ctx, false) })
}

// Observe delegates local observation without treating absent work as cleanup.
func (o ExecutionObserver) Observe(ctx context.Context) (Observation, error) {
	return o.Observer.Observe(ctx)
}

// Heartbeat sends the current observation first. A failed heartbeat still allows
// cleanup attempts: stale readiness must block starts, never exact-owner stop.
func (o ExecutionObserver) Heartbeat(ctx context.Context, obs Observation) error {
	heartbeatErr := o.Client.Heartbeat(ctx, obs)
	executionErr := o.Reconcile(ctx)
	return errors.Join(heartbeatErr, executionErr)
}

// Reconcile recovers the exact allocation, reuses Host/Coordinator for active
// processes, and confirms durable cancellation before reporting stopped work.
func (o ExecutionObserver) Reconcile(ctx context.Context) error {
	return o.reconcile(ctx, true)
}

func (o ExecutionObserver) reconcile(ctx context.Context, allowStart bool) error {
	if o.Client == nil || o.Supervisor == nil {
		return errors.New("scoped client and supervisor required")
	}
	a, err := o.Client.Task(ctx, TaskRequest{Action: "assignment"})
	if err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	scoped := ScopedTasks{Client: o.Client, Allocation: objectRef(&a.Allocation), Task: objectRef(&a.Task)}
	task := &a.Task
	if task.Spec.Suspended || !task.DeletionTimestamp.IsZero() {
		identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: task.Namespace, Name: task.Name}, ObjectUID: string(task.UID), Generation: task.Generation}
		record, err := o.Supervisor.Stop(ctx, identity)
		if err != nil {
			return err
		}
		if record.Task.Logical != identity.Logical || record.Task.ObjectUID != identity.ObjectUID || !record.Cleaned || !record.Deleted {
			return errors.New("supervisor has not proved durable cancellation and cleanup")
		}
		_, err = o.Client.Task(ctx, scoped.request("cleanup"))
		return err
	}
	if !allowStart {
		return nil
	}
	eligibility := allocatedEligibility{assignment: a}
	provider := &rawruntime.Provider{ID: "container-process", Supervisor: o.Supervisor, Admission: scopedAdmission{scoped: scoped}}
	execution, err := rawruntime.NewScopedHost(ctx, scoped, scoped, task.Namespace+"/Raw/"+a.Allocation.Spec.Node.UID, eligibility, host.TaskDecoderFunc(eligibility.decode), provider, []workload.Adapter{rawruntime.ProcessAdapter{}})
	if err != nil {
		return err
	}
	_, err = execution.Reconcile(ctx, client.ObjectKeyFromObject(task))
	return err
}

type scopedAdmission struct{ scoped ScopedTasks }

func (s scopedAdmission) Authorize(ctx context.Context, t rt.TaskIdentity) error {
	if t.ObjectUID != s.scoped.Task.UID || t.Logical.Namespace != s.scoped.Task.Namespace || t.Logical.Name != s.scoped.Task.Name {
		return ErrIdentity
	}
	a, err := s.scoped.Client.Task(ctx, s.scoped.request("authorize"))
	if err != nil {
		return err
	}
	if err := s.scoped.accept(a); err != nil {
		return err
	}
	if a.Task.Generation != t.Generation {
		return ErrIdentity
	}
	return nil
}

type allocatedEligibility struct{ assignment *Assignment }

func (e allocatedEligibility) Matches(t *native.Task) bool {
	a := e.assignment
	target, err := companion.Target(t)
	return err == nil && target != nil && objectRef(t) == objectRef(&a.Task) && t.Spec.Runtime.Kind == "Raw" && t.Spec.Runtime.Name == "" && t.Spec.Placement.Binding == nil && t.Spec.Placement.DockerBinding == nil && target.NodeRecordUID == a.Allocation.Spec.NodeRecordUID && target.ExecutionID == a.Allocation.Spec.ExecutionID
}
func (e allocatedEligibility) ValidationMessage(t *native.Task) string {
	if _, err := e.decode(t); err != nil {
		return "Task is not a supported allocated Raw process"
	}
	return ""
}
func (e allocatedEligibility) decode(t *native.Task) (coordination.TaskSnapshot, error) {
	if !e.Matches(t) || string(t.Spec.Workload.Kind) != string(rawruntime.ProcessWorkload) || t.Spec.Workload.Version != "v1" {
		return coordination.TaskSnapshot{}, ErrCapability
	}
	snapshot, err := rawruntime.DecodeTask(t)
	if err != nil {
		return snapshot, err
	}
	spec, err := rawruntime.DecodeProcess(snapshot.Configuration.Parameters)
	if err != nil {
		return snapshot, err
	}
	for _, env := range spec.Env {
		name, _, _ := strings.Cut(env, "=")
		if name == "CUDA_VISIBLE_DEVICES" || name == "NVIDIA_VISIBLE_DEVICES" {
			return snapshot, ErrCapability
		}
	}
	if len(e.assignment.Allocation.Spec.GPUUUIDs) == 0 {
		return snapshot, ErrCapability
	}
	spec.Env = append(spec.Env, "CUDA_VISIBLE_DEVICES="+strings.Join(e.assignment.Allocation.Spec.GPUUUIDs, ","))
	data, err := json.Marshal(spec)
	if err != nil {
		return snapshot, err
	}
	snapshot.Configuration.Parameters = data
	return snapshot, nil
}
