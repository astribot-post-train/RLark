package raw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	executionplan "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/plan"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
)

// ProcessWorkload is the opt-in experimental trusted-process workload. It does
// not implement Ray cluster orchestration or inject Kubernetes facilities.
const ProcessWorkload workload.TypeID = "raw.rlark.io/process"

// ProcessAdapter compiles an explicitly configured executable, environment and
// directory. Local supervisor policy remains authoritative for allowed paths.
type ProcessAdapter struct{}

func (ProcessAdapter) Info(context.Context) (workload.TypeInfo, error) {
	return workload.TypeInfo{ID: ProcessWorkload, Version: "v1", SupportedRuntimeKinds: []rt.Kind{"Raw"}}, nil
}

// DecodeProcess strictly validates the experimental process configuration.
func DecodeProcess(data []byte) (ProcessSpec, error) {
	var spec ProcessSpec
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&spec); err != nil {
		return spec, fault.New(fault.InvalidArgument, "invalid process configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return spec, fault.New(fault.InvalidArgument, "exactly one process configuration required")
	}
	if err := spec.Validate(); err != nil {
		return spec, fault.Wrap(fault.InvalidArgument, "validate process configuration", err)
	}
	return spec, nil
}

func (ProcessAdapter) Compile(ctx context.Context, r workload.CompileRequest) (executionplan.Plan, error) {
	if err := ctx.Err(); err != nil {
		return executionplan.Plan{}, err
	}
	if r.Runtime.Kind != "Raw" || r.Task.CarrierSelector.Type != processCarrier || r.Task.CarrierSelector.Version != "v1" || len(r.Task.Configuration.Carrier.Parameters) != 0 {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "v1 Raw process carrier required")
	}
	spec, err := DecodeProcess(r.Task.Configuration.Parameters)
	if err != nil {
		return executionplan.Plan{}, err
	}
	object, err := DesiredProcess(r.Task.Identity, spec)
	if err != nil {
		return executionplan.Plan{}, err
	}
	sum := sha256.Sum256(object.Manifest.Object)
	return executionplan.Plan{AdapterID: string(ProcessWorkload), AdapterVersion: "v1", CompiledHash: hex.EncodeToString(sum[:]), Objects: []rt.DesiredObject{object}, Discovery: executionplan.InstanceDiscovery{Roots: []string{"process"}}}, nil
}

func (ProcessAdapter) Inventory(ctx context.Context, r workload.InventoryRequest) (workload.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return workload.Inventory{}, err
	}
	return workload.Inventory{Candidates: []rt.ObjectCandidate{{Kind: ProcessKind, Namespace: r.Task.Logical.Namespace, Name: r.Task.Logical.Name, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: r.Task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup}}}, nil
}
func (ProcessAdapter) Observe(ctx context.Context, r workload.ObserveRequest) (workload.Observation, error) {
	if err := ctx.Err(); err != nil {
		return workload.Observation{}, err
	}
	if len(r.Instances.Instances) == 0 {
		return workload.Observation{Phase: "Pending"}, nil
	}
	if len(r.Instances.Instances) != 1 {
		return workload.Observation{}, fault.New(fault.Conflict, "Raw process inventory is ambiguous")
	}
	return workload.Observation{Phase: workload.Phase(r.Instances.Instances[0].Phase)}, nil
}
