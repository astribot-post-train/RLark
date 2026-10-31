package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/scheduler"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	raydocker "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// JobFinalizer protects owned Task identities until their allocations are
// positively cleaned and observed deleted. Native Job finalizers have separate
// ownership; this controller never removes them.
const JobFinalizer = "containers.rlark.io/task-protection"

const scheduleInterval = 5 * time.Second

const (
	jobExecutionObservedCondition  = "ExecutionObserved"
	jobAllocationReleasedCondition = "AllocationReleased"
	jobTaskSetRetiringCondition    = taskcontract.TaskSetRetiringCondition
)

// JobReservationReconciler materializes independent Raw process Tasks, reserves
// one node per Task, and aggregates the observed execution lifecycle. It is
// opt-in; runtime effects remain behind the separately scoped Agent API.
// Client must be uncached; current API state and CAS are the concurrency
// authority.
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
	if binding.Spec.Runtime != "Raw" && binding.Spec.Runtime != "Docker" {
		return invalidBinding("only Raw process templates are supported")
	}
	if binding.Spec.Runtime == "Docker" && raydocker.IsJob(job) {
		if _, err := raydocker.Expand(job); err != nil {
			return invalidBinding("invalid Docker Ray group: " + err.Error())
		}
		return nil
	}
	if err := taskcontract.ValidateJob(job); err != nil {
		return invalidBinding("invalid Raw process templates")
	}
	return nil
}

