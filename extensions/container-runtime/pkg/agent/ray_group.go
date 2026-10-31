package agent

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	ray "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (s TaskService) groupBinding(ctx context.Context, a *ext.ContainerAllocation) (*ext.ContainerJob, error) {
	if a == nil || !ray.AllocationMatches(a.Spec) {
		return nil, ErrIdentity
	}
	p := a.Spec.Group.Plan
	var b ext.ContainerJob
	if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: p.Binding.Namespace, Name: p.Binding.Name}, &b); err != nil {
		return nil, err
	}
	g := b.Status.Group
	if objectRef(&b) != p.Binding || b.Spec.Job != p.Job || b.Spec.Cluster != p.Cluster || b.Spec.Runtime != "Docker" || b.Spec.TenantID != p.TenantID || tenant.IDOf(&b) != p.TenantID || g == nil || g.ID != a.Spec.Group.ID || g.Digest != a.Spec.Group.Digest || !reflect.DeepEqual(g.Plan, p) {
		return nil, ErrIdentity
	}
	return &b, nil
}

// authorizeGroup observes every exact member for each effect. No membership,
// phase, heartbeat, credential, receipt or manifest is trusted from the caller.
func (s TaskService) authorizeGroup(ctx context.Context, a *Assignment, start bool) error {
	b, err := s.groupBinding(ctx, &a.Allocation)
	if err != nil {
		return err
	}
	g := b.Status.Group
	if !b.DeletionTimestamp.IsZero() || g.Phase != "Preparing" && g.Phase != "Starting" || start && g.Phase != "Starting" {
		return ErrUnavailable
	}
	var job native.Job
	if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: g.Plan.Job.Namespace, Name: g.Plan.Job.Name}, &job); err != nil {
		return err
	}
	if objectRef(&job) != g.Plan.Job || job.Generation != g.Plan.JobGeneration || job.Annotations[nativejob.RestartedAtAnnotation] != g.Plan.Restart || job.Spec.Suspended || !job.DeletionTimestamp.IsZero() || meta.IsStatusConditionTrue(job.Status.Conditions, taskcontract.TaskSetRetiringCondition) || tenant.IDOf(&job) != g.Plan.TenantID {
		return ErrUnavailable
	}
	for index, m := range g.Plan.Members {
		var node ext.ContainerNode
		if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: m.NodeRecord.Namespace, Name: m.NodeRecord.Name}, &node); err != nil {
			return err
		}
		if objectRef(&node) != m.NodeRecord || node.Spec.Node != m.Node || node.Spec.InstanceID != m.InstanceID || node.Spec.Cluster != g.Plan.Cluster || node.Spec.PrivateAddress != m.PrivateAddr || node.Spec.NetworkScope != g.Plan.NetworkScope || node.Annotations[CredentialAnnotation] != m.CredentialID || node.Status.AgentCredentialID != m.CredentialID || !sameGPUSet(node.Spec.AuthorizedGPUUUIDs, m.GPUUUIDs) || !slices.Contains(node.Status.Capabilities, ray.Capability) || !slices.Contains(node.Status.Capabilities, DockerExecutionCapability) {
			return ErrIdentity
		}
		id := Identity{Namespace: node.Namespace, Name: node.Name, UID: string(node.UID), CredentialID: m.CredentialID, InstanceID: m.InstanceID, ClusterUID: g.Plan.Cluster.UID, TenantID: g.Plan.TenantID}
		if err := s.Reporter.validateBindings(ctx, &node, id); err != nil {
			return err
		}
		now := s.Reporter.now()
		ready := meta.FindStatusCondition(node.Status.Conditions, "Ready")
		if !node.DeletionTimestamp.IsZero() || node.Status.ObservedGeneration != node.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != node.Generation || node.Status.LastHeartbeatTime == nil || node.Status.LastHeartbeatTime.After(now) || now.Sub(node.Status.LastHeartbeatTime.Time) > 45*time.Second {
			return ErrUnavailable
		}
		_, releaseReady, err := s.Reporter.releaseFence(ctx, &node, node.Status.AgentVersion)
		if err != nil {
			return err
		}
		if !releaseReady {
			return ErrUnavailable
		}
		var nn native.Node
		if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: m.Node.Namespace, Name: m.Node.Name}, &nn); err != nil {
			return err
		}
		if nn.Spec.Unschedulable {
			return ErrUnavailable
		}
		var task native.Task
		if err := s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: m.Task.Namespace, Name: m.Task.Name}, &task); err != nil {
			return err
		}
		if objectRef(&task) != m.Task || task.Generation != m.Generation || task.Spec.Suspended || !task.DeletionTimestamp.IsZero() || !ray.MatchesTask(&job, b.Namespace, &task) || task.Status.Phase == native.TaskPhaseFailed || task.Status.Phase == native.TaskPhaseSucceeded || task.Status.Phase == native.TaskPhaseStopped {
			return ErrUnavailable
		}
		allocation, err := (companion.Manager{Client: s.Reporter.Client, Now: s.Reporter.Now}).ValidateExisting(ctx, companion.Request{Binding: client.ObjectKeyFromObject(b), Node: client.ObjectKeyFromObject(&node), Task: client.ObjectKeyFromObject(&task)})
		if err != nil {
			return err
		}
		if allocation.Status.CleanupConfirmed || allocation.Spec.Group == nil || allocation.Spec.Group.MemberIndex != int32(index) || allocation.Spec.Group.ID != g.ID || allocation.Spec.Group.Digest != g.Digest || !reflect.DeepEqual(allocation.Spec.Group.Plan, g.Plan) {
			return ErrIdentity
		}
		if start && !ray.PreparedMatches(allocation) {
			return ErrUnavailable
		}
		if start {
			compiled, err := ray.CompileAllocation(&task, allocation.Spec)
			if err != nil {
				return err
			}
			if compiled.CompiledHash != allocation.Status.Prepared.ManifestDigest {
				return ErrIdentity
			}
		}
	}
	// Re-read the durable barrier after observing members to detect concurrent
	// stop/spec changes before granting the external operation.
	again, err := s.groupBinding(ctx, &a.Allocation)
	if err != nil {
		return err
	}
	if again.ResourceVersion != b.ResourceVersion || again.Status.Group.Phase != g.Phase {
		return ErrUnavailable
	}
	var latest native.Job
	if err := s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&job), &latest); err != nil {
		return err
	}
	if objectRef(&latest) != objectRef(&job) || latest.Generation != job.Generation || latest.Spec.Suspended || !latest.DeletionTimestamp.IsZero() || latest.Annotations[nativejob.RestartedAtAnnotation] != g.Plan.Restart || meta.IsStatusConditionTrue(latest.Status.Conditions, taskcontract.TaskSetRetiringCondition) {
		return ErrUnavailable
	}
	return nil
}

