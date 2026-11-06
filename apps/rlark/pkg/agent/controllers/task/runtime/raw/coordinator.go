package raw

import (
	"context"
	"errors"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/binding"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination/coordinator"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/environment"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	executionplan "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/plan"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
)

// NewCoordinator assembles the Raw Provider through the same compilation,
// binding, diff and identity-cleanup protocol as other Runtime implementations.
// Adapters must be explicitly supplied; this does not advertise Ray support.
func NewCoordinator(ctx context.Context, provider *Provider, adapters []workload.Adapter) (*coordinator.Coordinator, error) {
	if provider == nil || len(adapters) == 0 {
		return nil, errors.New("raw provider and explicit workload adapters required")
	}
	info, err := provider.Info(ctx)
	if err != nil {
		return nil, err
	}
	registrations := make([]coordinator.WorkloadRegistration, 0, len(adapters))
	seen := map[string]bool{}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil Raw workload adapter")
		}
		declared, err := adapter.Info(ctx)
		if err != nil {
			return nil, err
		}
		supported := false
		for _, kind := range declared.SupportedRuntimeKinds {
			if kind == info.Kind {
				supported = true
			}
		}
		if declared.ID == "" || declared.Version == "" || !supported {
			return nil, errors.New("workload adapter does not declare a versioned Raw implementation")
		}
		key := string(declared.ID) + "\x00" + declared.Version
		if seen[key] {
			return nil, errors.New("duplicate Raw workload implementation")
		}
		if _, ok := adapter.(workload.InventoryAdapter); !ok {
			return nil, errors.New("raw workload adapter requires identity-based cleanup inventory")
		}
		seen[key] = true
		registrations = append(registrations, coordinator.WorkloadRegistration{Type: declared.ID, Version: declared.Version, RuntimeKind: info.Kind, Adapter: adapter})
	}
	handlers, err := environment.NewRegistry(nil)
	if err != nil {
		return nil, err
	}
	return &coordinator.Coordinator{
		Runtimes: []coordinator.RuntimeRegistration{{Info: info, Provider: provider}}, Workloads: registrations,
		Resolver: existingContainerEnvironment{},
		Binder:   binding.BindingPlanBinder{Handlers: handlers, Validator: binding.DesiredPlanValidator{RuntimeKind: info.Kind, Objects: provider}, Lowerer: provider, Workloads: processAssembler{}},
	}, nil
}

// Existing container mounts, network and devices are admission prerequisites,
// not Kubernetes environment mutations. Reject requests to inject facilities.
type existingContainerEnvironment struct{}

func (existingContainerEnvironment) Resolve(ctx context.Context, request environment.ResolveRequest) (environment.ResolvedEnvironment, error) {
	if err := ctx.Err(); err != nil {
		return environment.ResolvedEnvironment{}, err
	}
	if request.Runtime.Kind != "Raw" || !request.Requirements.Empty() || len(request.Targets) != 0 {
		return environment.ResolvedEnvironment{}, fault.New(fault.InvalidArgument, "Raw environment injection is unsupported")
	}
	return environment.ResolvedEnvironment{Task: request.Task}, nil
}

type processAssembler struct{}

func (processAssembler) Assemble(ctx context.Context, _ binding.BindRequest, plan *executionplan.Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan == nil || len(plan.Objects) != 1 || len(plan.Targets) != 0 || len(plan.Units) != 0 || len(plan.Processes) != 0 || !plan.Environment.Empty() {
		return fault.New(fault.InvalidArgument, "Raw requires exactly one precompiled process without Pod facilities")
	}
	return nil
}
