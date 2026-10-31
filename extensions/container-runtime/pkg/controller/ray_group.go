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
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	ray "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func groupRef(obj metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

func (r JobReservationReconciler) writeGroup(ctx context.Context, b *ext.ContainerJob, g *ext.JobGroupStatus) error {
	before := b.DeepCopy()
	b.Status.Group = g.DeepCopy()
	if reflect.DeepEqual(before.Status, b.Status) {
		return nil
	}
	return r.Client.Status().Patch(ctx, b, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r JobReservationReconciler) rayPlan(ctx context.Context, job *native.Job, b *ext.ContainerJob, tasks []native.Task) (*ext.JobGroupStatus, error) {
	members, err := ray.Expand(job)
	if err != nil {
		return nil, err
	}
	byName := map[string]*native.Task{}
	for i := range tasks {
		byName[tasks[i].Name] = &tasks[i]
	}
	var inventory ext.ContainerNodeList
	if err := r.Client.List(ctx, &inventory, client.InNamespace(b.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(inventory.Items, func(i, j int) bool { return inventory.Items[i].Name < inventory.Items[j].Name })
	manager := companion.Manager{Client: r.Client, Now: r.Now}
	for _, m := range members {
		task := byName[m.Name]
		if task == nil || !ray.MatchesTask(job, b.Namespace, task) {
			return nil, companion.ErrIdentity
		}
	}
	// Match each candidate network scope independently. A valid placement must
	// put every member on one mutually reachable private network; matching across
	// scopes and rejecting afterward could hide a complete placement in another
	// scope.
	scopes := map[string]bool{}
	for i := range inventory.Items {
		n := &inventory.Items[i]
		if n.Spec.Cluster == b.Spec.Cluster && n.Spec.Runtime == "Docker" && n.Spec.NetworkScope != "" && slices.Contains(n.Status.Capabilities, ray.Capability) {
			scopes[n.Spec.NetworkScope] = true
		}
	}
	orderedScopes := make([]string, 0, len(scopes))
	for scope := range scopes {
		orderedScopes = append(orderedScopes, scope)
	}
	slices.Sort(orderedScopes)
	var chosen []*ext.ContainerNode
	var networkScope string
	for _, scope := range orderedScopes {
		edges := make([][]int, len(members))
		for rank, m := range members {
			task := byName[m.Name]
			for i := range inventory.Items {
				n := &inventory.Items[i]
				if n.Spec.NetworkScope != scope || n.Spec.Cluster != b.Spec.Cluster || n.Spec.Runtime != "Docker" || !slices.Contains(n.Status.Capabilities, ray.Capability) {
					continue
				}
				if _, err := manager.Plan(ctx, companion.Request{Binding: client.ObjectKeyFromObject(b), Node: client.ObjectKeyFromObject(n), Task: client.ObjectKeyFromObject(task)}); err != nil {
					if errors.Is(err, companion.ErrUnavailable) {
						continue
					}
					return nil, err
				}
				if ray.Fits(task, n) == nil {
					edges[rank] = append(edges[rank], i)
				}
			}
		}
		match := make([]int, len(inventory.Items))
		for i := range match {
			match[i] = -1
		}
		var visit func(int, map[int]bool) bool
		visit = func(rank int, seen map[int]bool) bool {
			for _, i := range edges[rank] {
				if seen[i] {
					continue
				}
				seen[i] = true
				if match[i] < 0 || visit(match[i], seen) {
					match[i] = rank
					return true
				}
			}
			return false
		}
		complete := true
		for rank := range members {
			if !visit(rank, map[int]bool{}) {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		candidate := make([]*ext.ContainerNode, len(members))
		for i, rank := range match {
			if rank >= 0 {
				candidate[rank] = &inventory.Items[i]
			}
		}
		chosen, networkScope = candidate, scope
		break
	}
	if len(chosen) != len(members) {
		return nil, companion.ErrUnavailable
	}
	ports := ray.DefaultPortPlan()
	plan := ext.RayGroupPlan{Binding: groupRef(b), Job: groupRef(job), JobGeneration: job.Generation, Restart: job.Annotations[nativejob.RestartedAtAnnotation], Cluster: b.Spec.Cluster, TenantID: tenant.IDOf(job), NetworkScope: networkScope, Ports: ext.RayPortPlan{Base: ports.Base, WorkerCount: ports.WorkerCount}}
	for rank, m := range members {
		n, t := chosen[rank], byName[m.Name]
		gpus := slices.Clone(n.Spec.AuthorizedGPUUUIDs)
		slices.Sort(gpus)
		plan.Members = append(plan.Members, ext.GroupMember{Task: groupRef(t), Generation: t.Generation, Node: n.Spec.Node, NodeRecord: groupRef(n), InstanceID: n.Spec.InstanceID, CredentialID: n.Status.AgentCredentialID, PrivateAddr: n.Spec.PrivateAddress, Rank: int32(rank), GPUUUIDs: gpus})
	}
	if err := ray.ValidatePlan(plan); err != nil {
		return nil, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return &ext.JobGroupStatus{ID: ray.PlanIdentity(plan), Digest: ray.PlanDigest(plan), Plan: plan, Phase: "Allocating", StartedAt: metav1.NewTime(now)}, nil
}

func (r JobReservationReconciler) reconcileRay(ctx context.Context, job *native.Job, b *ext.ContainerJob, tasks []native.Task) (ctrl.Result, error) {
	g := b.Status.Group.DeepCopy()
	if g == nil {
		var err error
		g, err = r.rayPlan(ctx, job, b, tasks)
		if errors.Is(err, companion.ErrUnavailable) {
			return r.report(ctx, job, "WaitingForGroupCapacity", "No complete Ray placement exists; no partial host reservations are taken", false)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.writeGroup(ctx, b, g)
	}
	if ray.ValidatePlan(g.Plan) != nil || g.ID != ray.PlanIdentity(g.Plan) || g.Digest != ray.PlanDigest(g.Plan) || g.Plan.Binding != groupRef(b) {
		return ctrl.Result{}, companion.ErrIdentity
	}
	if g.Phase == "Succeeded" || g.Phase == "Failed" {
		return r.reportState(ctx, job, native.JobPhase(g.Phase), rayRows(tasks), "GroupCompleted", "The exact Ray execution is terminal and will not be restarted by reconciliation", false, true, true)
	}
	if g.Phase == "Stopping" {
		return r.cleanupRay(ctx, job, b, g, tasks)
	}
	if g.Plan.Job != groupRef(job) || g.Plan.JobGeneration != job.Generation || g.Plan.Restart != job.Annotations[nativejob.RestartedAtAnnotation] {
		return r.failRay(ctx, b, g, "JobChanged")
	}
	// An observed head result finishes training; every spontaneous worker exit,
	// including a clean exit, fails the group. Persist outcome before fan-out.
	for i, m := range g.Plan.Members {
		task := rayFind(tasks, m.Task)
		if task == nil || task.Generation != m.Generation || !ray.MatchesTask(job, b.Namespace, task) {
			return r.failRay(ctx, b, g, "MemberIdentityChanged")
		}
		if taskcontract.Observed(task) && taskcontract.HasResult(task) {
			if i == 0 && task.Status.Phase == native.TaskPhaseSucceeded {
				g.Phase, g.Result, g.Reason = "Stopping", "Succeeded", "HeadCompleted"
				return ctrl.Result{RequeueAfter: time.Millisecond}, r.writeGroup(ctx, b, g)
			}
			return r.failRay(ctx, b, g, "MemberExited")
		}
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if !findGroupCondition(g, "GroupContainersRunning") && (g.StartedAt.IsZero() || now.Sub(g.StartedAt.Time) > 10*time.Minute) {
		return r.failRay(ctx, b, g, "GroupStartupTimeout")
	}
	if findGroupCondition(g, "GroupContainersRunning") {
		for _, member := range g.Plan.Members {
			var n ext.ContainerNode
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: member.NodeRecord.Namespace, Name: member.NodeRecord.Name}, &n); err != nil {
				return ctrl.Result{}, err
			}
			if groupRef(&n) != member.NodeRecord || n.Spec.InstanceID != member.InstanceID || n.Annotations[ext.AgentCredentialAnnotation] != member.CredentialID || n.Status.AgentCredentialID != member.CredentialID || n.Status.LastHeartbeatTime == nil || n.Status.LastHeartbeatTime.After(now) || now.Sub(n.Status.LastHeartbeatTime.Time) > 45*time.Second {
				return r.failRay(ctx, b, g, "MemberHeartbeatLost")
			}
		}
	}
	manager := companion.Manager{Client: r.Client, Now: r.Now}
	allocations := make([]*ext.ContainerAllocation, len(g.Plan.Members))
	for i, m := range g.Plan.Members {
		a, err := manager.Acquire(ctx, companion.Request{Binding: client.ObjectKeyFromObject(b), Node: client.ObjectKey{Namespace: m.NodeRecord.Namespace, Name: m.NodeRecord.Name}, Task: client.ObjectKey{Namespace: m.Task.Namespace, Name: m.Task.Name}})
		if err != nil {
			if errors.Is(err, companion.ErrUnavailable) || errors.Is(err, companion.ErrIdentity) {
				return r.failRay(ctx, b, g, "GroupReservationLost")
			}
			// Unknown Create outcomes keep the same target and operation on retry.
			return ctrl.Result{}, err
		}
		allocations[i] = a
	}
	if g.Phase == "Allocating" {
		g.Phase = "Preparing"
		condition(&g.Conditions, job.Generation, "GroupAllocated", true, "AllAllocationsPersisted", "All exact member host locks exist")
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.writeGroup(ctx, b, g)
	}
	prepared := true
	for _, a := range allocations {
		prepared = prepared && ray.PreparedMatches(a)
	}
	if g.Phase == "Preparing" && prepared {
		g.Phase = "Starting"
		condition(&g.Conditions, job.Generation, "GroupPrepared", true, "AllContainersPrepared", "Every member returned an exact creation receipt")
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.writeGroup(ctx, b, g)
	}
	allRunning := true
	for _, m := range g.Plan.Members {
		task := rayFind(tasks, m.Task)
		allRunning = allRunning && taskcontract.Observed(task) && task.Status.Phase == native.TaskPhaseRunning
	}
	phase := native.JobPhasePending
	reason, message := "GroupPreparing", "All containers must be prepared before any start is granted"
	if g.Phase == "Starting" {
		reason, message = "GroupStarting", "All exact receipts are persisted; each start revalidates the complete group"
	}
	if allRunning {
		// Container Running is not Ray membership readiness. The trusted launcher
		// independently waits for the exact GCS member set before training.
		phase, reason, message = native.JobPhaseRunning, "GroupContainersRunning", "Every member container is running; launcher owns Ray membership readiness"
		if c := findGroupCondition(g, "GroupContainersRunning"); !c {
			condition(&g.Conditions, job.Generation, "GroupContainersRunning", true, "AllContainersRunning", message)
			if err := r.writeGroup(ctx, b, g); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if findGroupCondition(g, "GroupContainersRunning") {
		return r.failRay(ctx, b, g, "RunningMemberLost")
	}
	return r.reportState(ctx, job, phase, rayRows(tasks), reason, message, true, allRunning, false)
}

func findGroupCondition(g *ext.JobGroupStatus, kind string) bool {
	for _, c := range g.Conditions {
		if c.Type == kind && c.Status == metav1.ConditionTrue && c.ObservedGeneration == g.Plan.JobGeneration {
			return true
		}
	}
	return false
}
func rayFind(tasks []native.Task, ref ext.NativeObjectReference) *native.Task {
	for i := range tasks {
		if groupRef(&tasks[i]) == ref {
			return &tasks[i]
		}
	}
	return nil
}
func rayRows(tasks []native.Task) []native.JobTaskStatus {
	rows := make([]native.JobTaskStatus, 0, len(tasks))
	for _, t := range tasks {
		rows = append(rows, native.JobTaskStatus{Name: t.Name, Phase: t.Status.Phase, Message: t.Status.Message})
	}
	return rows
}
func (r JobReservationReconciler) failRay(ctx context.Context, b *ext.ContainerJob, g *ext.JobGroupStatus, reason string) (ctrl.Result, error) {
	g.Phase, g.Result, g.Reason = "Stopping", "Failed", reason
	condition(&g.Conditions, g.Plan.JobGeneration, "GroupFailed", true, reason, "New starts are fenced; all members require exact cleanup")
	return ctrl.Result{RequeueAfter: time.Millisecond}, r.writeGroup(ctx, b, g)
}

func (r JobReservationReconciler) cleanupRay(ctx context.Context, job *native.Job, b *ext.ContainerJob, g *ext.JobGroupStatus, tasks []native.Task) (ctrl.Result, error) {
	// Do not release a single host until every exposed member has a durable
	// cleanup proof. Unknown creation or a missing Task retains the operation.
	allClean := true
	for _, m := range g.Plan.Members {
		task := rayFind(tasks, m.Task)
		if task == nil {
			return ctrl.Result{}, companion.ErrIdentity
		}
		target, err := companion.Target(task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if target == nil {
			continue
		}
		var a ext.ContainerAllocation
		err = r.Client.Get(ctx, client.ObjectKey{Namespace: m.Node.Namespace, Name: companion.Name(m.Node.UID)}, &a)
		if apierrors.IsNotFound(err) {
			receipt, err := readReceipt(task, target)
			if err != nil {
				return ctrl.Result{}, err
			}
			allClean = allClean && receipt != nil
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if !reflect.DeepEqual(a.Spec, *target) || !ray.AllocationMatches(a.Spec) {
			return ctrl.Result{}, companion.ErrIdentity
		}
		allClean = allClean && cleanupProven(&a)
	}
	if !allClean {
		return r.reportState(ctx, job, native.JobPhasePending, rayRows(tasks), "GroupCleanupPending", fmt.Sprintf("Ray group stopped (%s); waiting for every exact cleanup proof", g.Reason), false, true, false)
	}
	done := true
	for i := range tasks {
		released, err := r.releaseAllocations(ctx, &tasks[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		done = done && released
	}
	if !done {
		return r.report(ctx, job, "GroupCleanupPending", "Every member is cleaned; allocation deletion is still pending", false)
	}
	g.Phase = g.Result
	if g.Phase != "Succeeded" && g.Phase != "Failed" {
		return ctrl.Result{}, companion.ErrIdentity
	}
	if err := r.writeGroup(ctx, b, g); err != nil {
		return ctrl.Result{}, err
	}
	return r.reportState(ctx, job, native.JobPhase(g.Phase), rayRows(tasks), "GroupCompleted", "All exact group allocations are cleaned and observed absent; terminal Task identities are retained", false, true, true)
}