func desiredTasks(job *native.Job, binding *ext.ContainerJob) []*native.Task {
	if binding.Spec.Runtime == "Docker" && raydocker.IsJob(job) {
		members, err := raydocker.Expand(job)
		if err != nil {
			return nil
		}
		return raydocker.Tasks(job, binding.Namespace, members)
	}
	result := make([]*native.Task, 0, len(job.Spec.Tasks))
	for i := range job.Spec.Tasks {
		template := &job.Spec.Tasks[i]
		task := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: utils.ChildName(job.Name, template.Name), Namespace: binding.Namespace, Annotations: map[string]string{utils.ParentUIDAnnotation: string(job.UID), utils.ChildTemplateAnnotation: template.Name, nativejob.RestartedAtAnnotation: job.Annotations[nativejob.RestartedAtAnnotation]}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, native.GroupVersion.WithKind("Job"))}}, Spec: *template.TaskSpec.DeepCopy()}
		task.Spec.Tags = append(slices.Clone(job.Spec.Tags), task.Spec.Tags...)
		tenant.Inherit(job, task)
		result = append(result, task)
	}
	return result
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
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
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
	done, err := r.releaseAllocations(ctx, task)
	if err != nil || !done {
		return false, err
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

// releaseAllocations retains the Task and its allocation finalizer. Natural
// completion keeps that identity so a restart cannot materialize a second run.
func (r JobReservationReconciler) releaseAllocations(ctx context.Context, task *native.Task) (bool, error) {
	if controllerutil.ContainsFinalizer(task, host.TaskFinalizer) {
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
	return true, nil
}

func (r JobReservationReconciler) reportState(ctx context.Context, job *native.Job, phase native.JobPhase, tasks []native.JobTaskStatus, reason, message string, scheduled, observed, released bool) (ctrl.Result, error) {
	before := job.DeepCopy()
	job.Status.Phase = phase
	job.Status.Tasks = tasks
	job.Status.ObservedGeneration = job.Generation
	condition(&job.Status.Conditions, job.Generation, "Scheduled", scheduled, reason, message)
	condition(&job.Status.Conditions, job.Generation, jobExecutionObservedCondition, observed, reason, message)
	condition(&job.Status.Conditions, job.Generation, jobAllocationReleasedCondition, released, reason, message)
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if phase == native.JobPhaseRunning && job.Status.StartTime == nil {
		stamp := metav1.NewTime(now)
		job.Status.StartTime = &stamp
	}
	if phase == native.JobPhaseSucceeded || phase == native.JobPhaseFailed || phase == native.JobPhaseStopped {
		if job.Status.EndTime == nil {
			stamp := metav1.NewTime(now)
			job.Status.EndTime = &stamp
		}
	} else {
		job.Status.EndTime = nil
	}
	if len(tasks) == 0 && phase != native.JobPhaseStopped {
		job.Status.StartTime = nil
	}
	if !reflect.DeepEqual(before.Status, job.Status) {
		if err := r.Client.Status().Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: scheduleInterval}, nil
}

func (r JobReservationReconciler) report(ctx context.Context, job *native.Job, reason, message string, scheduled bool) (ctrl.Result, error) {
	phase := native.JobPhasePending
	if reason == "Stopped" {
		phase = native.JobPhaseStopped
	}
	return r.reportState(ctx, job, phase, nil, reason, message, scheduled, false, reason == "Stopped")
}

// reconcileProcesses handles the multi-template slice after every Task has
// been materialized. It deliberately keeps group semantics runtime-neutral:
// each member is independently admitted and cleaned, while the Job status is
// only an aggregate observation. Ray barriers and cross-member ordering belong
// to a later workload adapter.
func (r JobReservationReconciler) reconcileProcesses(ctx context.Context, job *native.Job, binding *ext.ContainerJob, children []native.Task) (ctrl.Result, error) {
	planner := scheduler.Planner{Allocator: companion.Manager{Client: r.Client, Now: r.Now}}
	statuses := make([]native.JobTaskStatus, 0, len(children))
	anyObserved, anyRunning, anyFailed, anyReserved, waiting := false, false, false, false, false
	allTerminal, allReleased, allSucceeded, allScheduled := true, true, true, true
	for i := range children {
		task := &children[i]
		row := native.JobTaskStatus{Name: task.Annotations[utils.ChildTemplateAnnotation], Phase: native.TaskPhasePending}
		if taskcontract.Observed(task) {
			anyObserved = true
			row.Phase, row.Message = task.Status.Phase, task.Status.Message
			anyRunning = anyRunning || task.Status.Phase == native.TaskPhaseRunning
		}
		statuses = append(statuses, row)
		if taskcontract.HasResult(task) {
			if !taskcontract.Completed(task) {
				allTerminal, allReleased, allSucceeded, allScheduled = false, false, false, false
				continue
			}
			target, err := companion.Target(task)
			if err != nil {
				return ctrl.Result{}, err
			}
			if target == nil || target.Task != (ext.NativeObjectReference{Namespace: task.Namespace, Name: task.Name, UID: string(task.UID)}) || target.Generation != task.Generation || target.Job != (ext.NativeObjectReference{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)}) || target.JobGeneration != job.Generation || target.TenantID != tenant.IDOf(job) {
				return ctrl.Result{}, invalidBinding("terminal Task has no exact allocation target")
			}
			done, err := r.releaseAllocations(ctx, task)
			if err != nil {
				return ctrl.Result{}, err
			}
			allReleased = allReleased && done
			anyReserved = anyReserved || !done
			anyFailed = anyFailed || task.Status.Phase == native.TaskPhaseFailed
			allSucceeded = allSucceeded && task.Status.Phase == native.TaskPhaseSucceeded
			continue
		}
		allTerminal, allReleased, allSucceeded = false, false, false
		_, err := planner.Acquire(ctx, scheduler.Request{Binding: client.ObjectKeyFromObject(binding), Task: client.ObjectKeyFromObject(task)})
		if err != nil {
			if errors.Is(err, scheduler.ErrNoCandidate) || errors.Is(err, companion.ErrUnavailable) {
				waiting, allScheduled = true, false
				// A blocked earlier member must not starve later observation,
				// allocation recovery, or terminal cleanup.
				continue
			}
			return ctrl.Result{}, err
		}
		anyReserved = true
	}
	if allTerminal && allReleased {
		phase := native.JobPhaseSucceeded
		reason := "ExecutionCompleted"
		message := "All Raw replicas succeeded and exact allocation cleanup/deletion are confirmed"
		if anyFailed || !allSucceeded {
			phase, reason, message = native.JobPhaseFailed, "ExecutionFailed", "A Raw replica failed after its exact cleanup and allocation deletion were confirmed"
		}
		return r.reportState(ctx, job, phase, statuses, reason, message, false, true, true)
	}
	phase, reason, message := native.JobPhasePending, "ReservedAwaitingDispatch", "Raw replica Tasks and exclusive node allocations are persisted; awaiting current Agent observations"
	if waiting {
		reason, message = "WaitingForCapacity", "At least one Raw process has no eligible free node; other members continue observation and cleanup"
	}
	if anyRunning {
		phase, reason, message = native.JobPhaseRunning, "ExecutionObserved", "At least one Raw replica is running; all allocations remain protected"
	} else if anyObserved && !waiting {
		reason, message = "ExecutionObserved", "Raw replica execution observations are available; allocations remain protected"
	}
	return r.reportState(ctx, job, phase, statuses, reason, message, allScheduled && anyReserved, anyObserved, false)
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
	var desired []*native.Task
	if !stopping {
		desired = desiredTasks(&job, binding)
	}
	// A changed member or generation retires the whole prior set before any
	// replacement is materialized. Otherwise unchanged siblings could retain an
	// allocation pinned to an old Job generation while new siblings start.
	retirement := meta.FindStatusCondition(job.Status.Conditions, jobTaskSetRetiringCondition)
	retiring := stopping || retirement != nil && retirement.Status == metav1.ConditionTrue
	for i := range children {
		task := &children[i]
		matches := false
		for _, wanted := range desired {
			if task.Name == wanted.Name && task.Namespace == wanted.Namespace && taskcontract.EqualSpec(task.Spec, wanted.Spec) && task.Annotations[utils.ChildTemplateAnnotation] == wanted.Annotations[utils.ChildTemplateAnnotation] && task.Annotations[nativejob.RestartedAtAnnotation] == wanted.Annotations[nativejob.RestartedAtAnnotation] && (task.DeletionTimestamp.IsZero() || taskcontract.Completed(task)) {
				matches = true
			}
		}
		target, err := companion.Target(task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !matches || target != nil && target.JobGeneration != job.Generation {
			retiring = true
		}
	}
	if retiring && len(children) > 0 {
		if retirement == nil || retirement.Status != metav1.ConditionTrue {
			before := job.DeepCopy()
			condition(&job.Status.Conditions, job.Generation, jobTaskSetRetiringCondition, true, "RetiringPreviousTasks", "All previous Task identities must be observed absent before replacement")
			if err := r.Client.Status().Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		if binding != nil && binding.Status.Group != nil {
			g := binding.Status.Group.DeepCopy()
			if g.Phase != "Stopping" && g.Phase != "Succeeded" && g.Phase != "Failed" {
				return r.failRay(ctx, binding, g, "GroupRetired")
			}
			if g.Phase == "Stopping" {
				return r.cleanupRay(ctx, &job, binding, g, children)
			}
		}
		// Request stop for every member before waiting on the first cleanup.
		// This is recoverable fan-out, not ordered by a volatile event queue.
		allDone := true
		var retireErr error
		for i := range children {
			done, err := r.retire(ctx, &children[i])
			retireErr = errors.Join(retireErr, err)
			allDone = allDone && done
		}
		if retireErr != nil {
			return ctrl.Result{}, retireErr
		}
		if !allDone {
			return r.report(ctx, &job, "CleanupPending", "Previous Task identities and allocations are retained until cleanup is proven", false)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if retirement != nil && retirement.Status == metav1.ConditionTrue {
		if binding != nil && binding.Status.Group != nil {
			if err := r.writeGroup(ctx, binding, nil); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		before := job.DeepCopy()
		condition(&job.Status.Conditions, job.Generation, jobTaskSetRetiringCondition, false, "PreviousTasksAbsent", "All previous Task identities are absent")
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Client.Status().Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
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
	for _, wanted := range desired {
		found := false
		for i := range children {
			if children[i].Namespace == wanted.Namespace && children[i].Name == wanted.Name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		var collision native.Task
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(wanted), &collision); err == nil {
			return r.report(ctx, &job, "OwnershipConflict", "The deterministic Task name is occupied by an object with unproven ownership", false)
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.Client.Create(ctx, wanted); err != nil {
			return ctrl.Result{}, fmt.Errorf("create owned container Task: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if len(children) != len(desired) {
		return ctrl.Result{}, invalidBinding("ambiguous owned Tasks")
	}
	if raydocker.IsJob(&job) {
		return r.reconcileRay(ctx, &job, binding, children)
	}
	if len(children) > 1 {
		return r.reconcileProcesses(ctx, &job, binding, children)
	}
	task := &children[0]
	if taskcontract.HasResult(task) {
		if !taskcontract.Completed(task) {
			return r.report(ctx, &job, "ExecutionObservationPending", "Terminal Task observation is stale or lacks current known/cleaned conditions; no new execution is allowed", false)
		}
		target, err := companion.Target(task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if target == nil || target.Task.UID != string(task.UID) || target.Generation != task.Generation || target.Job != (ext.NativeObjectReference{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)}) || target.TenantID != tenant.IDOf(&job) {
			return ctrl.Result{}, invalidBinding("terminal Task has no exact allocation target")
		}
		done, err := r.releaseAllocations(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		phase, reason, message := native.JobPhasePending, "TerminalCleanupPending", "Process result is observed; durable cancellation and allocation deletion are still pending"
		if done {
			phase, reason, message = native.JobPhase(task.Status.Phase), "ExecutionCompleted", "Process result and exact allocation cleanup/deletion are confirmed; Task identity is retained"
		}
		return r.reportState(ctx, &job, phase, []native.JobTaskStatus{{Name: job.Spec.Tasks[0].Name, Phase: task.Status.Phase, Message: task.Status.Message}}, reason, message, !done, true, done)
	}
	planner := scheduler.Planner{Allocator: companion.Manager{Client: r.Client, Now: r.Now}}
	_, err = planner.Acquire(ctx, scheduler.Request{Binding: client.ObjectKeyFromObject(binding), Task: client.ObjectKeyFromObject(&children[0])})
	if err != nil {
		if errors.Is(err, scheduler.ErrNoCandidate) || errors.Is(err, companion.ErrUnavailable) {
			return r.report(ctx, &job, "WaitingForCapacity", "No eligible free node is available; existing targets are retained", false)
		}
		return ctrl.Result{}, err
	}
	if taskcontract.Observed(task) {
		phase := native.JobPhasePending
		if task.Status.Phase == native.TaskPhaseRunning {
			phase = native.JobPhaseRunning
		}
		return r.reportState(ctx, &job, phase, []native.JobTaskStatus{{Name: job.Spec.Tasks[0].Name, Phase: task.Status.Phase, Message: task.Status.Message}}, "ExecutionObserved", "Current Agent execution observation is available; allocation remains protected", true, true, false)
	}
	return r.report(ctx, &job, "ReservedAwaitingDispatch", "Task and exclusive node allocation are persisted; awaiting a current Agent execution observation", true)
}
