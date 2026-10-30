package raw

import (
	"context"
	"errors"
	"fmt"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/rawallocation"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeAdmission enforces local incarnation, tenant, Job ownership and the full
// committed reservation group using fresh management API reads before a start.
// Client must be uncached. The node identity is operator-configured, not supplied
// by a Task or inferred from its selector. It is not a credential issuer.
type NodeAdmission struct {
	Client      client.Client
	Node        client.ObjectKey
	NodeUID     string
	Incarnation string
	TenantID    string
}

var _ Admission = NodeAdmission{}

// Authorize rejects direct Tasks, stale incarnations and incomplete groups.
func (a NodeAdmission) Authorize(ctx context.Context, identity rt.TaskIdentity) error {
	if a.Client == nil || a.Node.Namespace == "" || a.Node.Name == "" || a.NodeUID == "" || a.Incarnation == "" || a.TenantID == "" {
		return errors.New("raw node admission identity is incomplete")
	}
	deny := func() error {
		return fault.New(fault.PermissionDenied, "Raw Task is not authorized for this node and execution")
	}
	if identity.Logical.Namespace != a.Node.Namespace {
		return deny()
	}
	var task api.Task
	if err := a.Client.Get(ctx, client.ObjectKey{Namespace: identity.Logical.Namespace, Name: identity.Logical.Name}, &task); err != nil {
		return fmt.Errorf("read admitted Raw Task: %w", err)
	}
	b := task.Spec.Placement.Binding
	if task.UID == "" || string(task.UID) != identity.ObjectUID || task.Generation != identity.Generation || !task.DeletionTimestamp.IsZero() || task.Spec.Suspended || task.Spec.Runtime.Kind != "Raw" || b == nil || b.TaskUID != identity.ObjectUID || b.NodeName != a.Node.Name || b.NodeUID != a.NodeUID || b.Incarnation != a.Incarnation || tenant.IDOf(&task) != a.TenantID {
		return deny()
	}
	owner := metav1.GetControllerOf(&task)
	if owner == nil || owner.Kind != "Job" || owner.APIVersion != api.GroupVersion.String() || string(owner.UID) != b.JobUID || task.Annotations[utils.ParentUIDAnnotation] != b.JobUID {
		return deny()
	}
	var job api.Job
	if err := a.Client.Get(ctx, client.ObjectKey{Name: owner.Name}, &job); err != nil {
		return fmt.Errorf("read admitted Raw Job: %w", err)
	}
	if string(job.UID) != b.JobUID || tenant.IDOf(&job) != a.TenantID || job.Spec.Suspended || !job.DeletionTimestamp.IsZero() {
		return deny()
	}
	var node api.Node
	if err := a.Client.Get(ctx, a.Node, &node); err != nil {
		return fmt.Errorf("read admitted Raw Node: %w", err)
	}
	if err := rawallocation.ValidateBinding(&node, b, a.TenantID); err != nil {
		return fault.Wrap(fault.PermissionDenied, "validate Raw reservation", err)
	}
	// Reconstruct the entire reservation set. Its digest must match the binding;
	// a singleton forged Task cannot bypass a multi-node commit barrier.
	var nodes api.NodeList
	if err := a.Client.List(ctx, &nodes, client.InNamespace(a.Node.Namespace)); err != nil {
		return fmt.Errorf("read Raw group reservations: %w", err)
	}
	bindings := make([]api.RawTaskBinding, 0)
	for _, member := range nodes.Items {
		r := member.Status.Reservation
		if r == nil || r.JobUID != b.JobUID || r.ExecutionID != b.ExecutionID {
			continue
		}
		if r.TaskUID == "" {
			return deny()
		}
		bindings = append(bindings, api.RawTaskBinding{TaskUID: r.TaskUID, NodeName: member.Name, NodeUID: r.NodeUID, Incarnation: r.Incarnation, ExecutionID: r.ExecutionID, AllocationID: r.AllocationID, JobUID: r.JobUID, ClusterUID: r.ClusterUID, GroupDigest: r.GroupDigest})
	}
	// Reservation assignment is only the first half of dispatch. Every member
	// must also have its live controller-owned Task with the same binding before
	// any process starts; a missing/suspended peer cannot satisfy the barrier.
	var tasks api.TaskList
	if err := a.Client.List(ctx, &tasks, client.InNamespace(a.Node.Namespace)); err != nil {
		return fmt.Errorf("read assigned Raw Tasks: %w", err)
	}
	assigned := make(map[string]*api.Task, len(tasks.Items))
	for i := range tasks.Items {
		assigned[string(tasks.Items[i].UID)] = &tasks.Items[i]
	}
	for _, expected := range bindings {
		peer := assigned[expected.TaskUID]
		if peer == nil || peer.Spec.Suspended || !peer.DeletionTimestamp.IsZero() || peer.Spec.Runtime.Kind != "Raw" || tenant.IDOf(peer) != a.TenantID || peer.Spec.Placement.Binding == nil || *peer.Spec.Placement.Binding != expected {
			return deny()
		}
		owner := metav1.GetControllerOf(peer)
		if owner == nil || owner.Kind != "Job" || owner.APIVersion != api.GroupVersion.String() || owner.Name != job.Name || owner.UID != job.UID || peer.Annotations[utils.ParentUIDAnnotation] != b.JobUID {
			return deny()
		}
	}
	if err := (rawallocation.Manager{Client: a.Client}).ValidateGroup(ctx, a.Node.Namespace, a.TenantID, bindings); err != nil {
		return fault.Wrap(fault.PermissionDenied, "validate complete Raw group", err)
	}
	return nil
}
