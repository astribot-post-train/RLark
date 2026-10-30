package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/common"
	dockerallocation "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/dockerallocation"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const DockerAllocationFinalizer = "docker.rlark.io/allocation-cleanup"

func hasDockerTemplates(job *api.Job) bool {
	for _, t := range job.Spec.Tasks {
		if t.Runtime.Kind == api.RuntimeKind("Docker") {
			return true
		}
	}
	return false
}

func validateDockerJob(job *api.Job) error {
	if job.UID == "" || tenant.IDOf(job) == "" {
		return errors.New("Docker Job requires tenant identity and object UID")
	}
	headCount := 0
	for i := range job.Spec.Tasks {
		t := &job.Spec.Tasks[i]
		path := fmt.Sprintf("spec.tasks[%d]", i)
		if len(validation.IsDNS1123Label(t.Name)) != 0 || t.Runtime.Kind != api.RuntimeKind("Docker") || t.Runtime.Name != "" && t.Runtime.Name != "docker" {
			return fmt.Errorf("%s must use the Docker runtime and a DNS label name", path)
		}
		if t.Workload.Kind != taskapi.WorkloadTypeRay || t.Workload.Version != taskapi.WorkloadVersionRayV1 || t.Workload.Carrier.Type != taskapi.CarrierTypeDockerContainer || t.Workload.Carrier.Version != "v1" || t.Workload.Template == nil {
			return fmt.Errorf("%s requires rlinf.io/ray v1 with the Docker container carrier and template", path)
		}
		if t.Placement.Binding != nil || t.Placement.DockerBinding != nil || len(t.Environments) != 0 || job.Spec.Domain != "" || job.Spec.SSHPublicKey != "" {
			return fmt.Errorf("%s Docker placement does not support user bindings or injected environments", path)
		}
		if !t.Workload.Template.Spec.HostNetwork || len(t.Workload.Template.Spec.InitContainers) != 0 || len(t.Workload.Template.Spec.Volumes) != 0 || t.Workload.Template.Spec.ImagePullSecrets != nil || t.Workload.Template.Spec.ServiceAccountName != "" || t.Workload.Template.Spec.HostAliases != nil {
			return fmt.Errorf("%s requires a host-networked single-container template without Kubernetes-only fields", path)
		}
		if len(t.Workload.Template.Spec.Containers) != 1 || strings.TrimSpace(t.Workload.Template.Spec.Containers[0].Image) == "" {
			return fmt.Errorf("%s requires exactly one container with an image", path)
		}
		if _, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: t.Placement.NodeSelector}); err != nil {
			return fmt.Errorf("%s has invalid nodeSelector: %w", path, err)
		}
		replicas := taskReplicas(*t)
		if replicas < 1 || replicas > 1000 {
			return fmt.Errorf("%s replicas must be between 1 and 1000", path)
		}
		if t.Head {
			headCount++
			if replicas != 1 {
				return fmt.Errorf("%s Docker Ray head must have exactly one replica", path)
			}
		}
	}
	if headCount != 1 {
		return errors.New("Docker Ray Job requires exactly one head template")
	}
	return nil
}

type dockerTaskRef struct {
	template api.JobTaskTemplate
	name     string
	index    int32
}

func dockerTaskRefs(job *api.Job) []dockerTaskRef {
	var refs []dockerTaskRef
	for _, t := range job.Spec.Tasks {
		for i := int32(0); i < taskReplicas(t); i++ {
			refs = append(refs, dockerTaskRef{template: t, index: i, name: utils.ChildName(job.Name, fmt.Sprintf("%s-%d", t.Name, i))})
		}
	}
	return refs
}

func dockerTaskResources(task *api.Task) (int64, int64, int64, error) {
	if task.Spec.Workload.Template == nil || len(task.Spec.Workload.Template.Spec.Containers) != 1 {
		return 0, 0, 0, errors.New("Docker Task has no single main container")
	}
	c := task.Spec.Workload.Template.Spec.Containers[0]
	get := func(name corev1.ResourceName) resource.Quantity {
		if q, ok := c.Resources.Requests[name]; ok {
			return q
		}
		return c.Resources.Limits[name]
	}
	cpu, memory, gpu := get(corev1.ResourceCPU), get(corev1.ResourceMemory), get(corev1.ResourceName("nvidia.com/gpu"))
	if cpu.Sign() < 0 || memory.Sign() < 0 || gpu.Sign() < 0 || gpu.Value() != gpu.MilliValue() {
		return 0, 0, 0, errors.New("Docker resources must be non-negative whole GPU requests")
	}
	return cpu.MilliValue(), memory.Value(), gpu.Value(), nil
}

