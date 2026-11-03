package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativecluster "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/cluster"
	nativenode "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/node"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func connectivityFixture(t *testing.T, heartbeat time.Time) (*ConnectivityReconciler, client.Client, *native.Node, *ext.ContainerNode, *native.Cluster) {
	t.Helper()
	scheme := testScheme(t)
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster-uid", Labels: map[string]string{tenant.IDLabel: "team-a"}}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	node := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "node-uid", Labels: map[string]string{tenant.IDLabel: "team-a"}}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw, Unschedulable: true}}
	containerCluster := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "agents", UID: "container-cluster-uid", Labels: map[string]string{tenant.IDLabel: "team-a"}}, Spec: ext.ContainerClusterSpec{Cluster: ext.NativeObjectReference{Name: "cloud", UID: string(cluster.UID)}, Runtime: "Raw", AgentNamespace: "agents"}}
	now := metav1.NewTime(heartbeat)
	containerNode := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "container-node-uid", Generation: 1, Labels: map[string]string{tenant.IDLabel: "team-a"}, Annotations: map[string]string{ext.AgentCredentialAnnotation: "credential-1"}}, Spec: ext.ContainerNodeSpec{Cluster: ext.NativeObjectReference{Name: "cloud", UID: string(cluster.UID)}, Node: ext.NativeObjectReference{Name: "worker", Namespace: "agents", UID: string(node.UID)}, Runtime: "Raw", InstanceID: "instance-1", PrivateAddress: "10.0.0.10", NetworkScope: "private", Environment: "fleet", AuthorizedGPUUUIDs: []string{"GPU-test"}}}
	containerNode.Status.ObservedGeneration = 1
	containerNode.Status.AgentCredentialID = "credential-1"
	containerNode.Status.LastHeartbeatTime = &now
	containerNode.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cluster, node, containerNode).WithObjects(cluster, node, containerCluster, containerNode).Build()
	r := &ConnectivityReconciler{Client: c, Now: func() time.Time { return heartbeat.Add(time.Second) }}
	return r, c, node, containerNode, cluster
}

func TestConnectivityReconcilerProjectsFreshRawHeartbeat(t *testing.T) {
	ctx := context.Background()
	heartbeat := time.Now().UTC().Truncate(time.Second)
	r, c, node, containerNode, cluster := connectivityFixture(t, heartbeat)
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var gotNode native.Node
	if err := c.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: node.Name}, &gotNode); err != nil {
		t.Fatal(err)
	}
	if gotNode.Status.Raw == nil || gotNode.Status.Raw.Incarnation != containerNode.Spec.InstanceID || gotNode.Annotations["raw.rlark.io/credential-id"] != "credential-1" || gotNode.Annotations[nativeProjectionAnnotation] != string(containerNode.UID) {
		t.Fatalf("native Raw identity projection missing: annotations=%v raw=%#v", gotNode.Annotations, gotNode.Status.Raw)
	}
	if !gotNode.Spec.Unschedulable {
		t.Fatal("connectivity projection changed native Node schedulability")
	}
	var nodeLease coordinationv1.Lease
	if err := c.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: nodeHeartbeatPrefix + node.Name}, &nodeLease); err != nil {
		t.Fatal(err)
	}
	if !projectionLeaseOwned(&nodeLease, &gotNode, "Node", string(containerNode.UID)) || nodeLease.Spec.RenewTime == nil || !nodeLease.Spec.RenewTime.Time.Equal(heartbeat) {
		t.Fatalf("node Lease ownership or source heartbeat is wrong: %#v", nodeLease)
	}
	var clusterLease coordinationv1.Lease
	if err := c.Get(ctx, types.NamespacedName{Namespace: "agents", Name: heartbeatLeaseName}, &clusterLease); err != nil {
		t.Fatal(err)
	}
	if !projectionLeaseOwned(&clusterLease, cluster, "Cluster", "container-cluster-uid") || clusterLease.Spec.RenewTime == nil || !clusterLease.Spec.RenewTime.Time.Equal(heartbeat) {
		t.Fatalf("cluster Lease ownership or source heartbeat is wrong: %#v", clusterLease)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("repeated reconciliation should be idempotent: %v", err)
	}
}

func TestConnectivityReconcilerDoesNotReplayExpiredHeartbeat(t *testing.T) {
	ctx := context.Background()
	heartbeat := time.Now().UTC().Add(-time.Minute)
	r, c, _, _, cluster := connectivityFixture(t, heartbeat)
	r.Now = func() time.Time { return heartbeat.Add(time.Minute) }
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatal(err)
	}
	var leases coordinationv1.LeaseList
	if err := c.List(ctx, &leases); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 0 {
		t.Fatalf("expired heartbeat was projected or refreshed: %#v", leases.Items)
	}
}

