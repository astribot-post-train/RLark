package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	rawruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/scheduler"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// JobFinalizer protects owned Task identities until their allocations are
// positively cleaned and observed deleted. Native Job finalizers have separate
// ownership; this controller never removes them.
const JobFinalizer = "containers.rlark.io/task-protection"

const scheduleInterval = 5 * time.Second

// JobReservationReconciler materializes one Raw process Task and reserves its
// node. It is opt-in and does not send assignments or start workloads. Client
// must be uncached; current API state and CAS are the concurrency authority.
type JobReservationReconciler struct {
	Client client.Client
	Now    func() time.Time
}

// SetupWithManager installs the opt-in native Job reconciler. Periodic repair
// catches binding, allocation and heartbeat changes without event ordering.
func (r *JobReservationReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).Named("container-job-reservation").For(&native.Job{}).Owns(&native.Task{}).Complete(r)
}

func (r JobReservationReconciler) binding(ctx context.Context, job *native.Job) (*ext.ContainerJob, error) {
	var bindings ext.ContainerJobList
	if err := r.Client.List(ctx, &bindings); err != nil {
		return nil, err
	}
	var result *ext.ContainerJob
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.Job.Name != job.Name || b.Spec.Job.Namespace != job.Namespace {
			continue
		}
		if result != nil {
			return nil, invalidBinding("ambiguous Job binding")
		}
		result = b
	}
	if result == nil || !result.DeletionTimestamp.IsZero() {
		return nil, invalidBinding("Job binding is absent or deleting")
	}
	validator := ContainerJobReconciler{Client: r.Client}
	if err := validator.validateJob(ctx, result); err != nil {
		return nil, err
	}
	var cluster native.Cluster
	if err := getNative(ctx, r.Client, result.Spec.Cluster, &cluster); err != nil {
		return nil, err
	}
	var companions ext.ContainerClusterList
	if err := r.Client.List(ctx, &companions, client.InNamespace(result.Namespace)); err != nil {
		return nil, err
	}
	var cc *ext.ContainerCluster
	for i := range companions.Items {
		if companions.Items[i].Spec.Cluster == result.Spec.Cluster {
			if cc != nil {
				return nil, invalidBinding("ambiguous Cluster binding")
			}
			cc = &companions.Items[i]
		}
	}
	if cc == nil || !cc.DeletionTimestamp.IsZero() || cc.Spec.Runtime != result.Spec.Runtime {
		return nil, invalidBinding("Cluster binding is absent or incompatible")
	}
	if err := (ContainerClusterReconciler{Client: r.Client}).validateCluster(ctx, cc); err != nil {
		return nil, err
	}
	return result, nil
}

func processTemplate(job *native.Job, binding *ext.ContainerJob) error {
	if binding.Spec.Runtime != "Raw" || len(job.Spec.Tasks) != 1 || job.Spec.SSHPublicKey != "" {
		return invalidBinding("preview reserves one Raw process template; SSH and groups are unsupported")
	}
	t := &job.Spec.Tasks[0]
	if t.Name == "" || len(validation.IsDNS1123Subdomain(utils.ChildName(job.Name, t.Name))) != 0 || t.Head || t.Suspended || t.Runtime.Kind != "Raw" || t.Runtime.Name != "" || t.Placement.Environment == "" || t.Placement.Binding != nil || t.Placement.DockerBinding != nil || t.DownstreamName != "" || len(t.Environments) != 0 || t.Workload.Template != nil || string(t.Workload.Kind) != string(rawruntime.ProcessWorkload) || t.Workload.Version != "v1" || t.Workload.Carrier.Type != string(rawruntime.ProcessWorkload) || t.Workload.Carrier.Version != "v1" || len(t.Workload.Carrier.Parameters.Raw) != 0 {
		return invalidBinding("unsupported Raw process template")
	}
	spec, err := rawruntime.DecodeProcess(t.Workload.Parameters.Raw)
	if err != nil {
		return invalidBinding("invalid Raw process parameters")
	}
	if _, err := labels.ValidatedSelectorFromSet(t.Placement.NodeSelector); err != nil {
		return invalidBinding("invalid Node selector")
	}
	for _, env := range spec.Env {
		name, _, _ := strings.Cut(env, "=")
		if name == "CUDA_VISIBLE_DEVICES" || name == "NVIDIA_VISIBLE_DEVICES" {
			return invalidBinding("GPU visibility is assigned by the allocation, not user environment")
		}
	}
	return nil
}

