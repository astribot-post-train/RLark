package rawallocation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fixture(t *testing.T) (Manager, Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster-uid"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
	objects := []client.Object{cluster}
	for _, name := range []string{"a", "b"} {
		objects = append(objects, &api.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents", UID: types.UID(name + "-uid")}, Spec: api.NodeSpec{AgentType: api.AgentTypeRaw}, Status: api.NodeStatus{LastHeartbeatTime: timePtr(metav1.Now()), Phase: api.NodeOnline, Raw: &api.RawNodeStatus{Incarnation: "container-1", TrainingIP: map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}[name], NetworkScope: "private", Environment: "rlinf-v1", AuthorizedGPUUUIDs: []string{"GPU-" + name}}}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(objects...).Build()
	return Manager{Client: c}, Request{Cluster: client.ObjectKey{Name: "cloud"}, JobUID: "job-uid", TenantID: tenant.DefaultID, ExecutionID: "run-1", Environment: "rlinf-v1", Nodes: []client.ObjectKey{{Namespace: "agents", Name: "a"}, {Namespace: "agents", Name: "b"}}}
}

func TestReserveRecoveryAndCleanupFence(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := (Manager{Client: m.Client}).Reserve(ctx, req)
	if err != nil || len(recovered) != 2 || recovered[0] != bindings[0] {
		t.Fatalf("recovery: %v %v", recovered, err)
	}
	other := req
	other.ExecutionID = "another"
	if _, err := m.Reserve(ctx, other); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("competing reserve: %v", err)
	}
	if err := m.Release(ctx, req.Nodes[0], bindings[0], req.TenantID, false); err == nil {
		t.Fatal("released without cleanup")
	}
	if err := m.Release(ctx, req.Nodes[0], bindings[0], "another-tenant", true); err == nil {
		t.Fatal("cross tenant release")
	}
	wrong := bindings[0]
	wrong.NodeUID = "replacement"
	if err := m.Release(ctx, req.Nodes[0], wrong, req.TenantID, true); err == nil {
		t.Fatal("released wrong incarnation")
	}
	if err := m.Release(ctx, req.Nodes[0], bindings[0], req.TenantID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, req.Nodes[0], bindings[0], req.TenantID, true); err != nil {
		t.Fatal("duplicate release", err)
	}
}

func TestUnknownGPUAndMembershipFailClosed(t *testing.T) {
	for _, variant := range []string{"gpu", "namespace", "duplicate", "tenant", "replacement", "offline", "cordon", "overlap"} {
		t.Run(variant, func(t *testing.T) {
			m, req := fixture(t)
			ctx := context.Background()
			var n api.Node
			if err := m.Client.Get(ctx, req.Nodes[0], &n); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "gpu":
				n.Status.Raw.AuthorizedGPUUUIDs = nil
			case "namespace":
				req.Nodes[0].Namespace = "other"
			case "duplicate":
				req.Nodes[1] = req.Nodes[0]
			case "tenant":
				req.TenantID = "other"
			case "replacement":
				n.Status.Raw.Incarnation = ""
			case "offline":
				n.Status.Phase = api.NodeOffline
			case "cordon":
				var c api.Cluster
				if err := m.Client.Get(ctx, req.Cluster, &c); err != nil {
					t.Fatal(err)
				}
				c.Spec.Cordoned = true
				if err := m.Client.Update(ctx, &c); err != nil {
					t.Fatal(err)
				}
			case "overlap":
				if err := m.Client.Create(ctx, &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "other-uid"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Client.Status().Update(ctx, &n); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Reserve(ctx, req); err == nil {
				t.Fatal("unsafe reservation accepted")
			}
		})
	}
}

func TestPartialAcquireRollsBackAndDoesNotTouchOtherOwner(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	var b api.Node
	if err := m.Client.Get(ctx, req.Nodes[1], &b); err != nil {
		t.Fatal(err)
	}
	b.Status.Reservation = &api.RawNodeReservation{JobUID: "someone-else", ExecutionID: "run-other", Committed: true}
	if err := m.Client.Status().Update(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(ctx, req); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	var a api.Node
	if err := m.Client.Get(ctx, req.Nodes[0], &a); err != nil {
		t.Fatal(err)
	}
	if a.Status.Reservation != nil {
		t.Fatal("partial reservation leaked")
	}
	if err := m.Client.Get(ctx, req.Nodes[1], &b); err != nil {
		t.Fatal(err)
	}
	if b.Status.Reservation.JobUID != "someone-else" {
		t.Fatal("changed another owner")
	}
}

func TestConcurrentExecutionsHaveAtMostOneWinner(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			r := req
			r.ExecutionID = id
			_, err := m.Reserve(ctx, r)
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins > 1 {
		t.Fatal("concurrent owners both won")
	}
}

func TestContainerReplacementCannotRunOldBinding(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var n api.Node
	if err := m.Client.Get(ctx, req.Nodes[0], &n); err != nil {
		t.Fatal(err)
	}
	n.Status.Raw.Incarnation = "new-container"
	if err := ValidateBinding(&n, &bindings[0], req.TenantID); err == nil {
		t.Fatal("replacement accepted old assignment")
	}
}

func TestGroupBarrierRejectsSubsetsAndUncommittedNodes(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings[:1]); err == nil {
		t.Fatal("subset bypassed group barrier")
	}
	var node api.Node
	if err := m.Client.Get(ctx, req.Nodes[1], &node); err != nil {
		t.Fatal(err)
	}
	node.Status.Reservation.Committed = false
	if err := m.Client.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings); err == nil {
		t.Fatal("partial commit passed barrier")
	}
	recovered, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, recovered); err != nil {
		t.Fatal(err)
	}
}