func (s TaskService) prepared(ctx context.Context, a *Assignment, receipt *ext.ContainerPreparedReceipt) error {
	if a.Task.Annotations[host.TaskClaimantAnnotation] == "" || !controllerutil.ContainsFinalizer(&a.Task, host.TaskFinalizer) {
		return ErrUnavailable
	}
	candidate := a.Allocation.DeepCopy()
	candidate.Status.Prepared = receipt.DeepCopy()
	candidate.Status.ObservedGeneration = candidate.Generation
	if !ray.PreparedMatches(candidate) {
		return ErrIdentity
	}
	plan, err := ray.CompileAllocation(&a.Task, a.Allocation.Spec)
	if err != nil {
		return err
	}
	if plan.CompiledHash != receipt.ManifestDigest {
		return ErrIdentity
	}
	current := &a.Allocation
	if current.Status.Prepared != nil {
		if !reflect.DeepEqual(current.Status.Prepared, receipt) {
			return errors.New("prepared container receipt conflicts with existing identity")
		}
		return nil
	}
	before := current.DeepCopy()
	current.Status.Prepared = receipt.DeepCopy()
	current.Status.ObservedGeneration = current.Generation
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "ExecutionPrepared", Status: metav1.ConditionTrue, Reason: "ExactDockerContainerPrepared", ObservedGeneration: current.Generation, LastTransitionTime: metav1.NewTime(s.Reporter.now())})
	return s.Reporter.Client.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
