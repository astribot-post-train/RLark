package raw

import (
	"context"
	"errors"
	"fmt"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination/coordinator"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const processCarrier = "raw.rlark.io/process"

// NewHost assembles node-scoped claims, immutable decoding, Coordinator execution
// and management status. It starts no watchers and issues no credentials. The
// caller must separately enforce server-side node authorization before use.
func NewHost(ctx context.Context, management client.Client, admission NodeAdmission, provider *Provider, adapters []workload.Adapter) (*host.Host, error) {
	if management == nil || admission.Client == nil || admission.Node.Namespace == "" || admission.Node.Name == "" || admission.NodeUID == "" || admission.Incarnation == "" || admission.TenantID == "" || provider == nil {
		return nil, errors.New("management client and complete Raw node identity required")
	}
	// Freeze the actual admission boundary rather than trust an unrelated caller
	// supplied Provider admission. The constructor does not mutate its argument.
	local := *provider
	local.Admission = admission
	execution, err := NewCoordinator(ctx, &local, adapters)
	if err != nil {
		return nil, err
	}
	registrations := make([]host.WorkloadRegistration, 0, len(execution.Workloads))
	for _, registration := range execution.Workloads {
		registrations = append(registrations, host.WorkloadRegistration{Key: host.ImplementationKey{WorkloadType: registration.Type, WorkloadVersion: registration.Version, RuntimeKind: registration.RuntimeKind}, Adapter: registration.Adapter})
	}
	implementations, err := host.NewImplementationRegistry(ctx, registrations)
	if err != nil {
		return nil, err
	}
	selector := coordination.RuntimeSelector{ID: local.ID, Kind: "Raw"}
	result := &host.Host{Tasks: management, Status: host.KubernetesStatusWriter{Client: management}, Claimant: admission.Node.Namespace + "/Raw/" + admission.NodeUID, Eligibility: nodeEligibility{admission: admission, runtimeID: local.ID}, Bundle: host.Bundle{RuntimeSelector: selector, Decoder: host.TaskDecoderFunc(decodeTask), Executor: host.CoordinatorExecutor{Coordinator: execution, Selector: selector}, Implementations: implementations, Lifecycle: processLifecycle{}, StatusFromReconcile: processStatus}}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

type nodeEligibility struct {
	admission NodeAdmission
	runtimeID rt.ID
}

func (e nodeEligibility) Matches(task *api.Task) bool {
	b := task.Spec.Placement.Binding
	return task.Namespace == e.admission.Node.Namespace && task.Spec.Runtime.Kind == "Raw" && (task.Spec.Runtime.Name == "" || task.Spec.Runtime.Name == string(e.runtimeID)) && tenant.IDOf(task) == e.admission.TenantID && b != nil && b.TaskUID == string(task.UID) && b.NodeName == e.admission.Node.Name && b.NodeUID == e.admission.NodeUID && b.Incarnation == e.admission.Incarnation
}
func (e nodeEligibility) ValidationMessage(task *api.Task) string {
	if !e.Matches(task) {
		return "Task is not bound to this Raw node"
	}
	_, err := decodeTask(task)
	if err != nil {
		return err.Error()
	}
	return ""
}
func decodeTask(task *api.Task) (coordination.TaskSnapshot, error) {
	if task == nil || task.Spec.Runtime.Kind != "Raw" || task.Spec.Workload.Template != nil || len(task.Spec.Environments) != 0 || task.Spec.Workload.Carrier.Type != processCarrier || task.Spec.Workload.Carrier.Version != "v1" || len(task.Spec.Workload.Carrier.Parameters.Raw) != 0 {
		return coordination.TaskSnapshot{}, fmt.Errorf("raw Task requires the v1 process carrier without Pod template or environment injection")
	}
	identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: task.Namespace, Name: task.Name}, ObjectUID: string(task.UID), Generation: task.Generation}
	if err := validIdentity(identity); err != nil {
		return coordination.TaskSnapshot{}, err
	}
	if task.Spec.Workload.Kind == "" || task.Spec.Workload.Version == "" {
		return coordination.TaskSnapshot{}, errors.New("versioned Raw workload implementation required")
	}
	carrier := coordination.CarrierSelector{Type: processCarrier, Version: "v1"}
	return coordination.TaskSnapshot{Identity: identity, RuntimeSelector: coordination.RuntimeSelector{ID: rt.ID(task.Spec.Runtime.Name), Kind: "Raw"}, WorkloadSelector: coordination.WorkloadSelector{Type: string(task.Spec.Workload.Kind), Version: task.Spec.Workload.Version}, CarrierSelector: carrier, Configuration: coordination.WorkloadConfiguration{Parameters: append([]byte(nil), task.Spec.Workload.Parameters.Raw...), Carrier: coordination.VersionedConfiguration{Type: carrier.Type, Version: carrier.Version}}, Deleting: !task.DeletionTimestamp.IsZero()}, nil
}

type processLifecycle struct{}

func (processLifecycle) BeforeReconcile(context.Context, host.LifecycleInput) (host.LifecycleDecision, error) {
	return host.LifecycleDecision{Action: host.LifecycleProceed}, nil
}
func (processLifecycle) AfterReconcile(context.Context, host.LifecycleInput) (*host.StatusIntent, error) {
	return nil, nil
}

func processStatus(result coordinator.ReconcileResult, snapshot coordination.TaskSnapshot) *host.StatusIntent {
	observation := result.Evaluation.Observation
	phase := api.TaskPhase(observation.Phase)
	known := metav1.ConditionTrue
	reason := "ProcessObserved"
	switch phase {
	case api.TaskPhasePending, api.TaskPhaseRunning, api.TaskPhaseSucceeded, api.TaskPhaseFailed, api.TaskPhaseStopped:
	default:
		phase = api.TaskPhasePending
		known = metav1.ConditionFalse
		reason = "ProcessStateUnknown"
	}
	cleaned := metav1.ConditionFalse
	if phase == api.TaskPhaseSucceeded || phase == api.TaskPhaseFailed {
		cleaned = metav1.ConditionTrue
	}
	conditions := []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionCleaned", Status: cleaned, Reason: "SupervisedProcessObservation", ObservedGeneration: snapshot.Identity.Generation}}}
	conditions = append(conditions, host.ConditionMutation{Condition: metav1.Condition{Type: "ExecutionKnown", Status: known, Reason: reason, Message: observation.Message, ObservedGeneration: snapshot.Identity.Generation}})
	return &host.StatusIntent{TaskUID: snapshot.Identity.ObjectUID, Generation: snapshot.Identity.Generation, Phase: string(phase), MessageSet: true, Message: observation.Message, Priority: host.StatusPriorityWorkload, Conditions: conditions}
}
