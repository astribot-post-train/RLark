package docker

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

// ContainerAdapter implements one explicit immutable container per Task.
// It deliberately provides no Ray group coordination or environment injection.
type ContainerAdapter struct{}

func (ContainerAdapter) Info(context.Context) (workload.TypeInfo, error) {
	return workload.TypeInfo{ID: CarrierType, Version: CarrierVersion, SupportedRuntimeKinds: []rt.Kind{RuntimeKind}}, nil
}

// DecodeContainer strictly decodes the v1 single-container configuration.
// GPUUUIDs is an internal lowering field; callers accepting user input must
// reject it before injecting the allocation-authorized device set.
func DecodeContainer(data []byte) (ContainerSpec, error) {
	var spec ContainerSpec
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&spec); err != nil {
		return spec, fault.New(fault.InvalidArgument, "invalid Docker container configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return spec, fault.New(fault.InvalidArgument, "exactly one Docker container configuration required")
	}
	if err := spec.Validate(); err != nil {
		return spec, fault.Wrap(fault.InvalidArgument, "validate Docker container configuration", err)
	}
	return spec, nil
}

func (ContainerAdapter) Compile(ctx context.Context, r workload.CompileRequest) (executionplan.Plan, error) {
	if err := ctx.Err(); err != nil {
		return executionplan.Plan{}, err
	}
	if r.Runtime.Kind != RuntimeKind || r.Task.CarrierSelector.Type != CarrierType || r.Task.CarrierSelector.Version != CarrierVersion || len(r.Task.Configuration.Carrier.Parameters) != 0 {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "v1 Docker container carrier required")
	}
	spec, err := DecodeContainer(r.Task.Configuration.Parameters)
	if err != nil {
		return executionplan.Plan{}, err
	}
	object, err := DesiredContainer(r.Task.Identity, spec)
	if err != nil {
		return executionplan.Plan{}, err
	}
	sum := sha256.Sum256(object.Manifest.Object)
	return executionplan.Plan{AdapterID: CarrierType, AdapterVersion: CarrierVersion, CompiledHash: hex.EncodeToString(sum[:]), Objects: []rt.DesiredObject{object}, Discovery: executionplan.InstanceDiscovery{Roots: []string{"container"}}}, nil
}

func (ContainerAdapter) Inventory(ctx context.Context, r workload.InventoryRequest) (workload.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return workload.Inventory{}, err
	}
	if r.Runtime.Kind != RuntimeKind || r.Carrier.Type != CarrierType || r.Carrier.Version != CarrierVersion {
		return workload.Inventory{}, fault.New(fault.InvalidArgument, "v1 Docker container inventory required")
	}
	return workload.Inventory{Candidates: []rt.ObjectCandidate{{Kind: ContainerKind, Namespace: r.Task.Logical.Namespace, Name: r.Task.Logical.Name, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: r.Task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup}}}, nil
}

func (ContainerAdapter) Observe(ctx context.Context, r workload.ObserveRequest) (workload.Observation, error) {
	if err := ctx.Err(); err != nil {
		return workload.Observation{}, err
	}
	if len(r.Instances.Instances) == 0 {
		return workload.Observation{Phase: "Pending"}, nil
	}
	if len(r.Instances.Instances) != 1 {
		return workload.Observation{}, fault.New(fault.Conflict, "Docker container inventory is ambiguous")
	}
	instance := r.Instances.Instances[0]
	return workload.Observation{Phase: workload.Phase(instance.Phase), Reason: instance.Reason, Message: instance.Message}, nil
}
