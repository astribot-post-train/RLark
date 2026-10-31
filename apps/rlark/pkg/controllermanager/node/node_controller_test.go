package node

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

func TestReconcileUsesActiveNodeLease(t *testing.T) {
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	leaseDuration := int32(30)
	renewTime := metav1.NewMicroTime(now.Add(-5 * time.Second))
	previousHeartbeat := metav1.NewTime(now.Add(-time.Hour))

	scheme := newTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&rlarkv1alpha1.Node{}).WithObjects(
		&rlarkv1alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "rlark-agent-cluster-1"},
			Status: rlarkv1alpha1.NodeStatus{
				Phase:             rlarkv1alpha1.NodeOffline,
				Reason:            "NodeAgentLeaseMissing",
				LastHeartbeatTime: &previousHeartbeat,
			},
		},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: nodeHeartbeatLeaseName("worker-1"), Namespace: "rlark-agent-cluster-1"},
			Spec: coordinationv1.LeaseSpec{
				RenewTime:            &renewTime,
				LeaseDurationSeconds: &leaseDuration,
			},
		},
	).Build()

	reconciler := &Reconciler{Client: client, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), request("rlark-agent-cluster-1", "worker-1")); err != nil {
		t.Fatal(err)
	}

	var node rlarkv1alpha1.Node
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "rlark-agent-cluster-1", Name: "worker-1"}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Phase != rlarkv1alpha1.NodeOnline || node.Status.Reason != "AgentLeaseActive" {
		t.Fatalf("unexpected status: %#v", node.Status)
	}
	if node.Status.LastHeartbeatTime == nil || !node.Status.LastHeartbeatTime.Equal(&metav1.Time{Time: renewTime.Time}) {
		t.Fatalf("expected Lease renewal time, got %v", node.Status.LastHeartbeatTime)
	}
	if node.Status.LastStatusChangeTime == nil || !node.Status.LastStatusChangeTime.Equal(&metav1.Time{Time: now}) {
		t.Fatalf("expected status change time %s, got %v", now, node.Status.LastStatusChangeTime)
	}
	condition := apimeta.FindStatusCondition(node.Status.Conditions, rlarkv1alpha1.ConditionTypeReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "AgentLeaseActive" {
		t.Fatalf("unexpected Ready condition: %#v", condition)
	}
}

func TestReconcileUsesLeaseDurationForExpiry(t *testing.T) {
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	leaseDuration := int32(30)
	renewTime := metav1.NewMicroTime(now.Add(-time.Duration(leaseDuration+1) * time.Second))

	scheme := newTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&rlarkv1alpha1.Node{}).WithObjects(
		&rlarkv1alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "rlark-agent-cluster-1"}},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: nodeHeartbeatLeaseName("worker-1"), Namespace: "rlark-agent-cluster-1"},
			Spec: coordinationv1.LeaseSpec{
				RenewTime:            &renewTime,
				LeaseDurationSeconds: &leaseDuration,
			},
		},
	).Build()

	reconciler := &Reconciler{Client: client, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), request("rlark-agent-cluster-1", "worker-1")); err != nil {
		t.Fatal(err)
	}

	var node rlarkv1alpha1.Node
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "rlark-agent-cluster-1", Name: "worker-1"}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Phase != rlarkv1alpha1.NodeOffline || node.Status.Reason != "AgentLeaseExpired" {
		t.Fatalf("unexpected status: %#v", node.Status)
	}
}

func TestReconcileMarksMissingNodeLeaseOffline(t *testing.T) {
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	legacyHeartbeat := metav1.NewTime(now.Add(-time.Minute))

	scheme := newTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&rlarkv1alpha1.Node{}).WithObjects(
		&rlarkv1alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "rlark-agent-cluster-1"},
			Status: rlarkv1alpha1.NodeStatus{
				Phase:             rlarkv1alpha1.NodeOnline,
				Reason:            "AgentLeaseActive",
				LastHeartbeatTime: &legacyHeartbeat,
			},
		},
	).Build()

	reconciler := &Reconciler{Client: client, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), request("rlark-agent-cluster-1", "worker-1")); err != nil {
		t.Fatal(err)
	}

	var node rlarkv1alpha1.Node
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "rlark-agent-cluster-1", Name: "worker-1"}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Phase != rlarkv1alpha1.NodeOffline || node.Status.Reason != "NodeAgentLeaseMissing" {
		t.Fatalf("unexpected status: %#v", node.Status)
	}
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rlarkv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func request(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func TestRawLeaseFencesReplacedNodeAndRotatedCredentials(t *testing.T) {
	for _, mode := range []string{"current", "rotated", "replacement", "node-recreated", "unowned"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			node := &rlarkv1alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "raw", Namespace: "agents", UID: "node", Annotations: map[string]string{"raw.rlark.io/credential-id": "key"}}, Spec: rlarkv1alpha1.NodeSpec{AgentType: rlarkv1alpha1.AgentTypeRaw}, Status: rlarkv1alpha1.NodeStatus{Raw: &rlarkv1alpha1.RawNodeStatus{Incarnation: "container"}}}
			holder := "node/container/key"
			renew := metav1.NewMicroTime(now)
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: nodeHeartbeatLeaseName(node.Name), Namespace: node.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(node, rlarkv1alpha1.GroupVersion.WithKind("Node"))}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, RenewTime: &renew}}
			switch mode {
			case "rotated":
				node.Annotations["raw.rlark.io/credential-id"] = "new-key"
			case "replacement":
				node.Status.Raw.Incarnation = "new-container"
			case "node-recreated":
				node.UID = "new-node"
			case "unowned":
				lease.OwnerReferences = nil
			}
			c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithStatusSubresource(node).WithObjects(node, lease).Build()
			reconciler := &Reconciler{Client: c, Now: func() time.Time { return now }}
			if _, err := reconciler.Reconcile(context.Background(), request(node.Namespace, node.Name)); err != nil {
				t.Fatal(err)
			}
			var current rlarkv1alpha1.Node
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: node.Namespace, Name: node.Name}, &current); err != nil {
				t.Fatal(err)
			}
			if mode == "current" {
				if current.Status.Phase != rlarkv1alpha1.NodeOnline {
					t.Fatalf("current Lease rejected: %+v", current.Status)
				}
			} else if current.Status.Phase != rlarkv1alpha1.NodeOffline || current.Status.LastHeartbeatTime != nil || current.Status.Reason != "RawNodeLeaseIdentityMismatch" {
				t.Fatalf("stale Lease accepted: %+v", current.Status)
			}
		})
	}
}
