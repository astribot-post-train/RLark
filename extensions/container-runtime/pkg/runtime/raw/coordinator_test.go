package raw

import (
	"context"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	executionplan "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/plan"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
)

type processTestAdapter struct{ spec ProcessSpec }

func (processTestAdapter) Info(context.Context) (workload.TypeInfo, error) {
	return workload.TypeInfo{ID: "test/process", Version: "v1", SupportedRuntimeKinds: []rt.Kind{"Raw"}}, nil
}
func (a processTestAdapter) Compile(_ context.Context, r workload.CompileRequest) (executionplan.Plan, error) {
	object, err := DesiredProcess(r.Task.Identity, a.spec)
	return executionplan.Plan{AdapterID: "test/process", AdapterVersion: "v1", CompiledHash: "test-process-v1", Objects: []rt.DesiredObject{object}, Discovery: executionplan.InstanceDiscovery{Roots: []string{"process"}}}, err
}
func (processTestAdapter) Inventory(_ context.Context, r workload.InventoryRequest) (workload.Inventory, error) {
	return workload.Inventory{Candidates: []rt.ObjectCandidate{{Kind: ProcessKind, Namespace: r.Task.Logical.Namespace, Name: r.Task.Logical.Name, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: r.Task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup}}}, nil
}
func (processTestAdapter) Observe(_ context.Context, r workload.ObserveRequest) (workload.Observation, error) {
	if len(r.Instances.Instances) == 0 {
		return workload.Observation{Phase: "Pending"}, nil
	}
	return workload.Observation{Phase: workload.Phase(r.Instances.Instances[0].Phase)}, nil
}

func TestCoordinatorRawProcessRecoveryObservationAndCleanup(t *testing.T) {
	ctx := context.Background()
	provider, supervisor, req := request(t)
	spec, err := decodeManifest(req.Desired.Manifest.Object)
	if err != nil {
		t.Fatal(err)
	}
	adapter := processTestAdapter{spec: spec.Spec}
	execution, err := NewCoordinator(ctx, provider, []workload.Adapter{adapter})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := coordination.TaskSnapshot{Identity: req.Task, RuntimeSelector: coordination.RuntimeSelector{ID: provider.ID, Kind: "Raw"}, WorkloadSelector: coordination.WorkloadSelector{Type: "test/process", Version: "v1"}}
	if _, err := execution.Reconcile(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	// A fresh assembly must find the same object using its concrete metadata,
	// observe the existing instance and produce no further creation operations.
	execution, err = NewCoordinator(ctx, &Provider{ID: provider.ID, Supervisor: supervisor, Admission: provider.Admission}, []workload.Adapter{adapter})
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.Reconcile(ctx, snapshot)
	if err != nil || len(result.Applied) != 0 || len(result.Evaluation.Orphans) != 0 || result.Evaluation.Observation.Phase != "Running" || supervisor.starts != 1 {
		t.Fatalf("recovery: %+v, %v, starts=%d", result, err, supervisor.starts)
	}
	supervisor.uncertain = true
	if _, err := execution.CleanupTask(ctx, req.Task, snapshot.RuntimeSelector, snapshot.WorkloadSelector, snapshot.CarrierSelector); !fault.Is(err, fault.Unavailable) {
		t.Fatalf("uncertain cleanup: %v", err)
	}
	supervisor.uncertain = false
	cleanup, err := execution.CleanupTask(ctx, req.Task, snapshot.RuntimeSelector, snapshot.WorkloadSelector, snapshot.CarrierSelector)
	if err != nil || !cleanup.Complete {
		t.Fatalf("verified deletion: %+v %v", cleanup, err)
	}
	cleanup, err = execution.CleanupTask(ctx, req.Task, snapshot.RuntimeSelector, snapshot.WorkloadSelector, snapshot.CarrierSelector)
	if err != nil || !cleanup.Complete {
		t.Fatalf("cleanup proof: %+v %v", cleanup, err)
	}
}

func TestCoordinatorRejectsUnsupportedEnvironmentBeforeStart(t *testing.T) {
	provider, supervisor, req := request(t)
	manifest, err := decodeManifest(req.Desired.Manifest.Object)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := NewCoordinator(context.Background(), provider, []workload.Adapter{processTestAdapter{spec: manifest.Spec}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := coordination.TaskSnapshot{Identity: req.Task, RuntimeSelector: coordination.RuntimeSelector{ID: provider.ID, Kind: "Raw"}, WorkloadSelector: coordination.WorkloadSelector{Type: "test/process", Version: "v1"}, Environment: executionplan.Requirements{Environments: []executionplan.EnvironmentRequirement{{Name: "network", Type: "network", Version: "v1"}}}}
	if _, err := execution.Reconcile(context.Background(), snapshot); err == nil || supervisor.starts != 0 {
		t.Fatalf("unsupported injection started a process: %v", err)
	}
}

func TestManifestCannotRenameOrReownTask(t *testing.T) {
	for _, field := range []string{"name", "namespace", "uid"} {
		t.Run(field, func(t *testing.T) {
			provider, supervisor, req := request(t)
			manifest, err := decodeManifest(req.Desired.Manifest.Object)
			if err != nil {
				t.Fatal(err)
			}
			identity := req.Task
			switch field {
			case "name":
				identity.Logical.Name = "other"
			case "namespace":
				identity.Logical.Namespace = "other"
			case "uid":
				identity.ObjectUID = "other"
			}
			desired, err := DesiredProcess(identity, manifest.Spec)
			if err != nil {
				t.Fatal(err)
			}
			req.Desired = desired
			if _, err := provider.Apply(context.Background(), req); !fault.Is(err, fault.PermissionDenied) || supervisor.starts != 0 {
				t.Fatalf("identity bypass: %v", err)
			}
		})
	}
}