func (r *Reconciler) reconcileDockerJob(ctx context.Context, key client.ObjectKey) (ctrl.Result, error) {
	if r.RawClient == nil {
		return ctrl.Result{}, errors.New("uncached Docker management client required")
	}
	c := r.RawClient
	var job api.Job
	if err := c.Get(ctx, key, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if job.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&job, CleanupFinalizer) {
		before := job.DeepCopy()
		controllerutil.AddFinalizer(&job, CleanupFinalizer)
		return ctrl.Result{Requeue: true}, c.Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if err := validateDockerJob(&job); err != nil && job.DeletionTimestamp.IsZero() {
		before := job.DeepCopy()
		markJobInvalid(&job, err.Error())
		job.Status.ObservedGeneration = job.Generation
		if !reflect.DeepEqual(before.Status, job.Status) {
			if patchErr := c.Status().Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	tasks, err := r.listDockerOwnedTasks(ctx, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !job.DeletionTimestamp.IsZero() {
		return r.cleanupDockerJob(ctx, &job, tasks)
	}
	refs := dockerTaskRefs(&job)
	taskNamespace, err := r.resolveDockerNamespace(ctx, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	byName := map[string]*api.Task{}
	for _, task := range tasks {
		byName[task.Name] = task
	}
	changed := false
	for _, ref := range refs {
		task := byName[ref.name]
		if task == nil {
			task = buildTask(&job, ref.template, ref.name, taskNamespace)
			task.Finalizers = append(task.Finalizers, DockerAllocationFinalizer)
			if err := ctrl.SetControllerReference(&job, task, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}
			if err := c.Create(ctx, task); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
			changed = true
			continue
		}
		if err := ensureDockerTaskOwnership(&job, ref.template.Name, task); err != nil {
			return ctrl.Result{}, err
		}
		if !task.DeletionTimestamp.IsZero() {
			continue
		}
		if task.Spec.Suspended != job.Spec.Suspended && task.Spec.Placement.DockerBinding == nil {
			before := task.DeepCopy()
			task.Spec.Suspended = job.Spec.Suspended
			if err := c.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	// Re-list after creates so UID-bearing Tasks can be allocated in this pass.
	tasks, err = r.listDockerOwnedTasks(ctx, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	byName = make(map[string]*api.Task, len(tasks))
	for _, task := range tasks {
		byName[task.Name] = task
	}
	manager := dockerallocation.Manager{Client: c}
	refByName := make(map[string]dockerTaskRef, len(refs))
	for _, ref := range refs {
		refByName[ref.name] = ref
	}
	var headTask *api.Task
	for _, task := range tasks {
		if ref, ok := refByName[task.Name]; ok && ref.template.Head {
			headTask = task
			break
		}
	}
	headAddress := ""
	if headTask != nil && headTask.Spec.Placement.DockerBinding != nil {
		headAddress, err = r.dockerHeadAddress(ctx, headTask)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].template.Head != refs[j].template.Head {
			return refs[i].template.Head
		}
		return refs[i].name < refs[j].name
	})
	for _, ref := range refs {
		task := byName[ref.name]
		if task == nil || !task.DeletionTimestamp.IsZero() || task.Spec.Suspended || task.Status.Phase == api.TaskPhaseSucceeded || task.Status.Phase == api.TaskPhaseFailed {
			continue
		}
		if !ref.template.Head && headAddress == "" {
			continue
		}
		if !ref.template.Head && task.Spec.Placement.DockerBinding != nil {
			if err := r.ensureDockerHeadAddress(ctx, task, headAddress); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		if task.Spec.Placement.DockerBinding != nil {
			continue
		}
		cpu, memory, gpu, resourceErr := dockerTaskResources(task)
		if resourceErr != nil {
			return ctrl.Result{}, resourceErr
		}
		binding, reserveErr := r.reserveDockerTask(ctx, &job, task, manager, cpu, memory, gpu)
		if errors.Is(reserveErr, dockerallocation.ErrUnavailable) {
			continue
		}
		if reserveErr != nil {
			return ctrl.Result{}, reserveErr
		}
		before := task.DeepCopy()
		task.Spec.Placement.DockerBinding = &binding
		if err := c.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		if ref.template.Head {
			headAddress, err = r.dockerHeadAddress(ctx, task)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		changed = true
	}
	if err := r.updateDockerJobStatus(ctx, &job, tasks, changed); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *Reconciler) resolveDockerNamespace(ctx context.Context, job *api.Job) (string, error) {
	var clusters api.ClusterList
	if err := r.RawClient.List(ctx, &clusters); err != nil {
		return "", err
	}
	var nodes api.NodeList
	if err := r.RawClient.List(ctx, &nodes); err != nil {
		return "", err
	}
	counts := make(map[string]int)
	for _, cluster := range clusters.Items {
		if cluster.Spec.AgentNamespace != "" && tenant.IDOf(&cluster) == tenant.IDOf(job) {
			counts[cluster.Spec.AgentNamespace]++
		}
	}
	sort.Slice(clusters.Items, func(i, j int) bool { return clusters.Items[i].Name < clusters.Items[j].Name })
	for _, cluster := range clusters.Items {
		namespace := cluster.Spec.AgentNamespace
		if namespace == "" || counts[namespace] != 1 || tenant.IDOf(&cluster) != tenant.IDOf(job) || cluster.Spec.Cordoned || cluster.Spec.Draining || !cluster.DeletionTimestamp.IsZero() {
			continue
		}
		eligible := true
		for _, template := range job.Spec.Tasks {
			selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: template.Placement.NodeSelector})
			if err != nil {
				return "", fmt.Errorf("invalid Docker node selector for template %q: %w", template.Name, err)
			}
			found := false
			for i := range nodes.Items {
				node := &nodes.Items[i]
				if node.Namespace != namespace || !node.DeletionTimestamp.IsZero() || node.Spec.AgentType != api.AgentTypeDocker || node.Spec.Unschedulable || node.Status.Phase != api.NodeOnline || node.Status.NodeInfo.Incarnation == "" || tenant.IDOf(node) != tenant.IDOf(job) || !selector.Matches(labels.Set(node.Labels)) {
					continue
				}
				found = true
				break
			}
			if !found {
				eligible = false
				break
			}
		}
		if eligible {
			return cluster.Spec.AgentNamespace, nil
		}
	}
	return "", dockerallocation.ErrUnavailable
}

func (r *Reconciler) dockerHeadAddress(ctx context.Context, task *api.Task) (string, error) {
	binding := task.Spec.Placement.DockerBinding
	if binding == nil {
		return "", dockerallocation.ErrUnavailable
	}
	var node api.Node
	if err := r.RawClient.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: binding.NodeName}, &node); err != nil {
		return "", err
	}
	if err := dockerallocation.ValidateBinding(&node, binding, tenant.IDOf(task)); err != nil {
		return "", err
	}
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			ip := net.ParseIP(address.Address)
			if ip != nil && ip.IsPrivate() {
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("Docker Ray head Node %s has no private InternalIP", node.Name)
}

func (r *Reconciler) ensureDockerHeadAddress(ctx context.Context, task *api.Task, address string) error {
	if strings.TrimSpace(address) == "" {
		return dockerallocation.ErrUnavailable
	}
	params, err := taskapi.DecodeParametersStrict[taskapi.RayExecutionParametersV1](task.Spec.Workload.Parameters)
	if err != nil {
		return fmt.Errorf("decode Docker Ray parameters for Task %s: %w", task.Name, err)
	}
	if params.HeadAddress == address {
		return nil
	}
	params.HeadAddress = address
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	before := task.DeepCopy()
	task.Spec.Workload.Parameters = apiextensionsv1.JSON{Raw: raw}
	return r.RawClient.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) listDockerOwnedTasks(ctx context.Context, job *api.Job) ([]*api.Task, error) {
	var list api.TaskList
	if err := r.RawClient.List(ctx, &list, client.MatchingLabels{jobLabel: job.Name}); err != nil {
		return nil, err
	}
	result := make([]*api.Task, 0, len(list.Items))
	for i := range list.Items {
		t := &list.Items[i]
		owner := metav1.GetControllerOf(t)
		if owner == nil || owner.UID != job.UID || owner.Kind != "Job" || tenant.IDOf(t) != tenant.IDOf(job) {
			return nil, fmt.Errorf("Docker Task %s/%s ownership conflict", t.Namespace, t.Name)
		}
		if t.Spec.Runtime.Kind != api.RuntimeKind("Docker") {
			return nil, errors.New("Docker Job owns a non-Docker Task")
		}
		result = append(result, t)
	}
	return result, nil
}

func ensureDockerTaskOwnership(job *api.Job, template string, task *api.Task) error {
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.UID != job.UID || task.Annotations[utils.ParentUIDAnnotation] != string(job.UID) || task.Annotations[utils.ChildTemplateAnnotation] != template || tenant.IDOf(task) != tenant.IDOf(job) {
		return errors.New("Docker Task ownership conflict")
	}
	return nil
}

func (r *Reconciler) reserveDockerTask(ctx context.Context, job *api.Job, task *api.Task, manager dockerallocation.Manager, cpu, memory, gpu int64) (api.DockerTaskBinding, error) {
	var clusters api.ClusterList
	if err := manager.Client.List(ctx, &clusters); err != nil {
		return api.DockerTaskBinding{}, err
	}
	sort.Slice(clusters.Items, func(i, j int) bool { return clusters.Items[i].Name < clusters.Items[j].Name })
	for _, cluster := range clusters.Items {
		if cluster.Spec.AgentNamespace != task.Namespace {
			continue
		}
		binding, err := manager.Reserve(ctx, dockerallocation.Request{Cluster: client.ObjectKeyFromObject(&cluster), JobUID: string(job.UID), TaskUID: string(task.UID), TenantID: tenant.IDOf(job), NodeSelector: task.Spec.Placement.NodeSelector, CPUMilli: cpu, MemoryBytes: memory, GPUCount: gpu})
		if err == nil {
			return binding, nil
		}
		if !errors.Is(err, dockerallocation.ErrUnavailable) {
			return api.DockerTaskBinding{}, err
		}
	}
	return api.DockerTaskBinding{}, dockerallocation.ErrUnavailable
}

func (r *Reconciler) cleanupDockerJob(ctx context.Context, job *api.Job, tasks []*api.Task) (ctrl.Result, error) {
	pending := false
	manager := dockerallocation.Manager{Client: r.RawClient}
	for _, task := range tasks {
		if task.DeletionTimestamp.IsZero() {
			pending = true
			if !task.Spec.Suspended {
				before := task.DeepCopy()
				task.Spec.Suspended = true
				if err := r.RawClient.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return ctrl.Result{}, err
				}
			}
			continue
		}
		if controllerutil.ContainsFinalizer(task, common.TaskAgentCleanupFinalizer) {
			pending = true
			continue
		}
		if binding := task.Spec.Placement.DockerBinding; binding != nil {
			if err := manager.ReleaseAt(ctx, client.ObjectKey{Namespace: task.Namespace, Name: binding.NodeName}, *binding, tenant.IDOf(job)); err != nil {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.ContainsFinalizer(task, DockerAllocationFinalizer) {
			before := task.DeepCopy()
			controllerutil.RemoveFinalizer(task, DockerAllocationFinalizer)
			if err := r.RawClient.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if pending || len(tasks) != 0 {
		return ctrl.Result{RequeueAfter: cleanupRequeue}, nil
	}
	before := job.DeepCopy()
	controllerutil.RemoveFinalizer(job, CleanupFinalizer)
	return ctrl.Result{}, r.RawClient.Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) updateDockerJobStatus(ctx context.Context, job *api.Job, tasks []*api.Task, changed bool) error {
	before := job.DeepCopy()
	job.Status.Tasks = make([]api.JobTaskStatus, 0, len(tasks))
	phase := api.JobPhasePending
	allSucceeded := len(tasks) > 0
	anyRunning := false
	anyFailed := false
	for _, task := range tasks {
		job.Status.Tasks = append(job.Status.Tasks, api.JobTaskStatus{Name: task.Name, Phase: task.Status.Phase, Message: task.Status.Message})
		switch task.Status.Phase {
		case api.TaskPhaseRunning:
			anyRunning = true
			allSucceeded = false
		case api.TaskPhaseFailed:
			anyFailed = true
			allSucceeded = false
		case api.TaskPhaseSucceeded:
		case api.TaskPhaseStopped:
			allSucceeded = false
		default:
			allSucceeded = false
		}
	}
	if anyFailed {
		phase = api.JobPhaseFailed
	} else if allSucceeded {
		phase = api.JobPhaseSucceeded
	} else if anyRunning {
		phase = api.JobPhaseRunning
	}
	job.Status.Phase = phase
	job.Status.ObservedGeneration = job.Generation
	state, reason := metav1.ConditionFalse, "WaitingForDockerNode"
	if len(tasks) > 0 {
		bound := true
		for _, task := range tasks {
			if task.Spec.Placement.DockerBinding == nil {
				bound = false
				break
			}
		}
		if bound {
			state, reason = metav1.ConditionTrue, "DockerNodesBound"
		}
	}
	apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{Type: "Scheduled", Status: state, Reason: reason, ObservedGeneration: job.Generation})
	if phase == api.JobPhaseRunning && job.Status.StartTime == nil {
		now := metav1.Now()
		job.Status.StartTime = &now
	}
	if phase == api.JobPhaseSucceeded || phase == api.JobPhaseFailed {
		if job.Status.EndTime == nil {
			now := metav1.Now()
			job.Status.EndTime = &now
		}
	}
	if !reflect.DeepEqual(before.Status, job.Status) || changed {
		return r.RawClient.Status().Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}
