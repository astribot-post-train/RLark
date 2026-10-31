//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	ray "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DockerExecutionObserver adds the opt-in Docker Engine lifecycle to the
// observation Agent. The assignment endpoint remains the only source of Task
// identity and allocation; this process never receives a management kubeconfig.
type DockerExecutionObserver struct {
	Observer          Observer
	Client            *Client
	Engine            dockerruntime.EngineClient
	Journal           *dockerruntime.Journal
	EngineID          string
	GPUUUIDs          []string
	EnableDiagnostics bool
}

// RunDockerExecution runs the opt-in lifecycle loop while preserving cleanup
// attempts when a local observation fails. Optional diagnostics run independently.
func RunDockerExecution(ctx context.Context, o DockerExecutionObserver, interval, retryMax time.Duration, logger *slog.Logger) error {
	if o.Observer == nil || o.Client == nil || o.Engine == nil || o.Journal == nil || o.EngineID == "" {
		return errors.New("docker observer, client, Engine, journal and identity required")
	}
	if o.EnableDiagnostics {
		diagnosticCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); RunDockerDiagnostics(diagnosticCtx, o.Client, o.diagnosticProvider) }()
		defer func() { cancel(); <-done }()
	}
	return run(ctx, o.Observer, o, interval, retryMax, logger, func(ctx context.Context) error { return o.reconcile(ctx, false) })
}

// Heartbeat advertises execution only after checking the persistent local owner.
func (o DockerExecutionObserver) Heartbeat(ctx context.Context, observation Observation) error {
	if err := o.Journal.Check(ctx); err != nil {
		return errors.Join(err, o.reconcile(ctx, false))
	}
	observation.Capabilities = append(slices.Clone(observation.Capabilities), DockerExecutionCapability, ray.Capability)
	if o.EnableDiagnostics {
		observation.Capabilities = append(observation.Capabilities, DockerDiagnosticsCapability)
	}
	err := o.Client.Heartbeat(ctx, observation)
	return errors.Join(err, o.reconcile(ctx, err == nil))
}

func (o DockerExecutionObserver) reconcile(ctx context.Context, allowStart bool) error {
	a, err := o.Client.Task(ctx, TaskRequest{Action: "assignment"})
	if err != nil || a == nil {
		return err
	}
	if a.Allocation.Spec.NodeInstanceID != o.EngineID {
		return ErrIdentity
	}
	task := &a.Task
	identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: task.Namespace, Name: task.Name}, ObjectUID: string(task.UID), Generation: task.Generation}
	scoped := ScopedTasks{Client: o.Client, Allocation: objectRef(&a.Allocation), Task: objectRef(task)}
	provider := &dockerruntime.ScopedProvider{Provider: &dockerruntime.Provider{ID: dockerruntime.DefaultID, Client: o.Engine, Admission: scopedAdmission{scoped: scoped}}, Journal: o.Journal, EngineID: o.EngineID, GPUUUIDs: slices.Clone(a.Allocation.Spec.GPUUUIDs)}
	if a.Allocation.Spec.Group != nil {
		return o.reconcileRay(ctx, a, provider, scoped, identity, allowStart)
	}
	if task.Spec.Suspended || !task.DeletionTimestamp.IsZero() || taskcontract.Observed(task) && taskcontract.HasResult(task) {
		_, err := provider.Cleanup(ctx, identity)
		if err != nil {
			return err
		}
		if task.DeletionTimestamp.IsZero() && !task.Spec.Suspended && !taskcontract.Completed(task) {
			if err := scoped.WriteStatus(ctx, task, host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: string(task.Status.Phase), Conditions: []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionCleaned", Status: metav1.ConditionTrue, Reason: "DockerRemovalConfirmed", ObservedGeneration: task.Generation}}}}); err != nil {
				return err
			}
		}
		_, err = o.Client.Task(ctx, scoped.request("cleanup"))
		return err
	}
	if !allowStart {
		return nil
	}
	for _, gpu := range a.Allocation.Spec.GPUUUIDs {
		if !slices.Contains(o.GPUUUIDs, gpu) {
			return ErrCapability
		}
	}
	eligibility := dockerEligibility{task: a.Task, allocation: a.Allocation}
	execution, err := dockerruntime.NewScopedHost(ctx, scoped, scoped, task.Namespace+"/Docker/"+a.Allocation.Spec.Node.UID, eligibility, host.TaskDecoderFunc(eligibility.decode), provider, []workload.Adapter{dockerruntime.ContainerAdapter{}})
	if err != nil {
		return err
	}
	_, err = execution.Reconcile(ctx, client.ObjectKeyFromObject(task))
	return err
}

type dockerEligibility struct {
	task       api.Task
	allocation ext.ContainerAllocation
}

func (e dockerEligibility) Matches(t *api.Task) bool {
	if t == nil {
		return false
	}
	target, err := companion.Target(t)
	return err == nil && target != nil && reflect.DeepEqual(*target, e.allocation.Spec) && t.Name == e.task.Name && string(t.UID) == string(e.task.UID) && t.Namespace == e.task.Namespace && t.Spec.Runtime.Kind == api.RuntimeKind(dockerruntime.RuntimeKind) && tenant.IDOf(t) == tenant.IDOf(&e.task)
}
func (e dockerEligibility) ValidationMessage(t *api.Task) string {
	if !e.Matches(t) {
		return "Task is not bound to this Docker node"
	}
	if _, err := e.decode(t); err != nil {
		return err.Error()
	}
	return ""
}

func (e dockerEligibility) decode(t *api.Task) (coordination.TaskSnapshot, error) {
	if !e.Matches(t) || taskcontract.ValidateDockerTask(t.Spec) != nil {
		return coordination.TaskSnapshot{}, ErrCapability
	}
	snapshot, err := dockerruntime.DecodeTask(t)
	if err != nil {
		return snapshot, err
	}
	spec, err := dockerruntime.DecodeContainer(snapshot.Configuration.Parameters)
	if err != nil {
		return snapshot, err
	}
	spec.GPUUUIDs = slices.Clone(e.allocation.Spec.GPUUUIDs)
	if len(spec.GPUUUIDs) == 0 {
		return snapshot, ErrCapability
	}
	snapshot.Configuration.Parameters, err = json.Marshal(spec)
	return snapshot, err
}

func (o DockerExecutionObserver) diagnosticProvider(ctx context.Context, task rt.TaskIdentity) (*dockerruntime.ScopedProvider, error) {
	a, err := o.Client.Task(ctx, TaskRequest{Action: "assignment"})
	if err != nil {
		return nil, err
	}
	if a == nil || string(a.Task.UID) != task.ObjectUID || a.Task.Generation != task.Generation || a.Task.Namespace != task.Logical.Namespace || a.Task.Name != task.Logical.Name || a.Task.Spec.Runtime.Kind != "Docker" || a.Allocation.Spec.NodeInstanceID != o.EngineID || a.Allocation.Status.CleanupConfirmed {
		return nil, ErrIdentity
	}
	return &dockerruntime.ScopedProvider{Provider: &dockerruntime.Provider{ID: dockerruntime.DefaultID, Client: o.Engine}, Journal: o.Journal, EngineID: o.EngineID, GPUUUIDs: slices.Clone(a.Allocation.Spec.GPUUUIDs)}, nil
}
