package docker

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

// NewCoordinator assembles the Docker provider and explicitly registered
// workload adapters. Docker workers are launched by the host daemon; no
// Docker-in-Docker or Kubernetes object is involved.
func NewCoordinator(ctx context.Context, provider *Provider, adapters []workload.Adapter) (*coordinator.Coordinator, error) {
	if provider == nil || len(adapters) == 0 {
		return nil, errors.New("Docker provider and workload adapters are required")
	}
	info, err := provider.Info(ctx)
	if err != nil {
		return nil, err
	}
	registrations := make([]coordinator.WorkloadRegistration, 0, len(adapters))
	seen := map[string]struct{}{}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil Docker workload adapter")
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
			return nil, errors.New("workload adapter does not declare a versioned Docker implementation")
		}
		key := string(declared.ID) + "\x00" + declared.Version
		if _, ok := seen[key]; ok {
			return nil, errors.New("duplicate Docker workload implementation")
		}
		if _, ok := adapter.(workload.InventoryAdapter); !ok {
			return nil, errors.New("Docker workload adapter requires identity-based cleanup inventory")
		}
		seen[key] = struct{}{}
		registrations = append(registrations, coordinator.WorkloadRegistration{Type: declared.ID, Version: declared.Version, RuntimeKind: info.Kind, Adapter: adapter})
	}
	handlers, err := environment.NewRegistry(nil)
	if err != nil {
		return nil, err
	}
	return &coordinator.Coordinator{
		Runtimes:  []coordinator.RuntimeRegistration{{Info: info, Provider: provider, Comparator: provider}},
		Workloads: registrations,
		Resolver:  dockerEnvironmentResolver{},
		Binder: binding.BindingPlanBinder{
			Handlers:  handlers,
			Validator: binding.DesiredPlanValidator{RuntimeKind: info.Kind, Objects: provider},
			Lowerer:   provider,
			Workloads: containerAssembler{},
		},
	}, nil
}

type dockerEnvironmentResolver struct{}

func (dockerEnvironmentResolver) Resolve(ctx context.Context, request environment.ResolveRequest) (environment.ResolvedEnvironment, error) {
	if err := ctx.Err(); err != nil {
		return environment.ResolvedEnvironment{}, err
	}
	if request.Runtime.Kind != RuntimeKind || !request.Requirements.Empty() || len(request.Targets) != 0 {
		return environment.ResolvedEnvironment{}, fault.New(fault.InvalidArgument, "Docker environment injection is unsupported; use explicit container fields")
	}
	return environment.ResolvedEnvironment{Task: request.Task}, nil
}

type containerAssembler struct{}

func (containerAssembler) Assemble(ctx context.Context, _ binding.BindRequest, plan *executionplan.Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan == nil || len(plan.Objects) != 1 || len(plan.Targets) != 0 || len(plan.Units) != 0 || len(plan.Processes) != 0 || !plan.Environment.Empty() {
		return fault.New(fault.InvalidArgument, "Docker requires exactly one precompiled container without external facilities")
	}
	return nil
}