func TestConnectivityReconcilerRefusesUnknownNodeLease(t *testing.T) {
	ctx := context.Background()
	heartbeat := time.Now().UTC().Truncate(time.Second)
	r, base, node, containerNode, cluster := connectivityFixture(t, heartbeat)
	foreign := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: node.Namespace, UID: "foreign-uid"}}
	if err := base.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: nodeHeartbeatPrefix + node.Name, Namespace: node.Namespace, Annotations: map[string]string{nativeProjectionAnnotation: "foreign-container-node"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(foreign, native.GroupVersion.WithKind("Node"))}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err == nil {
		t.Fatal("unknown native Lease was adopted")
	}
	var gotNode native.Node
	if err := base.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: node.Name}, &gotNode); err != nil {
		t.Fatal(err)
	}
	if gotNode.Status.Raw != nil || gotNode.Annotations[nativeProjectionAnnotation] != "" {
		t.Fatal("native Node was partially projected before Lease ownership was proven")
	}
	var gotContainerNode ext.ContainerNode
	if err := base.Get(ctx, types.NamespacedName{Namespace: containerNode.Namespace, Name: containerNode.Name}, &gotContainerNode); err != nil {
		t.Fatal(err)
	}
}

func TestConnectivityReconcilerDoesNotChangeReservation(t *testing.T) {
	ctx := context.Background()
	heartbeat := time.Now().UTC().Truncate(time.Second)
	r, c, node, _, cluster := connectivityFixture(t, heartbeat)
	want := &native.RawNodeReservation{JobUID: "job", ExecutionID: "run", AllocationID: "alloc", NodeUID: string(node.UID), Incarnation: "instance-1", TenantID: "team-a"}
	var current native.Node
	if err := c.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}
	current.Status.Reservation = want.DeepCopy()
	if err := c.Status().Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err == nil {
		t.Fatal("legacy reservation did not fence the adapter")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Reservation == nil || current.Status.Reservation.AllocationID != want.AllocationID {
		t.Fatalf("connectivity adapter modified workload reservation: %#v", current.Status.Reservation)
	}
	if current.Status.DockerReservations != nil {
		t.Fatal("connectivity adapter populated Docker reservations")
	}
}

func TestConnectivityNativeControllerLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	r, c, nn, cn, cluster := connectivityFixture(t, now)
	second := nn.DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "worker-b", "node-b", ""
	secondCN := cn.DeepCopy()
	secondCN.Name, secondCN.UID, secondCN.ResourceVersion = "worker-b", "container-b", ""
	secondCN.Spec.Node.Name, secondCN.Spec.Node.UID = second.Name, string(second.UID)
	secondCN.Spec.InstanceID, secondCN.Spec.PrivateAddress, secondCN.Spec.AuthorizedGPUUUIDs = "instance-b", "10.0.0.11", []string{"GPU-b"}
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, secondCN); err != nil {
		t.Fatal(err)
	}
	observedAt := now
	r.Now = func() time.Time { return observedAt }
	nodeController := nativenode.Reconciler{Client: c, Now: func() time.Time { return observedAt }}
	clusterController := nativecluster.Reconciler{Client: c, Now: func() time.Time { return observedAt }}
	reconcileAll := func(want native.ClusterPhase, online int32) {
		t.Helper()
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		for _, node := range []*native.Node{nn, second} {
			if _, err := nodeController.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := clusterController.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		var got native.Cluster
		if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != want || int32(got.Status.OnlineNodes) != online {
			t.Fatalf("got phase %s online %d, want %s online %d", got.Status.Phase, got.Status.OnlineNodes, want, online)
		}
	}
	report := func(node *ext.ContainerNode) {
		t.Helper()
		var current ext.ContainerNode
		if err := c.Get(ctx, client.ObjectKeyFromObject(node), &current); err != nil {
			t.Fatal(err)
		}
		current.Status.LastHeartbeatTime = &metav1.Time{Time: observedAt}
		if err := c.Status().Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
	}
	reconcileAll(native.ClusterOnline, 2)
	// A restart/periodic scan cannot extend an observation's original deadline.
	var before coordinationv1.Lease
	leaseKey := types.NamespacedName{Namespace: nn.Namespace, Name: nodeHeartbeatPrefix + nn.Name}
	if err := c.Get(ctx, leaseKey, &before); err != nil {
		t.Fatal(err)
	}
	observedAt = now.Add(30 * time.Second)
	reconcileAll(native.ClusterOnline, 2)
	var after coordinationv1.Lease
	if err := c.Get(ctx, leaseKey, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Spec, after.Spec) {
		t.Fatal("reconciliation extended an old observation")
	}
	observedAt = now.Add(50 * time.Second)
	report(secondCN)
	reconcileAll(native.ClusterDowngraded, 1)
	observedAt = now.Add(100 * time.Second)
	reconcileAll(native.ClusterOffline, 0)
	report(cn)
	report(secondCN)
	reconcileAll(native.ClusterOnline, 2)
	var got native.Node
	if err := c.Get(ctx, client.ObjectKeyFromObject(nn), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != native.NodeOnline || !got.Spec.Unschedulable {
		t.Fatal("recovered node must be online and still cordoned")
	}
}

func TestConnectivityRejectsStaleAndConflictingBindings(t *testing.T) {
	tests := []struct {
		name string
		edit func(*native.Node, *ext.ContainerNode)
	}{
		{"credential rotated without new observation", func(_ *native.Node, n *ext.ContainerNode) { n.Annotations[ext.AgentCredentialAnnotation] = "new-key" }},
		{"missing credential", func(_ *native.Node, n *ext.ContainerNode) { n.Status.AgentCredentialID = "" }},
		{"future heartbeat", func(_ *native.Node, n *ext.ContainerNode) {
			n.Status.LastHeartbeatTime = &metav1.Time{Time: n.Status.LastHeartbeatTime.Add(time.Minute)}
		}},
		{"unobserved generation", func(_ *native.Node, n *ext.ContainerNode) { n.Generation++ }},
		{"not ready", func(_ *native.Node, n *ext.ContainerNode) { n.Status.Conditions[1].Status = metav1.ConditionFalse }},
		{"replacement native Node", func(n *native.Node, _ *ext.ContainerNode) { n.UID = "replacement" }},
		{"cross tenant", func(n *native.Node, _ *ext.ContainerNode) { n.Labels[tenant.IDLabel] = "team-b" }},
		{"conflicting native credential", func(n *native.Node, _ *ext.ContainerNode) {
			n.Annotations = map[string]string{rawnode.CredentialAnnotation: "foreign"}
		}},
		{"conflicting projection owner", func(n *native.Node, _ *ext.ContainerNode) {
			n.Annotations = map[string]string{nativeProjectionAnnotation: "other-companion"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			r, c, nn, cn, cluster := connectivityFixture(t, now)
			tt.edit(nn, cn)
			if err := c.Update(ctx, nn); err != nil {
				t.Fatal(err)
			}
			status := cn.DeepCopy().Status
			if err := c.Update(ctx, cn); err != nil {
				t.Fatal(err)
			}
			cn.Status = status
			if err := c.Status().Update(ctx, cn); err != nil {
				t.Fatal(err)
			}
			// Some invalid bindings are surfaced as conflicts; others simply stop renewal.
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
			var leases coordinationv1.LeaseList
			if err := c.List(ctx, &leases); err != nil {
				t.Fatal(err)
			}
			if len(leases.Items) != 0 {
				t.Fatalf("untrusted binding produced Leases: %#v", leases.Items)
			}
		})
	}
}

func TestConnectivityPartialWriteRecoversAndRotatesCredential(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	r, c, nn, cn, cluster := connectivityFixture(t, now)
	failure := errors.New("status unavailable")
	failed := false
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, base client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if _, ok := obj.(*native.Node); ok && !failed {
			failed = true
			return failure
		}
		return base.SubResource(subresource).Patch(ctx, obj, patch, opts...)
	}})
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
	if _, err := r.Reconcile(ctx, req); !errors.Is(err, failure) {
		t.Fatalf("status failure lost: %v", err)
	}
	var leases coordinationv1.LeaseList
	if err := c.List(ctx, &leases); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 0 {
		t.Fatal("partial capability write authorized a heartbeat")
	}
	// A fresh reconciler observes the partial metadata and completes the status.
	r = &ConnectivityReconciler{Client: c, Now: func() time.Time { return now.Add(time.Second) }}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var current ext.ContainerNode
	if err := c.Get(ctx, client.ObjectKeyFromObject(cn), &current); err != nil {
		t.Fatal(err)
	}
	current.Annotations[ext.AgentCredentialAnnotation] = "new-key"
	if err := c.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	current.Status.AgentCredentialID = "new-key"
	current.Status.LastHeartbeatTime = &metav1.Time{Time: now.Add(time.Second)}
	if err := c.Status().Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got native.Node
	if err := c.Get(ctx, client.ObjectKeyFromObject(nn), &got); err != nil {
		t.Fatal(err)
	}
	var lease coordinationv1.Lease
	if err := c.Get(ctx, types.NamespacedName{Namespace: nn.Namespace, Name: nodeHeartbeatPrefix + nn.Name}, &lease); err != nil {
		t.Fatal(err)
	}
	if !rawnode.HeartbeatMatches(&got, &lease) || got.Annotations[rawnode.CredentialAnnotation] != "new-key" {
		t.Fatal("rotated identity does not match native heartbeat contract")
	}
}
