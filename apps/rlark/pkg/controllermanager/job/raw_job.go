package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rawruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/rawallocation"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// RawAllocationFinalizer retains the Task identity until its exact reservation
	// is released. It is independent from the Agent's process cleanup finalizer.
	RawAllocationFinalizer = "raw.rlark.io/allocation-cleanup"
	rawDesiredAnnotation   = "raw.rlark.io/desired-digest"
	rawRetry               = 5 * time.Second
)

func hasRawTemplates(job *api.Job) bool {
	for _, t := range job.Spec.Tasks {
		if t.Runtime.Kind == "Raw" {
			return true
		}
	}
	return false
}

func validateRawJob(job *api.Job) error {
	if tenant.IDOf(job) == "" || job.UID == "" {
		return errors.New("Raw Job requires tenant identity and object UID")
	}
	if len(job.Spec.Tasks) != 1 || job.Spec.Tasks[0].Runtime.Kind != "Raw" {
		return errors.New("Raw preview supports exactly one process template; mixed runtimes and multiple nodes are unsupported")
	}
	t := job.Spec.Tasks[0]
	if len(validation.IsDNS1123Label(t.Name)) != 0 || t.Placement.Environment == "" || len(t.Placement.NodeSelector) == 0 {
		return errors.New("Raw template requires a DNS label name, explicit environment and exact-match nodeSelector")
	}
	if _, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: t.Placement.NodeSelector}); err != nil {
		return fmt.Errorf("invalid Raw selector: %w", err)
	}
	if t.Placement.Binding != nil || t.Runtime.Name != "" && t.Runtime.Name != "raw-node" || t.Head || t.Suspended || job.Spec.Domain != "" || job.Spec.SSHPublicKey != "" || len(t.Environments) != 0 || t.Workload.Template != nil {
		return errors.New("Raw process preview does not support user bindings, Ray heads, per-template suspension, Pod templates, network or SSH environment injection")
	}
	if string(t.Workload.Kind) != string(rawruntime.ProcessWorkload) || t.Workload.Version != "v1" || t.Workload.Carrier.Type != string(rawruntime.ProcessWorkload) || t.Workload.Carrier.Version != "v1" || len(t.Workload.Carrier.Parameters.Raw) != 0 {
		return errors.New("Raw preview requires raw.rlark.io/process v1 workload and carrier; Ray/RLinf are not implemented")
	}
	spec, err := rawruntime.DecodeProcess(t.Workload.Parameters.Raw)
	if err != nil {
		return err
	}
	for _, v := range spec.Env {
		if strings.HasPrefix(v, "CUDA_VISIBLE_DEVICES=") {
			return errors.New("CUDA_VISIBLE_DEVICES is assigned from the authorized node GPUs")
		}
	}
	return nil
}

