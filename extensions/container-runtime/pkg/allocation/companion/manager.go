// Package companion implements exclusive allocation in the companion API. It
// never writes preview reservations on native RLark Nodes.
package companion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Finalizer retains the exclusive lock through lost Agent reports and deletion.
const Finalizer = "containers.rlark.io/allocation-cleanup"

// ErrUnavailable means capacity is absent, stale, fenced or already reserved.
var ErrUnavailable = errors.New("container capacity unavailable")

// ErrIdentity means an object instance or its tenant/ownership no longer matches.
var ErrIdentity = errors.New("container allocation identity conflict")

// Manager requires an uncached API client: create-if-absent on a deterministic
// per-native-Node key is the atomic lock. The API, not process memory, owns it.
type Manager struct {
	Client           client.Client
	Now              func() time.Time
	HeartbeatTimeout time.Duration
}

// Request identifies already persisted native and companion objects. User input
// cannot specify an allocation UID, GPU subset, or execution identity.
type Request struct{ Binding, Node, Task client.ObjectKey }

// Name is stable across Agent/controller restarts and container replacements on
// the same native Node UID. Never use display names as lock identities.
func Name(nodeUID string) string {
	sum := sha256.Sum256([]byte(nodeUID))
	return "node-" + hex.EncodeToString(sum[:20])
}
func reference(obj metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

func (m Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
func (m Manager) timeout() time.Duration {
	if m.HeartbeatTimeout == 0 {
		return 45 * time.Second
	}
	return m.HeartbeatTimeout
}

// Acquire revalidates authoritative state and creates one whole-node reservation
// before dispatch. Repeating an uncertain successful Create recovers exactly the
// same reservation; a different Task or object incarnation cannot adopt it.
func (m Manager) Acquire(ctx context.Context, req Request) (*ext.ContainerAllocation, error) {
	return m.acquire(ctx, req, false)
}

// ValidateExisting revalidates an exact lock without creating an allocation or
// repairing Task metadata. The dispatch API must never become an allocator.
func (m Manager) ValidateExisting(ctx context.Context, req Request) (*ext.ContainerAllocation, error) {
	return m.acquire(ctx, req, true)
}

func (m Manager) acquire(ctx context.Context, req Request, existingOnly bool) (*ext.ContainerAllocation, error) {
	if m.Client == nil || m.timeout() <= 0 {
		return nil, errors.New("uncached client and positive heartbeat timeout required")
	}
	var binding ext.ContainerJob
	var node ext.ContainerNode
	var task native.Task
	if err := m.Client.Get(ctx, req.Binding, &binding); err != nil {
		return nil, fmt.Errorf("read Job binding: %w", err)
	}
	if err := m.Client.Get(ctx, req.Node, &node); err != nil {
		return nil, fmt.Errorf("read container Node: %w", err)
	}
	if err := m.Client.Get(ctx, req.Task, &task); err != nil {
		return nil, fmt.Errorf("read Task: %w", err)
	}
	var job native.Job
	var cluster native.Cluster
	var nativeNode native.Node
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: binding.Spec.Job.Namespace, Name: binding.Spec.Job.Name}, &job); err != nil {
		return nil, err
	}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: binding.Spec.Cluster.Namespace, Name: binding.Spec.Cluster.Name}, &cluster); err != nil {
		return nil, err
	}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nativeNode); err != nil {
		return nil, err
	}
	if err := ownership.JobBindingMatches(&binding, &job, tenant.IDOf(&job)); err != nil {
		return nil, fmt.Errorf("%w: invalid native Job binding", ErrIdentity)
	}
	for _, pair := range []struct {
		ref ext.NativeObjectReference
		obj metav1.Object
	}{{binding.Spec.Cluster, &cluster}, {node.Spec.Cluster, &cluster}, {node.Spec.Node, &nativeNode}} {
		if err := ownership.MatchesNative(pair.ref, pair.obj); err != nil {
			return nil, fmt.Errorf("%w: stale native reference", ErrIdentity)
		}
	}
	owner := metav1.GetControllerOf(&task)
	if task.UID == "" || task.Generation < 1 || job.Generation < 1 || node.UID == "" || binding.UID == "" || owner == nil || owner.UID != job.UID || owner.Kind != "Job" || owner.APIVersion != native.GroupVersion.String() || owner.Name != job.Name || job.Annotations[nativejob.ControllerOwnerAnnotation] != ext.Group || binding.Namespace != cluster.Spec.AgentNamespace || node.Namespace != cluster.Spec.AgentNamespace || task.Namespace != cluster.Spec.AgentNamespace || nativeNode.Namespace != cluster.Spec.AgentNamespace || node.Spec.Runtime != binding.Spec.Runtime || string(task.Spec.Runtime.Kind) != string(binding.Spec.Runtime) || string(nativeNode.Spec.AgentType) != string(binding.Spec.Runtime) {
		return nil, ErrIdentity
	}
	for _, obj := range []metav1.Object{&binding, &node, &task, &nativeNode, &cluster} {
		if tenant.IDOf(obj) != binding.Spec.TenantID || !obj.GetDeletionTimestamp().IsZero() {
			return nil, ErrIdentity
		}
	}
	if !job.DeletionTimestamp.IsZero() || job.Spec.Suspended || task.Spec.Suspended {
		return nil, ErrUnavailable
	}
	selector, err := labels.ValidatedSelectorFromSet(task.Spec.Placement.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid node selector", ErrIdentity)
	}
	if !selector.Matches(labels.Set(nativeNode.Labels)) {
		return nil, ErrUnavailable
	}
	if nativeNode.Status.Reservation != nil || len(nativeNode.Status.DockerReservations) != 0 || task.Spec.Placement.Binding != nil || task.Spec.Placement.DockerBinding != nil {
		return nil, fmt.Errorf("%w: legacy reservation/binding requires explicit migration", ErrIdentity)
	}
	if node.Spec.InstanceID == "" || node.Spec.Environment == "" || task.Spec.Placement.Environment != node.Spec.Environment || node.Spec.NetworkScope == "" || !net.ParseIP(node.Spec.PrivateAddress).IsPrivate() {
		return nil, ErrUnavailable
	}
	if len(node.Spec.AuthorizedGPUUUIDs) == 0 {
		return nil, fmt.Errorf("%w: GPU entitlement is not provisioned", ErrUnavailable)
	}
	seen := map[string]bool{}
	for _, id := range node.Spec.AuthorizedGPUUUIDs {
		if !strings.HasPrefix(id, "GPU-") || strings.ContainsAny(id, ", \t\r\n") || seen[id] {
			return nil, ErrIdentity
		}
		seen[id] = true
	}
	wanted := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: Name(string(nativeNode.UID)), Namespace: node.Namespace, Finalizers: []string{Finalizer}}, Spec: ext.ContainerAllocationSpec{Cluster: reference(&cluster), Job: reference(&job), Task: reference(&task), Node: reference(&nativeNode), TenantID: binding.Spec.TenantID, Generation: task.Generation, JobGeneration: job.Generation, ExecutionID: string(task.UID), NodeInstanceID: node.Spec.InstanceID, PrivateAddress: node.Spec.PrivateAddress, NetworkScope: node.Spec.NetworkScope, NodeRecordUID: string(node.UID), JobBindingUID: string(binding.UID), GPUUUIDs: slices.Clone(node.Spec.AuthorizedGPUUUIDs)}}
	sort.Strings(wanted.Spec.GPUUUIDs)
	tenant.Inherit(&job, wanted)
	wanted.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&task, native.GroupVersion.WithKind("Task"))}
	var current ext.ContainerAllocation
	key := client.ObjectKeyFromObject(wanted)
	err = m.Client.Get(ctx, key, &current)
	if err == nil {
		matched, err := sameReservation(&current, wanted)
		if err != nil {
			return nil, err
		}
		if existingOnly {
			target, err := Target(&task)
			if err != nil || target == nil || !reflect.DeepEqual(*target, wanted.Spec) || !controllerutil.ContainsFinalizer(&task, TaskFinalizer) {
				return nil, ErrIdentity
			}
			return matched, nil
		}
		if err := m.pinTarget(ctx, &task, wanted.Spec); err != nil {
			return nil, err
		}
		return matched, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if existingOnly {
		return nil, ErrUnavailable
	}
	if cluster.Spec.Cordoned || cluster.Spec.Draining || nativeNode.Spec.Unschedulable || !node.Status.Schedulable || node.Status.Busy || node.Status.AgentCredentialID == "" || node.Status.AgentCredentialID != node.Annotations[ext.AgentCredentialAnnotation] || node.Status.ObservedGeneration != node.Generation || !meta.IsStatusConditionPresentAndEqual(node.Status.Conditions, "Ready", metav1.ConditionTrue) || node.Status.LastHeartbeatTime == nil || node.Status.LastHeartbeatTime.After(m.now()) || m.now().Sub(node.Status.LastHeartbeatTime.Time) > m.timeout() {
		return nil, ErrUnavailable
	}
	if meta.FindStatusCondition(node.Status.Conditions, "Ready").ObservedGeneration != node.Generation {
		return nil, ErrUnavailable
	}
	if task.Status.Phase == native.TaskPhaseSucceeded || task.Status.Phase == native.TaskPhaseFailed || task.Status.Phase == native.TaskPhaseStopped {
		return nil, ErrUnavailable
	}
	// A target release is a scheduling fence until the node acknowledges that
	// release. Upgrade locking will share this same per-node allocation key.
	var clusters ext.ContainerClusterList
	if err := m.Client.List(ctx, &clusters, client.InNamespace(cluster.Spec.AgentNamespace)); err != nil {
		return nil, err
	}
	count := 0
	for _, cc := range clusters.Items {
		if cc.Spec.Cluster == reference(&cluster) {
			count++
			if cc.Spec.Runtime != node.Spec.Runtime || !cc.DeletionTimestamp.IsZero() {
				return nil, ErrUnavailable
			}
			if cc.Spec.TargetAgentRelease != "" {
				var release ext.AgentRelease
				if err := m.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: cc.Spec.TargetAgentRelease}, &release); err != nil {
					return nil, err
				}
				if release.UID == "" || !release.DeletionTimestamp.IsZero() || tenant.IDOf(&release) != tenant.IDOf(&node) || artifact.ValidateSpec(release.Spec) != nil || !release.Status.Available || release.Status.ObservedGeneration != release.Generation || node.Status.AgentVersion != release.Spec.Version {
					return nil, fmt.Errorf("%w: node is fenced for target release", ErrUnavailable)
				}
			}
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("%w: ambiguous or missing companion Cluster", ErrIdentity)
	}
	var all native.ClusterList
	if err := m.Client.List(ctx, &all); err != nil {
		return nil, err
	}
	for _, other := range all.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return nil, ErrIdentity
		}
	}
	var held ext.ContainerAllocationList
	if err := m.Client.List(ctx, &held); err != nil {
		return nil, err
	}
	for _, a := range held.Items {
		if a.Spec.Task == reference(&task) {
			return nil, fmt.Errorf("%w: Task already has an allocation", ErrIdentity)
		}
		if a.Spec.Node.UID == string(nativeNode.UID) || a.Spec.NetworkScope == node.Spec.NetworkScope && a.Spec.PrivateAddress == node.Spec.PrivateAddress {
			return nil, ErrUnavailable
		}
		for _, id := range a.Spec.GPUUUIDs {
			if seen[id] {
				return nil, ErrUnavailable
			}
		}
	}
	// Duplicate entitlement/address anywhere in companion inventory is ambiguous
	// even while idle. Reject both registrations instead of guessing ownership.
	var inventory ext.ContainerNodeList
	if err := m.Client.List(ctx, &inventory); err != nil {
		return nil, err
	}
	for _, other := range inventory.Items {
		if other.UID == node.UID {
			continue
		}
		if other.Spec.Node == node.Spec.Node {
			return nil, ErrIdentity
		}
		if other.Spec.NetworkScope == node.Spec.NetworkScope && other.Spec.PrivateAddress == node.Spec.PrivateAddress {
			return nil, ErrIdentity
		}
		for _, id := range other.Spec.AuthorizedGPUUUIDs {
			if seen[id] {
				return nil, ErrIdentity
			}
		}
	}
	if err := m.pinTarget(ctx, &task, wanted.Spec); err != nil {
		return nil, err
	}
	if err := m.Client.Create(ctx, wanted); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := m.Client.Get(ctx, key, &current); err != nil {
			return nil, err
		}
		return sameReservation(&current, wanted)
	}
	return wanted, nil
}
func sameReservation(current, wanted *ext.ContainerAllocation) (*ext.ContainerAllocation, error) {
	if !current.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(current, Finalizer) || !reflect.DeepEqual(current.Spec, wanted.Spec) || tenant.IDOf(current) != wanted.Spec.TenantID || !reflect.DeepEqual(current.OwnerReferences, wanted.OwnerReferences) {
		return nil, fmt.Errorf("%w: node is reserved by a different execution", ErrUnavailable)
	}
	return current, nil
}

