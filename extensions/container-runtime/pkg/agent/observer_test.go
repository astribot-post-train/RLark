package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func observerFixture(t *testing.T) (Reporter, Identity, *ext.ContainerNode) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ext.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := native.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster", Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	nativeNode := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "native-node", Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	cc := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "agents", UID: "cc", Generation: 1, Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: ext.ContainerClusterSpec{Cluster: ext.NativeObjectReference{Name: "cloud", UID: "cluster"}, Runtime: ext.Runtime("Raw"), AgentNamespace: "agents"}, Status: ext.ContainerClusterStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	node := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "companion", Generation: 1, Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}, Annotations: map[string]string{CredentialAnnotation: "cred-1"}}, Spec: ext.ContainerNodeSpec{Cluster: ext.NativeObjectReference{Name: "cloud", UID: "cluster"}, Node: ext.NativeObjectReference{Namespace: "agents", Name: "worker", UID: "native-node"}, Runtime: ext.Runtime("Raw"), InstanceID: "container-1", PrivateAddress: "10.0.3.85", Environment: "fleet", NetworkScope: "vpc", AuthorizedGPUUUIDs: []string{"GPU-a"}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(cluster, nativeNode, cc, node).Build()
	return Reporter{Client: client, Now: func() time.Time { return time.Unix(100, 0) }}, Identity{Namespace: "agents", Name: "worker", UID: "companion", ClusterUID: "cluster", InstanceID: "container-1", TenantID: tenant.DefaultID, CredentialID: "cred-1"}, node
}

func TestReporterRequiresAuthorizedVisibleGPUAndUpdatesStatus(t *testing.T) {
	r, id, node := observerFixture(t)
	err := r.Report(context.Background(), id, Observation{Version: "v1", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}, Capacity: map[string]string{"nvidia.com/gpu": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	var got ext.ContainerNode
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(node), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.Schedulable || got.Status.AgentVersion != "v1" || got.Status.LastHeartbeatTime == nil || got.Status.AuthorizedGPUUUIDs[0] != "GPU-a" {
		t.Fatalf("unexpected observation: %#v", got.Status)
	}
}

func TestReporterRejectsCredentialRotationAndMissingGPU(t *testing.T) {
	r, id, _ := observerFixture(t)
	if err := r.Report(context.Background(), id, Observation{Version: "v1", PrivateAddress: "10.0.3.85"}); !errors.Is(err, ErrCapability) {
		t.Fatalf("missing GPU was accepted: %v", err)
	}
	id.CredentialID = "old"
	if err := r.Report(context.Background(), id, Observation{Version: "v1", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("stale credential was accepted: %v", err)
	}
}

func TestTargetReleaseFencesSchedulingWithoutHidingHeartbeat(t *testing.T) {
	r, id, node := observerFixture(t)
	var cluster ext.ContainerCluster
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.TargetAgentRelease = "next"
	if err := r.Client.Update(context.Background(), &cluster); err != nil {
		t.Fatal(err)
	}
	// A missing target artifact does not turn a connected Agent into an outage.
	if err := r.Report(context.Background(), id, Observation{Version: "v1", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}); err != nil {
		t.Fatal(err)
	}
	var got ext.ContainerNode
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(node), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Schedulable || got.Status.LastHeartbeatTime == nil || got.Status.AgentVersion != "v1" {
		t.Fatalf("release fence lost liveness: %#v", got.Status)
	}
}
