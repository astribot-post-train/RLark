package companion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(t *testing.T) (Manager, Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ext.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := native.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: "cluster-uid"}, Spec: native.ClusterSpec{AgentNamespace: "agents"}, Status: native.ClusterStatus{Phase: native.ClusterOnline}}
	nativeNode := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "node-uid"}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "training", UID: "job-uid", Generation: 1, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group}}}
	task := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: "training-worker", Namespace: "agents", UID: "task-uid", Generation: 1, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, native.GroupVersion.WithKind("Job"))}}, Spec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Placement: native.TaskPlacementSpec{Environment: "fleet"}}}
	cc := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: "agents", UID: "cc-uid"}, Spec: ext.ContainerClusterSpec{Cluster: reference(cluster), Runtime: "Raw", AgentNamespace: "agents"}}
	binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: "training", Namespace: "agents", UID: "binding-uid"}, Spec: ext.ContainerJobSpec{Cluster: reference(cluster), Job: reference(job), TenantID: tenant.DefaultID, Runtime: "Raw"}}
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	node := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "companion-node-uid", Generation: 1, Annotations: map[string]string{ext.AgentCredentialAnnotation: "credential"}}, Spec: ext.ContainerNodeSpec{Cluster: reference(cluster), Node: reference(nativeNode), Runtime: "Raw", InstanceID: "container-1", PrivateAddress: "10.0.3.85", Environment: "fleet", NetworkScope: "ksyun-vpc", AuthorizedGPUUUIDs: []string{"GPU-a"}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, AgentCredentialID: "credential", Schedulable: true, LastHeartbeatTime: &metav1.Time{Time: now}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ext.ContainerAllocation{}, node, nativeNode, task, job).WithObjects(cluster, nativeNode, job, task, cc, binding, node).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*ext.ContainerAllocation); ok {
			obj.SetUID(types.UID("allocation-" + obj.GetName()))
			obj.SetGeneration(1)
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return Manager{Client: c, Now: func() time.Time { return now }}, Request{Binding: client.ObjectKeyFromObject(binding), Node: client.ObjectKeyFromObject(node), Task: client.ObjectKeyFromObject(task)}
}