// Release consumes only the exact persisted cleanup proof for the allocation
// UID. Heartbeat expiry, Task deletion, and an untrusted boolean are insufficient.
// Deletion is observed before removing the lock finalizer, making crash recovery
// safe. The caller must retain Task identity until this operation finishes.
func (m Manager) Release(ctx context.Context, key client.ObjectKey, uid string) error {
	if m.Client == nil || uid == "" {
		return ErrIdentity
	}
	var a ext.ContainerAllocation
	if err := m.Client.Get(ctx, key, &a); err != nil {
		return client.IgnoreNotFound(err)
	}
	if string(a.UID) != uid {
		return ErrIdentity
	}
	proof := meta.FindStatusCondition(a.Status.Conditions, "CleanupConfirmed")
	if !a.Status.CleanupConfirmed || proof == nil || proof.Status != metav1.ConditionTrue || proof.ObservedGeneration != a.Generation || a.Status.ObservedGeneration != a.Generation {
		return fmt.Errorf("%w: cleanup is unconfirmed", ErrUnavailable)
	}
	if a.DeletionTimestamp.IsZero() {
		if err := m.Client.Delete(ctx, &a, client.Preconditions{UID: &a.UID, ResourceVersion: &a.ResourceVersion}); err != nil {
			return err
		}
		if err := m.Client.Get(ctx, key, &a); err != nil {
			return client.IgnoreNotFound(err)
		}
		if string(a.UID) != uid {
			return ErrIdentity
		}
	}
	before := a.DeepCopy()
	controllerutil.RemoveFinalizer(&a, Finalizer)
	if len(before.Finalizers) != len(a.Finalizers) {
		if err := m.Client.Patch(ctx, &a, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	var observed ext.ContainerAllocation
	if err := m.Client.Get(ctx, key, &observed); err != nil {
		return client.IgnoreNotFound(err)
	}
	if string(observed.UID) != uid {
		return nil
	}
	return fmt.Errorf("%w: allocation deletion is still pending", ErrUnavailable)
}