func rawDesiredDigest(job *api.Job) string {
	data, _ := json.Marshal(struct {
		Tasks   []api.JobTaskTemplate
		Tags    []api.JobTag
		Restart string
	}{job.Spec.Tasks, job.Spec.Tags, job.Annotations[RestartedAtAnnotation]}) // Typed resource values are JSON serializable.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reconcileRawJob is a direct-client reconciliation path. A Task with our
// finalizer is created before reserving a node, so every partial write retains
// a durable execution identity. No cache participates in allocation decisions.
func (r *Reconciler) reconcileRawJob(ctx context.Context, key client.ObjectKey) (ctrl.Result, error) {
	if r.RawClient == nil {
		return ctrl.Result{}, errors.New("uncached Raw management client required")
	}
	c := r.RawClient
	var job api.Job
	if err := c.Get(ctx, key, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if job.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&job, CleanupFinalizer) {
		before := job.DeepCopy()
		controllerutil.AddFinalizer(&job, CleanupFinalizer)
		return ctrl.Result{RequeueAfter: rawRetry}, c.Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	before := job.DeepCopy()
	var list api.TaskList
	if err := c.List(ctx, &list, client.MatchingLabels{jobLabel: job.Name}); err != nil {
		return ctrl.Result{}, err
	}
	owned := []*api.Task{}
	for idx := range list.Items {
		t := &list.Items[idx]
		owner := metav1.GetControllerOf(t)
		if owner == nil || owner.UID != job.UID {
			return ctrl.Result{}, fmt.Errorf("Task %s/%s has conflicting Job ownership", t.Namespace, t.Name)
		}
		if owner.Kind != "Job" || owner.APIVersion != api.GroupVersion.String() || t.Annotations[utils.ParentUIDAnnotation] != string(job.UID) || tenant.IDOf(t) != tenant.IDOf(&job) {
			return ctrl.Result{}, errors.New("Raw Task ownership or tenant conflict")
		}
		owned = append(owned, t)
	}
	validationErr := validateRawJob(&job)
	digest := rawDesiredDigest(&job)
	pending := false
	for _, task := range owned {
		if task.Spec.Runtime.Kind != "Raw" {
			return ctrl.Result{}, errors.New("cannot adopt an existing non-Raw Task into a Raw Job")
		}
		remove := !job.DeletionTimestamp.IsZero() || validationErr != nil || task.Annotations[rawDesiredAnnotation] != digest || (!job.Spec.Suspended && task.Spec.Suspended)
		if remove && task.DeletionTimestamp.IsZero() {
			if err := c.Delete(ctx, task, client.Preconditions{UID: &task.UID, ResourceVersion: &task.ResourceVersion}); err != nil {
				return ctrl.Result{}, err
			}
			pending = true
			continue
		}
		if !task.DeletionTimestamp.IsZero() {
			pending = true
			// New effects cannot begin after deletion. The Agent persists its own
			// finalizer before any effect and removes it only after supervised cleanup.
			if controllerutil.ContainsFinalizer(task, host.TaskFinalizer) {
				continue
			}
			if err := r.releaseRawTask(ctx, &job, task); err != nil {
				return ctrl.Result{}, err
			}
			if controllerutil.ContainsFinalizer(task, RawAllocationFinalizer) {
				original := task.DeepCopy()
				controllerutil.RemoveFinalizer(task, RawAllocationFinalizer)
				if err := c.Patch(ctx, task, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
					return ctrl.Result{}, err
				}
			}
			continue
		}
		if job.Spec.Suspended && !task.Spec.Suspended {
			original := task.DeepCopy()
			task.Spec.Suspended = true
			if err := c.Patch(ctx, task, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
			pending = true
			continue
		}
		cleaned := rawTaskCleaned(task)
		if cleaned {
			if err := r.releaseRawTask(ctx, &job, task); err != nil {
				return ctrl.Result{}, err
			}
		} else if task.Spec.Suspended {
			pending = true
			// Unbound Tasks cannot execute, and can be removed without waiting for an
			// Agent that has never been authorized to see them.
			if task.Spec.Placement.Binding == nil {
				if err := c.Delete(ctx, task, client.Preconditions{UID: &task.UID, ResourceVersion: &task.ResourceVersion}); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else if !taskTerminal(task) {
			if err := r.bindRawTask(ctx, &job, task); err != nil {
				if !errors.Is(err, rawallocation.ErrUnavailable) {
					return ctrl.Result{}, err
				}
				pending = true
			}
		} else {
			pending = true
		}
	}
	if len(owned) == 0 && job.DeletionTimestamp.IsZero() && !job.Spec.Suspended && validationErr == nil {
		if err := r.createRawTask(ctx, &job, digest); err != nil {
			if !errors.Is(err, rawallocation.ErrUnavailable) {
				return ctrl.Result{}, err
			}
		}
		pending = true
	}
	if !job.DeletionTimestamp.IsZero() {
		if len(owned) > 0 {
			return ctrl.Result{RequeueAfter: rawRetry}, nil
		}
		controllerutil.RemoveFinalizer(&job, CleanupFinalizer)
		return ctrl.Result{}, c.Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if validationErr != nil {
		markJobInvalid(&job, validationErr.Error())
		apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{Type: jobConditionValidated, Status: metav1.ConditionFalse, Reason: "UnsupportedRawJob", Message: validationErr.Error(), ObservedGeneration: job.Generation})
	} else {
		markJobValid(&job)
		job.Status.Phase = api.JobPhasePending
		if job.Spec.Suspended && !pending {
			job.Status.Phase = api.JobPhaseStopped
		}
		job.Status.Tasks = nil
		if len(owned) == 0 {
			job.Status.StartTime = nil
		}
		if len(owned) == 1 {
			task := owned[0]
			job.Status.Tasks = []api.JobTaskStatus{{Name: job.Spec.Tasks[0].Name, Phase: task.Status.Phase, Message: task.Status.Message}}
			if task.Status.ObservedGeneration == task.Generation && !pending && rawTaskCleaned(task) {
				job.Status.Phase = api.JobPhase(task.Status.Phase)
			} else if task.Status.Phase == api.TaskPhaseRunning && task.Status.ObservedGeneration == task.Generation && !pending {
				job.Status.Phase = api.JobPhaseRunning
			}
		}
		scheduled := len(owned) == 1 && owned[0].Spec.Placement.Binding != nil && !pending
		state, reason := metav1.ConditionFalse, "WaitingForRawNode"
		if scheduled {
			state, reason = metav1.ConditionTrue, "RawNodeBound"
		}
		apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{Type: "Scheduled", Status: state, Reason: reason, ObservedGeneration: job.Generation})
		if job.Status.Phase == api.JobPhaseRunning && job.Status.StartTime == nil {
			now := metav1.Now()
			job.Status.StartTime = &now
		}
		if job.Status.Phase == api.JobPhaseSucceeded || job.Status.Phase == api.JobPhaseFailed || job.Status.Phase == api.JobPhaseStopped {
			if job.Status.EndTime == nil {
				now := metav1.Now()
				job.Status.EndTime = &now
			}
		} else {
			job.Status.EndTime = nil
		}
	}
	job.Status.ObservedGeneration = job.Generation
	if !reflect.DeepEqual(before.Status, job.Status) {
		if err := c.Status().Patch(ctx, &job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Periodic reconciliation handles capacity changes and repairs lost watches.
	return ctrl.Result{RequeueAfter: rawRetry}, nil
}

func rawTaskCleaned(task *api.Task) bool {
	if task.Status.ObservedGeneration != task.Generation {
		return false
	}
	if task.Spec.Suspended && task.Status.Phase == api.TaskPhaseStopped {
		return true
	}
	condition := apimeta.FindStatusCondition(task.Status.Conditions, "ExecutionCleaned")
	return taskTerminal(task) && condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == task.Generation
}

func (r *Reconciler) rawCandidates(ctx context.Context, job *api.Job) ([]api.Node, map[string]api.Cluster, error) {
	var clusters api.ClusterList
	if err := r.RawClient.List(ctx, &clusters); err != nil {
		return nil, nil, err
	}
	byNamespace := map[string]api.Cluster{}
	ambiguous := map[string]bool{}
	for _, c := range clusters.Items {
		if _, ok := byNamespace[c.Spec.AgentNamespace]; ok {
			ambiguous[c.Spec.AgentNamespace] = true
		}
		byNamespace[c.Spec.AgentNamespace] = c
	}
	var nodes api.NodeList
	if err := r.RawClient.List(ctx, &nodes); err != nil {
		return nil, nil, err
	}
	candidates := []api.Node{}
	t := job.Spec.Tasks[0]
	selector := labels.SelectorFromSet(t.Placement.NodeSelector)
	for _, node := range nodes.Items {
		c, ok := byNamespace[node.Namespace]
		if !ok || ambiguous[node.Namespace] || tenant.IDOf(&c) != tenant.IDOf(job) || tenant.IDOf(&node) != tenant.IDOf(job) || !selector.Matches(labels.Set(node.Labels)) || node.Spec.AgentType != api.AgentTypeRaw || node.Status.Raw == nil || node.Status.Raw.Environment != t.Placement.Environment {
			continue
		}
		authorized, err := rawnode.AuthorizedCapability(&node)
		if err != nil || !rawnode.SameCapability(authorized, node.Status.Raw) {
			continue
		}
		candidates = append(candidates, node)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return client.ObjectKeyFromObject(&candidates[i]).String() < client.ObjectKeyFromObject(&candidates[j]).String()
	})
	return candidates, byNamespace, nil
}

func (r *Reconciler) createRawTask(ctx context.Context, job *api.Job, digest string) error {
	candidates, clusters, err := r.rawCandidates(ctx, job)
	if err != nil {
		return err
	}
	for idx := range candidates {
		node := &candidates[idx]
		cluster := clusters[node.Namespace]
		if node.Status.Reservation != nil || cluster.Spec.Cordoned || cluster.Spec.Draining || node.Spec.Unschedulable || node.Status.Phase != api.NodeOnline {
			continue
		}
		task := buildTask(job, job.Spec.Tasks[0], utils.ChildName(job.Name, job.Spec.Tasks[0].Name), node.Namespace)
		task.Annotations[rawDesiredAnnotation] = digest
		task.Finalizers = []string{RawAllocationFinalizer}
		if err := ctrl.SetControllerReference(job, task, r.Scheme); err != nil {
			return err
		}
		if err := r.RawClient.Create(ctx, task); err != nil {
			return err
		}
		return nil
	}
	return rawallocation.ErrUnavailable
}

func (r *Reconciler) bindRawTask(ctx context.Context, job *api.Job, task *api.Task) error {
	if task.Spec.Placement.Binding != nil {
		return nil
	} // Binding never migrates implicitly.
	candidates, clusters, err := r.rawCandidates(ctx, job)
	if err != nil {
		return err
	}
	manager := rawallocation.Manager{Client: r.RawClient}
	var reservations api.NodeList
	if err := r.RawClient.List(ctx, &reservations); err != nil {
		return err
	}
	recovered := []api.Node{}
	for _, node := range reservations.Items {
		if node.Status.Reservation != nil && node.Status.Reservation.ExecutionID == string(task.UID) {
			matched := false
			for _, candidate := range candidates {
				if candidate.UID == node.UID {
					matched = true
				}
			}
			if !matched {
				return rawallocation.ErrUnavailable
			}
			recovered = append(recovered, node)
		}
	}
	if len(recovered) > 1 {
		return errors.New("single-node Raw execution has multiple reservations")
	}
	if len(recovered) == 1 {
		candidates = recovered
	}
	// Recover an earlier reserve/commit/assignment before considering a new node.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].Status.Reservation, candidates[j].Status.Reservation
		return a != nil && a.ExecutionID == string(task.UID) && !(b != nil && b.ExecutionID == string(task.UID))
	})
	for idx := range candidates {
		node := &candidates[idx]
		if node.Namespace != task.Namespace {
			continue
		}
		cluster := clusters[node.Namespace]
		bindings, err := manager.Reserve(ctx, rawallocation.Request{Cluster: client.ObjectKeyFromObject(&cluster), JobUID: string(job.UID), TenantID: tenant.IDOf(job), ExecutionID: string(task.UID), Environment: task.Spec.Placement.Environment, Nodes: []client.ObjectKey{client.ObjectKeyFromObject(node)}})
		if errors.Is(err, rawallocation.ErrUnavailable) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return err
		}
		binding, err := manager.AssignTask(ctx, client.ObjectKeyFromObject(node), bindings[0], tenant.IDOf(job), string(task.UID))
		if err != nil {
			return err
		}
		original := task.DeepCopy()
		task.Spec.Placement.Binding = &binding
		var spec rawruntime.ProcessSpec
		if err := json.Unmarshal(task.Spec.Workload.Parameters.Raw, &spec); err != nil {
			return err
		}
		spec.Env = append(spec.Env, "CUDA_VISIBLE_DEVICES="+strings.Join(node.Status.Raw.AuthorizedGPUUUIDs, ","))
		raw, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		task.Spec.Workload.Parameters.Raw = raw
		return r.RawClient.Patch(ctx, task, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	}
	return rawallocation.ErrUnavailable
}

func (r *Reconciler) releaseRawTask(ctx context.Context, job *api.Job, task *api.Task) error {
	var nodes api.NodeList
	if err := r.RawClient.List(ctx, &nodes, client.InNamespace(task.Namespace)); err != nil {
		return err
	}
	manager := rawallocation.Manager{Client: r.RawClient}
	for idx := range nodes.Items {
		node := &nodes.Items[idx]
		v := node.Status.Reservation
		if v == nil || v.ExecutionID != string(task.UID) {
			continue
		}
		if v.JobUID != string(job.UID) || v.TenantID != tenant.IDOf(job) || v.TaskUID != "" && v.TaskUID != string(task.UID) {
			return errors.New("Raw reservation ownership conflict")
		}
		binding := api.RawTaskBinding{TaskUID: v.TaskUID, NodeName: node.Name, NodeUID: v.NodeUID, Incarnation: v.Incarnation, JobUID: v.JobUID, ExecutionID: v.ExecutionID, AllocationID: v.AllocationID, ClusterUID: v.ClusterUID, GroupDigest: v.GroupDigest}
		if task.Spec.Placement.Binding != nil && *task.Spec.Placement.Binding != binding {
			return errors.New("Raw cleanup binding differs from reservation")
		}
		if string(node.UID) != v.NodeUID || tenant.IDOf(node) != tenant.IDOf(job) || node.Status.Raw == nil || node.Status.Raw.Incarnation != v.Incarnation {
			return errors.New("Raw cleanup node identity conflict")
		}
		if !v.Committed {
			// No Task binding could have been published before commit. Release this
			// never-dispatched partial allocation using an optimistic status patch.
			if task.Spec.Placement.Binding != nil || v.TaskUID != "" {
				return errors.New("uncommitted Raw reservation has an execution binding")
			}
			before := node.DeepCopy()
			node.Status.Reservation = nil
			if err := r.RawClient.Status().Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		} else if err := manager.Release(ctx, client.ObjectKeyFromObject(node), binding, tenant.IDOf(job), true); err != nil {
			return err
		}
	}
	return nil
}
