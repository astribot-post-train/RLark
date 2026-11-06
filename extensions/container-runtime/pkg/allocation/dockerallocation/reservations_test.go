package dockerallocation

import (
	"context"
	"errors"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestCPUOnlyReservationAlsoExcludesOtherTasksUntilCleanup(t *testing.T) {
	m, cluster := allocationFixture(t)
	ctx := context.Background()
	req := Request{Cluster: cluster, JobUID: "job-1", TaskUID: "task-1", TenantID: tenant.DefaultID, CPUMilli: 1000}
	binding, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.JobUID, other.TaskUID = "job-2", "task-2"
	if _, err := m.Reserve(ctx, other); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CPU-only host sharing must wait for cleanup: %v", err)
	}
	if err := m.ReleaseAt(ctx, client.ObjectKey{Namespace: "agents", Name: "docker-1"}, binding, tenant.DefaultID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(ctx, other); err != nil {
		t.Fatal("host was not reusable after confirmed cleanup", err)
	}
}

func TestRecoveryFindsExistingReservationBeforeFreeNodes(t *testing.T) {
	for _, scenario := range []string{"online", "offline", "changed resources"} {
		t.Run(scenario, func(t *testing.T) {
			m, cluster := allocationFixture(t)
			ctx := context.Background()
			req := Request{Cluster: cluster, JobUID: "job-1", TaskUID: "task-1", TenantID: tenant.DefaultID, CPUMilli: 1000}
			binding, err := m.Reserve(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			var reserved api.Node
			if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: binding.NodeName}, &reserved); err != nil {
				t.Fatal(err)
			}
			free := reserved.DeepCopy()
			free.Name, free.UID, free.ResourceVersion = "a-free-node", "free-uid", ""
			free.Status.DockerReservations = nil
			if err := m.Client.Create(ctx, free); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "offline":
				reserved.Status.Phase = api.NodeOffline
				if err := m.Client.Status().Update(ctx, &reserved); err != nil {
					t.Fatal(err)
				}
			case "changed resources":
				req.CPUMilli++
			}
			recovered, err := m.Reserve(ctx, req)
			if scenario == "online" {
				if err != nil || recovered != binding {
					t.Fatalf("recovered=%#v err=%v", recovered, err)
				}
			} else if err == nil {
				t.Fatal("invalid recovery must not allocate a second host")
			}
			if err := m.Client.Get(ctx, client.ObjectKeyFromObject(free), free); err != nil {
				t.Fatal(err)
			}
			if len(free.Status.DockerReservations) != 0 {
				t.Fatal("recovery allocated an additional host")
			}
		})
	}
}

func TestReservationConflictRequiresFreshSnapshot(t *testing.T) {
	m, cluster := allocationFixture(t)
	ctx := context.Background()
	var second api.Node
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "docker-1"}, &second); err != nil {
		t.Fatal(err)
	}
	second.Name, second.UID, second.ResourceVersion = "docker-2", "second-uid", ""
	if err := m.Client.Create(ctx, &second); err != nil {
		t.Fatal(err)
	}
	patches := 0
	m.Client = interceptor.NewClient(m.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			patches++
			return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "nodes"}, obj.GetName(), errors.New("concurrent reservation"))
		},
	})
	_, err := m.Reserve(ctx, Request{Cluster: cluster, JobUID: "job-1", TaskUID: "task-1", TenantID: tenant.DefaultID, CPUMilli: 1000})
	if !apierrors.IsConflict(err) || patches != 1 {
		t.Fatalf("conflict must stop stale allocation: patches=%d err=%v", patches, err)
	}
}
