package controller

import (
	"context"
	"testing"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := native.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := containerapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestContainerClusterReconcilerChecksUIDAndNamespace(t *testing.T) {
	ctx := context.Background()
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: "native-uid"}, Spec: native.ClusterSpec{AgentNamespace: "raw-agents"}}
	companion := &containerapi.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: "raw-agents", UID: "binding"}, Spec: containerapi.ContainerClusterSpec{Cluster: containerapi.NativeObjectReference{Name: "gpu", UID: "native-uid"}, Runtime: "Raw", AgentNamespace: "raw-agents"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(companion).WithObjects(cluster, companion).Build()
	r := ContainerClusterReconciler{Client: c}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: "raw-agents"}}); err != nil {
		t.Fatal(err)
	}
	var got containerapi.ContainerCluster
	if err := c.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: "raw-agents"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Configured" || got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("valid binding not observed: %#v", got.Status)
	}
	got.Spec.Cluster.UID = "replacement"
	if err := c.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: "raw-agents"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: "raw-agents"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Invalid" || got.Status.Conditions[0].Reason != "InvalidBinding" {
		t.Fatalf("stale binding was not rejected: %#v", got.Status)
	}
}

func TestContainerNodeRegistrationNeverGrantsScheduling(t *testing.T) {
	ctx := context.Background()
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: "cluster-uid"}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	node := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "node-uid"}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	clusterView := &containerapi.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "raw-view", Namespace: "agents", UID: "view-uid"}, Spec: containerapi.ContainerClusterSpec{Cluster: containerapi.NativeObjectReference{Name: "gpu", UID: "cluster-uid"}, Runtime: "Raw", AgentNamespace: "agents"}}
	nodeView := &containerapi.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "raw-worker", Namespace: "agents"}, Spec: containerapi.ContainerNodeSpec{Cluster: containerapi.NativeObjectReference{Name: "gpu", UID: "cluster-uid"}, Node: containerapi.NativeObjectReference{Namespace: "agents", Name: "worker", UID: "node-uid"}, Runtime: "Raw", InstanceID: "container-1"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(nodeView).WithObjects(cluster, node, clusterView, nodeView).Build()
	r := ContainerNodeReconciler{Client: c}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "raw-worker", Namespace: "agents"}})
	if err != nil {
		t.Fatal(err)
	}
	var got containerapi.ContainerNode
	if err := c.Get(ctx, types.NamespacedName{Name: "raw-worker", Namespace: "agents"}, &got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if got.Status.Schedulable || ready == nil || ready.Reason != "AgentNotObserved" {
		t.Fatalf("registration was incorrectly treated as schedulable: %#v", got.Status)
	}
}

func TestJobOwnerRequiresOneCurrentUIDAndTenantBinding(t *testing.T) {
	ctx := context.Background()
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: "job-uid", Labels: map[string]string{tenant.IDLabel: "team-a"}, Annotations: map[string]string{"rlark.io/controller-owner": containerapi.Group}}}
	binding := &containerapi.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "control", Labels: map[string]string{tenant.IDLabel: "team-a"}}, Spec: containerapi.ContainerJobSpec{Job: containerapi.NativeObjectReference{Name: "train", UID: "job-uid"}, TenantID: "team-a", Runtime: "Raw"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(binding).Build()
	owner := JobOwner{Client: c}
	if _, err := owner.Owns(ctx, job); err == nil {
		t.Fatal("unvalidated binding claimed native Job")
	}
	binding.Generation = 2
	binding.Status.ObservedGeneration = 2
	binding.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 2}}
	if err := c.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	owned, err := owner.Owns(ctx, job)
	if err != nil || !owned {
		t.Fatalf("valid extension binding not recognized: owned=%v err=%v", owned, err)
	}
	job.Labels[tenant.IDLabel] = "team-b"
	if _, err := owner.Owns(ctx, job); err == nil {
		t.Fatal("cross-tenant binding accepted")
	}
	job.Labels[tenant.IDLabel] = "team-a"
	binding.Spec.Job.UID = "old-job-uid"
	if err := c.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Owns(ctx, job); err == nil {
		t.Fatal("stale UID binding accepted")
	}
}

func TestAgentReleaseDigestMustBeSHA256(t *testing.T) {
	ctx := context.Background()
	release := &containerapi.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "control"}, Spec: containerapi.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: "amd64", SHA256: "bad", ArtifactPath: "agent", Protocol: "v1"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(release).WithObjects(release).Build()
	r := AgentReleaseReconciler{Client: c}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "v1", Namespace: "control"}}); err != nil {
		t.Fatal(err)
	}
	var got containerapi.AgentRelease
	if err := c.Get(ctx, types.NamespacedName{Name: "v1", Namespace: "control"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Available || got.Status.Conditions[0].Reason != "InvalidArtifact" {
		t.Fatalf("invalid artifact became available: %#v", got.Status)
	}
}