func TestSharedNetworkAddressFailsClosed(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	var node api.Node
	if err := m.Client.Get(ctx, req.Nodes[1], &node); err != nil {
		t.Fatal(err)
	}
	node.Status.Raw.TrainingIP = "10.0.0.1"
	if err := m.Client.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(ctx, req); err == nil {
		t.Fatal("shared address accepted without port isolation")
	}
}

func timePtr(t metav1.Time) *metav1.Time { return &t }

func TestStaleHeartbeatRetainsReservationButBlocksDispatch(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale dispatch: %v", err)
	}
	var node api.Node
	if err := m.Client.Get(ctx, req.Nodes[0], &node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Reservation == nil {
		t.Fatal("disconnect released reservation")
	}
	if _, err := m.Reserve(ctx, req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale reservation: %v", err)
	}
}

func TestClusterCordonAfterReservationBlocksDispatch(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var cluster api.Cluster
	if err := m.Client.Get(ctx, req.Cluster, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Cordoned = true
	if err := m.Client.Update(ctx, &cluster); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cordoned dispatch: %v", err)
	}
}

func TestAssignTaskIsRecoverableAndImmutable(t *testing.T) {
	m, req := fixture(t)
	ctx := context.Background()
	bindings, err := m.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := m.AssignTask(ctx, req.Nodes[0], bindings[0], req.TenantID, "task-uid")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := (Manager{Client: m.Client}).AssignTask(ctx, req.Nodes[0], bindings[0], req.TenantID, "task-uid")
	if err != nil || recovered != assigned {
		t.Fatalf("assignment recovery: %+v %v", recovered, err)
	}
	if _, err := m.AssignTask(ctx, req.Nodes[0], bindings[0], req.TenantID, "other"); err == nil {
		t.Fatal("reassigned occupied reservation")
	}
	forged := bindings[1]
	forged.TaskUID = "other"
	if _, err := m.AssignTask(ctx, req.Nodes[1], forged, req.TenantID, "task-b"); err == nil {
		t.Fatal("accepted foreign binding")
	}
	recoveredGroup, err := m.Reserve(ctx, req)
	if err != nil || recoveredGroup[0].TaskUID != "task-uid" {
		t.Fatalf("allocation recovery lost assignment: %+v %v", recoveredGroup, err)
	}
}

func TestGroupCapabilityDriftAndDuplicateGPUFailClosed(t *testing.T) {
	for _, variant := range []string{"gpu", "address", "network", "runtime", "environment"} {
		t.Run(variant, func(t *testing.T) {
			m, req := fixture(t)
			ctx := context.Background()
			bindings, err := m.Reserve(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			var node api.Node
			if err := m.Client.Get(ctx, req.Nodes[1], &node); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "gpu":
				node.Status.Raw.AuthorizedGPUUUIDs = []string{"GPU-a"}
			case "address":
				node.Status.Raw.TrainingIP = "10.0.0.1"
			case "network":
				node.Status.Raw.NetworkScope = "other"
			case "environment":
				node.Status.Raw.Environment = "other"
			case "runtime":
				node.Spec.AgentType = api.AgentTypeKubernetes
			}
			if variant == "runtime" {
				err = m.Client.Update(ctx, &node)
			} else {
				err = m.Client.Status().Update(ctx, &node)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := m.ValidateGroup(ctx, "agents", req.TenantID, bindings); err == nil {
				t.Fatal("dispatch accepted capability drift")
			}
			if _, err := m.Reserve(ctx, req); err == nil {
				t.Fatal("reservation accepted capability drift")
			}
		})
	}
}
