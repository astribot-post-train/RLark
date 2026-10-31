package docker

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dockerStatusReason = "DockerContainerObserved"

// NewHost builds the node-scoped Docker Task host. The caller supplies the
// already authenticated management client and the node identity observed by
// the Agent; this constructor has no side effects.
func NewHost(ctx context.Context, management client.Client, admission NodeAdmission, provider *Provider, adapters []workload.Adapter) (*host.Host, error) {
	if management == nil || admission.Client == nil || admission.Node.Namespace == "" || admission.Node.Name == "" || admission.NodeUID == "" || admission.Incarnation == "" || admission.TenantID == "" || provider == nil {
		return nil, errors.New("management client and complete Docker node identity required")
	}
	local := *provider
	local.Admission = admission
	return NewScopedHost(ctx, management, host.KubernetesStatusWriter{Client: management}, admission.Node.Namespace+"/Docker/"+admission.NodeUID, nodeEligibility{admission: admission, runtimeID: local.runtimeID()}, host.TaskDecoderFunc(decodeTask), &local, adapters)
}

// NewScopedHost assembles the Docker Host with the restricted Task transport
// used by a node Agent. It has no Kubernetes API side effects and requires the
// provider admission callback to revalidate every start.
func NewScopedHost(ctx context.Context, tasks host.TaskClient, status host.StatusWriter, claimant string, eligibility host.Eligibility, decoder host.TaskDecoder, provider RuntimeProvider, adapters []workload.Adapter) (*host.Host, error) {
	if tasks == nil || status == nil || claimant == "" || provider == nil {
		return nil, errors.New("scoped Docker tasks, status and provider are required")
	}
	execution, err := NewCoordinator(ctx, provider, adapters)
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
	selector := coordination.RuntimeSelector{ID: provider.runtimeID(), Kind: RuntimeKind}
	result := &host.Host{Tasks: tasks, Status: status, Claimant: claimant, Eligibility: eligibility, Bundle: host.Bundle{RuntimeSelector: selector, Decoder: decoder, Executor: host.CoordinatorExecutor{Coordinator: execution, Selector: selector}, Implementations: implementations, Lifecycle: dockerLifecycle{}, StatusFromReconcile: dockerStatus}}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

// DecodeTask returns a validated Docker Task snapshot without external effects.
func DecodeTask(task *api.Task) (coordination.TaskSnapshot, error) { return decodeTask(task) }

type nodeEligibility struct {
	admission NodeAdmission
	runtimeID rt.ID
}

func (e nodeEligibility) Matches(task *api.Task) bool {
	b := task.Spec.Placement.DockerBinding
	return task.Namespace == e.admission.Node.Namespace && task.Spec.Runtime.Kind == api.RuntimeKind(RuntimeKind) && (task.Spec.Runtime.Name == "" || task.Spec.Runtime.Name == string(e.runtimeID)) && tenant.IDOf(task) == e.admission.TenantID && b != nil && b.TaskUID == string(task.UID) && b.NodeName == e.admission.Node.Name && b.NodeUID == e.admission.NodeUID && b.Incarnation == e.admission.Incarnation
}
func (e nodeEligibility) ValidationMessage(task *api.Task) string {
	if !e.Matches(task) {
		return "Task is not bound to this Docker node"
	}
	_, err := decodeTask(task)
	if err != nil {
		return err.Error()
	}
	return ""
}

func decodeTask(task *api.Task) (coordination.TaskSnapshot, error) {
	if task == nil || task.Spec.Runtime.Kind != api.RuntimeKind(RuntimeKind) || task.Spec.Workload.Carrier.Type != CarrierType || task.Spec.Workload.Carrier.Version != CarrierVersion || len(task.Spec.Workload.Carrier.Parameters.Raw) != 0 || len(task.Spec.Environments) != 0 {
		return coordination.TaskSnapshot{}, fmt.Errorf("Docker Task requires the v1 container carrier")
	}
	identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: task.Namespace, Name: task.Name}, ObjectUID: string(task.UID), Generation: task.Generation}
	if err := identity.Logical.Validate(); err != nil {
		return coordination.TaskSnapshot{}, err
	}
	if identity.ObjectUID == "" {
		return coordination.TaskSnapshot{}, errors.New("Task UID is required")
	}
	if task.Spec.Workload.Kind == "" || task.Spec.Workload.Version == "" {
		return coordination.TaskSnapshot{}, errors.New("versioned Docker workload implementation required")
	}
	carrier := coordination.CarrierSelector{Type: CarrierType, Version: CarrierVersion}
	var parameters []byte
	if task.Spec.Workload.Parameters.Raw != nil {
		parameters = append([]byte(nil), task.Spec.Workload.Parameters.Raw...)
	}
	template := corev1.PodTemplateSpec{}
	if task.Spec.Workload.Template != nil {
		template = *task.Spec.Workload.Template.DeepCopy()
	}
	return coordination.TaskSnapshot{Identity: identity, RuntimeSelector: coordination.RuntimeSelector{ID: rt.ID(task.Spec.Runtime.Name), Kind: RuntimeKind}, WorkloadSelector: coordination.WorkloadSelector{Type: string(task.Spec.Workload.Kind), Version: task.Spec.Workload.Version}, CarrierSelector: carrier, Configuration: coordination.WorkloadConfiguration{Parameters: parameters, Template: template, Carrier: coordination.VersionedConfiguration{Type: carrier.Type, Version: carrier.Version}}, Deleting: !task.DeletionTimestamp.IsZero()}, nil
}

type dockerLifecycle struct{}

func (dockerLifecycle) BeforeReconcile(context.Context, host.LifecycleInput) (host.LifecycleDecision, error) {
	return host.LifecycleDecision{Action: host.LifecycleProceed}, nil
}
func (dockerLifecycle) AfterReconcile(context.Context, host.LifecycleInput) (*host.StatusIntent, error) {
	return nil, nil
}

func dockerStatus(result coordinator.ReconcileResult, snapshot coordination.TaskSnapshot) *host.StatusIntent {
	phase := api.TaskPhase(result.Evaluation.Observation.Phase)
	known := metav1.ConditionTrue
	if phase != api.TaskPhasePending && phase != api.TaskPhaseRunning && phase != api.TaskPhaseSucceeded && phase != api.TaskPhaseFailed && phase != api.TaskPhaseStopped {
		phase, known = api.TaskPhasePending, metav1.ConditionFalse
	}
	cleaned := metav1.ConditionFalse
	// Terminal state does not prove container removal. The scoped Agent writes
	// ExecutionCleaned only after its durable cancellation/removal handshake.
	return &host.StatusIntent{TaskUID: snapshot.Identity.ObjectUID, Generation: snapshot.Identity.Generation, Phase: string(phase), MessageSet: true, Message: result.Evaluation.Observation.Message, Priority: host.StatusPriorityWorkload, Conditions: []host.ConditionMutation{{Condition: metav1.Condition{Type: "ExecutionCleaned", Status: cleaned, Reason: dockerStatusReason, ObservedGeneration: snapshot.Identity.Generation}}, {Condition: metav1.Condition{Type: "ExecutionKnown", Status: known, Reason: dockerStatusReason, Message: result.Evaluation.Observation.Message, ObservedGeneration: snapshot.Identity.Generation}}}}
}
