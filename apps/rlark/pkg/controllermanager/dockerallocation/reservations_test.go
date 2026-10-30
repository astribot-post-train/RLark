package dockerallocation

import (
	"context"
	"errors"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func allocationFixture(t *testing.T) (Manager, client.ObjectKey) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("cluster-uid")}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "docker-1", Namespace: "agents", UID: types.UID("node-uid")}, Spec: api.NodeSpec{AgentType: api.AgentTypeDocker}, Status: api.NodeStatus{Phase: api.NodeOnline, NodeInfo: api.NodeInfo{Incarnation: "engine-1"}, Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}, Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}}}
	tenant.SetIdentity(cluster, tenant.DefaultID, "test")
	tenant.SetIdentity(node, tenant.DefaultID, "test")
	return Manager{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(cluster, node).Build()}, client.ObjectKey{Name: "cluster"}
}

func TestReserveIsRecoverableAndDoesNotOvercommitGPU(t *testing.T) {
	m, cluster := allocationFixture(t)
	ctx := context.Background()
	req := Request{Cluster: cluster, JobUID: "job-1", TaskUID: "task-1", TenantID: tenant.DefaultID, GPUCount: 1}
	binding, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := m.Reserve(ctx, req)
	if err != nil || recovered != binding {
		t.Fatalf("recovery binding=%#v err=%v", recovered, err)
	}
	other := req
	other.JobUID, other.TaskUID = "job-2", "task-2"
	if _, err := m.Reserve(ctx, other); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("overcommit err=%v", err)
	}
	if err := m.ReleaseAt(ctx, client.ObjectKey{Namespace: "agents", Name: "docker-1"}, binding, tenant.DefaultID); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseAt(ctx, client.ObjectKey{Namespace: "agents", Name: "docker-1"}, binding, tenant.DefaultID); err != nil {
		t.Fatal("release should be idempotent", err)
	}
}