func desiredTask(job *native.Job, binding *ext.ContainerJob) *native.Task {
	template := &job.Spec.Tasks[0]
	task := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: utils.ChildName(job.Name, template.Name), Namespace: binding.Namespace, Annotations: map[string]string{utils.ParentUIDAnnotation: string(job.UID), utils.ChildTemplateAnnotation: template.Name, nativejob.RestartedAtAnnotation: job.Annotations[nativejob.RestartedAtAnnotation]}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, native.GroupVersion.WithKind("Job"))}}, Spec: *template.TaskSpec.DeepCopy()}
	task.Spec.Tags = append(slices.Clone(job.Spec.Tags), task.Spec.Tags...)
	tenant.Inherit(job, task)
	return task
}

func taskOwnedBy(task *native.Task, job *native.Job) bool {
	owner := metav1.GetControllerOf(task)
	return owner != nil && owner.Name == job.Name && owner.UID == job.UID && owner.APIVersion == native.GroupVersion.String() && owner.Kind == "Job" && task.Annotations[utils.ParentUIDAnnotation] == string(job.UID) && task.Annotations[utils.ChildTemplateAnnotation] != "" && tenant.IDOf(task) == tenant.IDOf(job)
}

func (r JobReservationReconciler) children(ctx context.Context, job *native.Job) ([]native.Task, error) {
	var list native.TaskList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	result := []native.Task{}
	for i := range list.Items {
		t := &list.Items[i]
		owner := metav1.GetControllerOf(t)
		if t.Annotations[utils.ParentUIDAnnotation] != string(job.UID) && (owner == nil || owner.UID != job.UID) {
			continue
		}
		if !taskOwnedBy(t, job) {
			return nil, invalidBinding("Task has conflicting ownership or tenant")
		}
		result = append(result, *t)
	}
	return result, nil
}