func TestValidateExistingNeverCreatesOrRepairs(t *testing.T) {
	ctx := context.Background()
	m, req := fixture(t)
	if _, err := m.ValidateExisting(ctx, req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing reservation accepted: %v", err)
	}
	var allocations ext.ContainerAllocationList
	if err := m.Client.List(ctx, &allocations); err != nil || len(allocations.Items) != 0 {
		t.Fatalf("validation allocated capacity: %v", err)
	}
	a, err := m.Acquire(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := m.ValidateExisting(ctx, req)
	if err != nil || recovered.UID != a.UID {
		t.Fatalf("existing allocation did not validate: %v", err)
	}
	var task native.Task
	if err := m.Client.Get(ctx, req.Task, &task); err != nil {
		t.Fatal(err)
	}
	delete(task.Annotations, TargetAnnotation)
	if err := m.Client.Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	before := task.ResourceVersion
	if _, err := m.ValidateExisting(ctx, req); !errors.Is(err, ErrIdentity) {
		t.Fatalf("lost target accepted: %v", err)
	}
	if err := m.Client.Get(ctx, req.Task, &task); err != nil {
		t.Fatal(err)
	}
	if task.ResourceVersion != before || task.Annotations[TargetAnnotation] != "" {
		t.Fatal("validation repaired controller-owned target")
	}
}

func TestTargetReleaseFenceRejectsForeignAndInvalidAvailableRelease(t *testing.T) {
	for _, mode := range []string{"valid", "foreign tenant", "invalid protocol", "stale generation", "missing UID"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m, req := fixture(t)
			var cc ext.ContainerCluster
			if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "gpu"}, &cc); err != nil {
				t.Fatal(err)
			}
			cc.Spec.TargetAgentRelease = "v1"
			if err := m.Client.Update(ctx, &cc); err != nil {
				t.Fatal(err)
			}
			var node ext.ContainerNode
			if err := m.Client.Get(ctx, req.Node, &node); err != nil {
				t.Fatal(err)
			}
			node.Status.AgentVersion = "v1"
			if err := m.Client.Status().Update(ctx, &node); err != nil {
				t.Fatal(err)
			}
			release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "agents", UID: "release-uid", Generation: 1, Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: ext.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: "amd64", Protocol: "v1", ArtifactPath: "agent", SHA256: strings.Repeat("0", 64)}, Status: ext.AgentReleaseStatus{Available: true, ObservedGeneration: 1}}
			switch mode {
			case "foreign tenant":
				release.Labels[tenant.IDLabel] = "other"
			case "invalid protocol":
				release.Spec.Protocol = "v2"
			case "stale generation":
				release.Generation = 2
			case "missing UID":
				release.UID = ""
			}
			if err := m.Client.Create(ctx, release); err != nil {
				t.Fatal(err)
			}
			_, err := m.Acquire(ctx, req)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid release accepted: %v", err)
			}
			var allocations ext.ContainerAllocationList
			if err := m.Client.List(ctx, &allocations); err != nil {
				t.Fatal(err)
			}
			if len(allocations.Items) != 0 {
				t.Fatal("fenced release acquired capacity")
			}
		})
	}
}
func TestExclusiveAcquireRecoveryAndConfirmedRelease(t *testing.T) {
	ctx := context.Background()
	m, req := fixture(t)
	first, err := m.Acquire(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := (Manager{Client: m.Client, Now: m.Now}).Acquire(ctx, req)
	if err != nil || first.UID != recovered.UID {
		t.Fatalf("restart did not recover exact allocation: %v", err)
	}
	var node native.Node
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Reservation != nil || len(node.Status.DockerReservations) != 0 {
		t.Fatal("companion allocator wrote native reservation")
	}
	var task native.Task
	if err := m.Client.Get(ctx, req.Task, &task); err != nil {
		t.Fatal(err)
	}
	other := task.DeepCopy()
	other.Name = "second-task"
	other.UID = "task-2"
	other.ResourceVersion = ""
	if err := m.Client.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	secondReq := req
	secondReq.Task = client.ObjectKeyFromObject(other)
	if _, err := m.Acquire(ctx, secondReq); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("competing Task acquired same node: %v", err)
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(first), string(first.UID)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown cleanup released allocation: %v", err)
	}
	first.Status.CleanupConfirmed = true
	first.Status.ObservedGeneration = first.Generation
	first.Status.Conditions = []metav1.Condition{{Type: "CleanupConfirmed", Status: metav1.ConditionTrue, ObservedGeneration: first.Spec.Generation}}
	if err := m.Client.Status().Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(first), "stale-uid"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("stale allocation UID released lock: %v", err)
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(first), string(first.UID)); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(first), string(first.UID)); err != nil {
		t.Fatalf("duplicate release failed: %v", err)
	}
	if err := m.Client.Get(ctx, client.ObjectKeyFromObject(first), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("lock remains after confirmed cleanup: %v", err)
	}
	var pinned native.Task
	if err := m.Client.Get(ctx, req.Task, &pinned); err != nil {
		t.Fatal(err)
	}
	target, err := Target(&pinned)
	if err != nil || target == nil || target.ExecutionID != string(pinned.UID) || target.NodeRecordUID == "" {
		t.Fatalf("allocation target was not durably pinned: %#v %v", target, err)
	}
}

func TestMalformedOrConflictingTargetFailsClosed(t *testing.T) {
	ctx := context.Background()
	m, req := fixture(t)
	var task native.Task
	mustGet(t, m, req.Task, &task)
	task.Annotations = map[string]string{TargetAnnotation: `{"task":{"name":"wrong"}}`}
	if err := m.Client.Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, req); !errors.Is(err, ErrIdentity) {
		t.Fatalf("malformed target was accepted: %v", err)
	}
}

func addSecondNode(t *testing.T, m Manager, req Request) Request {
	t.Helper()
	ctx := context.Background()
	var node ext.ContainerNode
	mustGet(t, m, req.Node, &node)
	var nativeNode native.Node
	mustGet(t, m, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nativeNode)
	nativeNode.Name, nativeNode.UID, nativeNode.ResourceVersion = "other", "other-native", ""
	if err := m.Client.Create(ctx, &nativeNode); err != nil {
		t.Fatal(err)
	}
	node.Name, node.UID, node.ResourceVersion = "other", "other-companion", ""
	node.Spec.Node = reference(&nativeNode)
	node.Spec.PrivateAddress = "10.0.3.86"
	node.Spec.AuthorizedGPUUUIDs = []string{"GPU-b"}
	if err := m.Client.Create(ctx, &node); err != nil {
		t.Fatal(err)
	}
	other := req
	other.Node = client.ObjectKeyFromObject(&node)
	return other
}

