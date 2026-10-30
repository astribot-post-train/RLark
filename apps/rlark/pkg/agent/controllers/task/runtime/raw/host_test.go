package raw

import (
	"context"
	"testing"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/rawallocation"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHostProcessClaimRecoveryStatusAndCleanup(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "training", UID: "job"}}
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "agents", UID: "node-uid"}, Spec: api.NodeSpec{AgentType: api.AgentTypeRaw}, Status: api.NodeStatus{Phase: api.NodeOnline, LastHeartbeatTime: &metav1.Time{Time: time.Now()}, Raw: &api.RawNodeStatus{Incarnation: "container", TrainingIP: "10.0.0.1", NetworkScope: "private", Environment: "env", AuthorizedGPUUUIDs: []string{"GPU-1"}}}}
	task := &api.Task{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "task-uid", Generation: 1, Annotations: map[string]string{utils.ParentUIDAnnotation: "job"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}}
	task.Spec.Runtime.Kind = "Raw"
	task.Spec.Workload.Kind, task.Spec.Workload.Version = "test/process", "v1"
	task.Spec.Workload.Carrier = api.TaskWorkloadCarrier{Type: processCarrier, Version: "v1"}
	management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}, &api.Task{}).WithObjects(cluster, job, node).Build()
	manager := rawallocation.Manager{Client: management}
	bindings, err := manager.Reserve(ctx, rawallocation.Request{Cluster: client.ObjectKeyFromObject(cluster), JobUID: "job", TenantID: tenant.DefaultID, ExecutionID: "run", Environment: "env", Nodes: []client.ObjectKey{client.ObjectKeyFromObject(node)}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := manager.AssignTask(ctx, client.ObjectKeyFromObject(node), bindings[0], tenant.DefaultID, string(task.UID))
	if err != nil {
		t.Fatal(err)
	}
	task.Spec.Placement.Binding = &binding
	if err := management.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	provider, supervisor, _ := request(t)
	admission := NodeAdmission{Client: management, Node: client.ObjectKeyFromObject(node), NodeUID: string(node.UID), Incarnation: "container", TenantID: tenant.DefaultID}
	assemble := func() *host.Host {
		t.Helper()
		result, err := NewHost(ctx, management, admission, provider, []workload.Adapter{processTestAdapter{spec: ProcessSpec{Argv: []string{"/usr/bin/python3", "train.py"}, Directory: "/work"}}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	reconcile := func(h *host.Host) {
		t.Helper()
		result, err := h.Reconcile(ctx, client.ObjectKeyFromObject(task))
		if err != nil || result.Outcome != host.OutcomeSucceeded {
			t.Fatalf("reconcile: %+v %v", result, err)
		}
	}
	h := assemble()
	reconcile(h)
	reconcile(assemble())
	if supervisor.starts != 1 {
		t.Fatalf("restart duplicated process: %d", supervisor.starts)
	}
	var current api.Task
	if err := management.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations[host.TaskClaimantAnnotation] != "agents/Raw/node-uid" || current.Status.Phase != api.TaskPhaseRunning {
		t.Fatalf("claim/status: %+v", current)
	}
	// Unknown observations must not be projected as a successful known state.
	supervisor.record.Phase = "Unknown"
	reconcile(h)
	if err := management.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	known := false
	for _, condition := range current.Status.Conditions {
		if condition.Type == "ExecutionKnown" {
			known = condition.Status == metav1.ConditionFalse && condition.ObservedGeneration == 1
		}
	}
	if !known || current.Status.Phase != api.TaskPhasePending {
		t.Fatalf("unknown status: %+v", current.Status)
	}
	// A sibling Host must neither claim nor stop this node's execution.
	sibling := admission
	sibling.NodeUID = "other-node"
	other, err := NewHost(ctx, management, sibling, provider, []workload.Adapter{processTestAdapter{spec: supervisor.record.Spec}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Reconcile(ctx, client.ObjectKeyFromObject(task)); err != nil {
		t.Fatal(err)
	}
	if supervisor.stops != 0 || supervisor.starts != 1 {
		t.Fatal("sibling changed execution")
	}
	current.Spec.Suspended = true
	if err := management.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	result, err := h.Reconcile(ctx, client.ObjectKeyFromObject(task))
	if err != nil || result.Reason != "TaskSuspended" || !supervisor.record.Deleted {
		t.Fatalf("suspend: %+v %v", result, err)
	}
	if err := management.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if err := management.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Reconcile(ctx, client.ObjectKeyFromObject(task)); err != nil {
		t.Fatal(err)
	}
}