// retire requests durable stop, then consumes only allocation cleanup proofs.
// A target with no allocation is ambiguous (a Create may still be in flight).
// It intentionally blocks rather than guessing that no process was started.
func (r JobReservationReconciler) retire(ctx context.Context, task *native.Task) (bool, error) {
	if !task.Spec.Suspended && task.DeletionTimestamp.IsZero() {
		before := task.DeepCopy()
		task.Spec.Suspended = true
		if err := r.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
		return false, nil
	}
	target, err := companion.Target(task)
	if err != nil {
		return false, err
	}
	receipt, err := readReceipt(task, target)
	if err != nil {
		return false, err
	}
	var list ext.ContainerAllocationList
	if err := r.Client.List(ctx, &list); err != nil {
		return false, err
	}
	found := false
	for i := range list.Items {
		a := &list.Items[i]
		if a.Spec.Task.UID != string(task.UID) {
			continue
		}
		if target == nil || !reflect.DeepEqual(a.Spec, *target) || tenant.IDOf(a) != tenant.IDOf(task) {
			return false, invalidBinding("Task allocation does not match its durable target")
		}
		owner := metav1.GetControllerOf(a)
		if a.UID == "" || a.Namespace != task.Namespace || a.Name != companion.Name(a.Spec.Node.UID) || owner == nil || owner.UID != task.UID || owner.Name != task.Name || owner.Kind != "Task" || owner.APIVersion != native.GroupVersion.String() {
			return false, invalidBinding("allocation has conflicting Task ownership")
		}
		found = true
		if receipt != nil && receipt.Allocation.UID != string(a.UID) {
			return false, invalidBinding("allocation UID differs from saved cleanup receipt")
		}
		if !cleanupProven(a) {
			return false, nil
		}
		if err := r.saveReceipt(ctx, task, a); err != nil {
			return false, err
		}
		if err := (companion.Manager{Client: r.Client}).Release(ctx, client.ObjectKeyFromObject(a), string(a.UID)); err != nil {
			if errors.Is(err, companion.ErrUnavailable) {
				return false, nil
			}
			return false, err
		}
	}
	if target != nil && !found && receipt == nil {
		return false, nil
	}
	if controllerutil.ContainsFinalizer(task, companion.TaskFinalizer) {
		before := task.DeepCopy()
		controllerutil.RemoveFinalizer(task, companion.TaskFinalizer)
		if err := r.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	if task.DeletionTimestamp.IsZero() {
		if err := r.Client.Delete(ctx, task, client.Preconditions{UID: &task.UID, ResourceVersion: &task.ResourceVersion}); err != nil {
			return false, client.IgnoreNotFound(err)
		}
	}
	var current native.Task
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	return current.UID != task.UID, nil
}

func (r JobReservationReconciler) report(ctx context.Context, job *native.Job, reason, message string, scheduled bool) (ctrl.Result, error) {
	before := job.DeepCopy()
	job.Status.Phase = native.JobPhasePending
	job.Status.Tasks = nil
	if reason == "Stopped" {
		job.Status.Phase = native.JobPhaseStopped
	}
	job.Status.ObservedGeneration = job.Generation
	condition(&job.Status.Conditions, job.Generation, "Scheduled", scheduled, reason, message)
	if !reflect.DeepEqual(before.Status, job.Status) {
		if err := r.Client.Status().Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: scheduleInterval}, nil
}

// Reconcile reserves the exact current Job execution without creating runtime
// effects. Stop, invalid configuration and deletion all retire previous Tasks.
func (r JobReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, errors.New("uncached Job scheduling client is required")
	}
	var job native.Job
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned := job.Annotations[nativejob.ControllerOwnerAnnotation] == ext.Group
	if !owned && !controllerutil.ContainsFinalizer(&job, JobFinalizer) {
		return ctrl.Result{}, nil
	}
	if job.UID == "" || job.Generation < 1 {
		return ctrl.Result{}, invalidBinding("persisted Job identity required")
	}
	binding, validationErr := r.binding(ctx, &job)
	if validationErr == nil {
		validationErr = processTemplate(&job, binding)
	}
	if validationErr != nil && !errors.Is(validationErr, errInvalidBinding) && !apierrors.IsNotFound(validationErr) {
		return ctrl.Result{}, validationErr
	}
	if !controllerutil.ContainsFinalizer(&job, JobFinalizer) && validationErr == nil && job.DeletionTimestamp.IsZero() {
		before := job.DeepCopy()
		controllerutil.AddFinalizer(&job, JobFinalizer)
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Client.Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	children, err := r.children(ctx, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	var allocations ext.ContainerAllocationList
	if err := r.Client.List(ctx, &allocations); err != nil {
		return ctrl.Result{}, err
	}
	for _, a := range allocations.Items {
		if a.Spec.Job.UID != string(job.UID) {
			continue
		}
		hasTask := false
		for _, task := range children {
			if a.Spec.Task.UID == string(task.UID) {
				hasTask = true
			}
		}
		if !hasTask {
			return r.report(ctx, &job, "CleanupPending", "An allocation has lost its Task identity; explicit recovery is required", false)
		}
	}
	stopping := !owned || validationErr != nil || job.Spec.Suspended || !job.DeletionTimestamp.IsZero()
	var desired *native.Task
	if !stopping {
		desired = desiredTask(&job, binding)
	}
	for i := range children {
		task := &children[i]
		matches := desired != nil && task.Name == desired.Name && task.Namespace == desired.Namespace && taskcontract.EqualSpec(task.Spec, desired.Spec) && task.Annotations[utils.ChildTemplateAnnotation] == desired.Annotations[utils.ChildTemplateAnnotation] && task.Annotations[nativejob.RestartedAtAnnotation] == desired.Annotations[nativejob.RestartedAtAnnotation] && task.DeletionTimestamp.IsZero()
		if matches {
			continue
		}
		done, err := r.retire(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return r.report(ctx, &job, "CleanupPending", "Previous Task identities and allocations are retained until cleanup is proven", false)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if stopping {
		var allocations ext.ContainerAllocationList
		if err := r.Client.List(ctx, &allocations); err != nil {
			return ctrl.Result{}, err
		}
		for _, a := range allocations.Items {
			if a.Spec.Job.UID == string(job.UID) {
				return r.report(ctx, &job, "CleanupPending", "An allocation still refers to this Job instance; Task identity recovery is required", false)
			}
		}
		if !job.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(&job, JobFinalizer) {
			before := job.DeepCopy()
			controllerutil.RemoveFinalizer(&job, JobFinalizer)
			return ctrl.Result{}, r.Client.Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		if validationErr != nil {
			return r.report(ctx, &job, "UnsupportedOrInvalid", "Container Job binding or process template is invalid; no new allocation is allowed", false)
		}
		return r.report(ctx, &job, "Stopped", "No owned Tasks or allocations remain", false)
	}
	if len(children) == 0 {
		var collision native.Task
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), &collision); err == nil {
			return r.report(ctx, &job, "OwnershipConflict", "The deterministic Task name is occupied by an object with unproven ownership", false)
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.Client.Create(ctx, desired); err != nil {
			// Never adopt an object by deterministic name; the next reconciliation
			// inspects exact ownership before changing anything.
			return ctrl.Result{}, fmt.Errorf("create owned container Task: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if len(children) != 1 {
		return ctrl.Result{}, invalidBinding("ambiguous owned Tasks")
	}
	planner := scheduler.Planner{Allocator: companion.Manager{Client: r.Client, Now: r.Now}}
	_, err = planner.Acquire(ctx, scheduler.Request{Binding: client.ObjectKeyFromObject(binding), Task: client.ObjectKeyFromObject(&children[0])})
	if err != nil {
		if errors.Is(err, scheduler.ErrNoCandidate) || errors.Is(err, companion.ErrUnavailable) {
			return r.report(ctx, &job, "WaitingForCapacity", "No eligible free node is available; existing targets are retained", false)
		}
		return ctrl.Result{}, err
	}
	return r.report(ctx, &job, "ReservedAwaitingDispatch", "Task and exclusive node allocation are persisted; Agent workload dispatch is not implemented", true)
}