func TestConcurrentSameTaskDifferentNodesHasOneTarget(t *testing.T) {
	ctx := context.Background()
	m, first := fixture(t)
	second := addSecondNode(t, m, first)
	// Both requests finish eligibility reads before either target CAS. This is
	// the cross-node race a per-node lock alone cannot prevent.
	var mu sync.Mutex
	arrivals := 0
	proceed := make(chan struct{})
	base := m.Client
	m.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*native.Task); ok {
			mu.Lock()
			arrivals++
			if arrivals == 2 {
				close(proceed)
			}
			mu.Unlock()
			select {
			case <-proceed:
			case <-time.After(5 * time.Second):
				return errors.New("target CAS barrier timed out")
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	out := make(chan error, 2)
	for _, req := range []Request{first, second} {
		go func(r Request) { _, err := m.Acquire(ctx, r); out <- err }(req)
	}
	wins := 0
	for range 2 {
		if err := <-out; err == nil {
			wins++
		} else if !apierrors.IsConflict(err) {
			t.Errorf("unexpected loser error: %v", err)
		}
	}
	var locks ext.ContainerAllocationList
	if err := base.List(ctx, &locks); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || len(locks.Items) != 1 {
		t.Fatalf("one Task reserved multiple nodes: wins=%d locks=%d", wins, len(locks.Items))
	}
	var task native.Task
	mustGet(t, m, first.Task, &task)
	target, err := Target(&task)
	if err != nil || target == nil || target.Node != locks.Items[0].Spec.Node {
		t.Fatalf("lock does not match persisted target: %+v %v", target, err)
	}
}

func TestLostTargetPatchCannotAllocateOrRetarget(t *testing.T) {
	ctx := context.Background()
	m, req := fixture(t)
	other := addSecondNode(t, m, req)
	base := m.Client
	m.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		return errors.New("target persisted but response lost")
	}})
	if _, err := m.Acquire(ctx, req); err == nil {
		t.Fatal("expected lost target patch response")
	}
	var locks ext.ContainerAllocationList
	if err := base.List(ctx, &locks); err != nil || len(locks.Items) != 0 {
		t.Fatalf("uncertain target patch was followed by allocation: %d %v", len(locks.Items), err)
	}
	m.Client = base
	if _, err := m.Acquire(ctx, other); !errors.Is(err, ErrIdentity) {
		t.Fatalf("uncertain target was reassigned: %v", err)
	}
	if _, err := m.Acquire(ctx, req); err != nil {
		t.Fatalf("persisted target could not recover: %v", err)
	}
}
func TestUnavailableAndConflictingInputsNeverCreateAllocation(t *testing.T) {
	tests := map[string]func(*testing.T, Manager, Request){
		"heartbeat expired": func(t *testing.T, m Manager, r Request) {
			var n ext.ContainerNode
			mustGet(t, m, r.Node, &n)
			old := metav1.NewTime(m.now().Add(-time.Hour))
			n.Status.LastHeartbeatTime = &old
			if err := m.Client.Status().Update(context.Background(), &n); err != nil {
				t.Fatal(err)
			}
		},
		"cross tenant": func(t *testing.T, m Manager, r Request) {
			var n ext.ContainerNode
			mustGet(t, m, r.Node, &n)
			tenant.SetIdentity(&n, "other", "")
			if err := m.Client.Update(context.Background(), &n); err != nil {
				t.Fatal(err)
			}
		},
		"instance replaced": func(t *testing.T, m Manager, r Request) {
			var n ext.ContainerNode
			mustGet(t, m, r.Node, &n)
			n.Spec.Node.UID = "replaced"
			if err := m.Client.Update(context.Background(), &n); err != nil {
				t.Fatal(err)
			}
		},
		"cordoned": func(t *testing.T, m Manager, r Request) {
			var n native.Node
			mustGet(t, m, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n)
			n.Spec.Unschedulable = true
			if err := m.Client.Update(context.Background(), &n); err != nil {
				t.Fatal(err)
			}
		},
		"duplicate GPUs": func(t *testing.T, m Manager, r Request) {
			var n ext.ContainerNode
			mustGet(t, m, r.Node, &n)
			n.Name = "other"
			n.UID = "other-node"
			n.ResourceVersion = ""
			n.Spec.Node.UID = "other-native"
			n.Spec.Node.Name = "other"
			n.Spec.PrivateAddress = "10.0.3.86"
			if err := m.Client.Create(context.Background(), &n); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m, r := fixture(t)
			mutate(t, m, r)
			if _, err := m.Acquire(context.Background(), r); err == nil {
				t.Fatal("invalid allocation accepted")
			}
			var list ext.ContainerAllocationList
			if err := m.Client.List(context.Background(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Fatal("invalid request created a lock")
			}
		})
	}
}
func mustGet(t *testing.T, m Manager, key client.ObjectKey, obj client.Object) {
	t.Helper()
	if err := m.Client.Get(context.Background(), key, obj); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentAcquisitionHasOneWinner(t *testing.T) {
	ctx := context.Background()
	m, r := fixture(t)
	var base native.Task
	mustGet(t, m, r.Task, &base)
	const competitors = 12
	requests := make([]Request, competitors)
	for i := range requests {
		task := base.DeepCopy()
		task.Name = fmt.Sprintf("competitor-%d", i)
		task.UID = types.UID(task.Name)
		task.ResourceVersion = ""
		if err := m.Client.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		requests[i] = r
		requests[i].Task = client.ObjectKeyFromObject(task)
	}
	var wg sync.WaitGroup
	out := make(chan bool, competitors)
	for _, request := range requests {
		wg.Add(1)
		go func(req Request) { defer wg.Done(); _, err := m.Acquire(ctx, req); out <- err == nil }(request)
	}
	wg.Wait()
	close(out)
	wins := 0
	for won := range out {
		if won {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("got %d successful allocations, want exactly one", wins)
	}
}

// lostCreateClient models an accepted API Create whose response is lost.
// Recovery must observe the durable object and must not choose another node.
type lostCreateClient struct {
	client.Client
	lost bool
}

func (c *lostCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*ext.ContainerAllocation); ok && !c.lost {
		c.lost = true
		return errors.New("API response lost after successful Create")
	}
	return nil
}
func TestLostCreateResponseRecoversOneDurableLock(t *testing.T) {
	ctx := context.Background()
	m, r := fixture(t)
	c := &lostCreateClient{Client: m.Client}
	m.Client = c
	if _, err := m.Acquire(ctx, r); err == nil {
		t.Fatal("expected lost response")
	}
	recovered, err := (Manager{Client: c.Client, Now: m.Now}).Acquire(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var locks ext.ContainerAllocationList
	if err := c.List(ctx, &locks); err != nil {
		t.Fatal(err)
	}
	if len(locks.Items) != 1 || locks.Items[0].UID != recovered.UID {
		t.Fatalf("recovery created extra or different lock: %#v", locks.Items)
	}
	var node ext.ContainerNode
	mustGet(t, m, r.Node, &node)
	node.Status.LastHeartbeatTime = nil
	node.Status.Schedulable = false
	if err := c.Client.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(recovered), string(recovered.UID)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Agent loss released an unconfirmed allocation: %v", err)
	}
}
func TestDanglingAllocationBlocksReusedGPUOnNewNode(t *testing.T) {
	ctx := context.Background()
	m, r := fixture(t)
	orphan := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "old-node", Namespace: "removed-cluster", UID: "orphan-allocation", Finalizers: []string{Finalizer}}, Spec: ext.ContainerAllocationSpec{Node: ext.NativeObjectReference{Name: "removed", UID: "old-native-uid"}, GPUUUIDs: []string{"GPU-a"}}}
	if err := m.Client.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, r); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reused GPU ignored an unresolved old allocation: %v", err)
	}
}

func TestReleaseWaitsForObservedDeletionDespiteOtherFinalizers(t *testing.T) {
	ctx := context.Background()
	m, r := fixture(t)
	a, err := m.Acquire(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	a.Finalizers = append(a.Finalizers, "example.io/other-owner")
	if err := m.Client.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status.CleanupConfirmed = true
	a.Status.ObservedGeneration = a.Generation
	a.Status.Conditions = []metav1.Condition{{Type: "CleanupConfirmed", Status: metav1.ConditionTrue, ObservedGeneration: a.Generation}}
	if err := m.Client.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(a)
	for range 2 {
		if err := m.Release(ctx, key, string(a.UID)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted delete was mistaken for completed deletion: %v", err)
		}
	}
	var pending ext.ContainerAllocation
	mustGet(t, m, key, &pending)
	if pending.DeletionTimestamp.IsZero() || len(pending.Finalizers) != 1 || pending.Finalizers[0] != "example.io/other-owner" {
		t.Fatalf("modified another finalizer or failed to delete: %#v", pending.ObjectMeta)
	}
}
