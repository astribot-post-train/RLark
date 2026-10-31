package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(t *testing.T) (Planner, Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := native.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := ext.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster"}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job", Generation: 1, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group}}}
	clusterRef := ext.NativeObjectReference{Name: cluster.Name, UID: string(cluster.UID)}
	binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "agents", UID: "binding"}, Spec: ext.ContainerJobSpec{Cluster: clusterRef, Job: ext.NativeObjectReference{Name: job.Name, UID: string(job.UID)}, TenantID: tenant.DefaultID, Runtime: "Raw"}}
	task := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "task", Generation: 1, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, native.GroupVersion.WithKind("Job"))}}, Spec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Placement: native.TaskPlacementSpec{Environment: "prepared"}}}
	cc := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "agents", UID: "cc"}, Spec: ext.ContainerClusterSpec{Cluster: clusterRef, Runtime: "Raw", AgentNamespace: "agents"}}
	objects := []client.Object{cluster, job, binding, task, cc}
	for i, name := range []string{"a", "b"} {
		node := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents", UID: types.UID("node-" + name)}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
		cn := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents", UID: types.UID("cn-" + name), Generation: 1, Annotations: map[string]string{ext.AgentCredentialAnnotation: "credential"}}, Spec: ext.ContainerNodeSpec{Cluster: clusterRef, Node: ext.NativeObjectReference{Name: name, Namespace: "agents", UID: string(node.UID)}, Runtime: "Raw", InstanceID: "instance-" + name, PrivateAddress: fmt.Sprintf("10.0.3.%d", 85+i), Environment: "prepared", NetworkScope: "cloud", AuthorizedGPUUUIDs: []string{"GPU-" + name}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, AgentCredentialID: "credential", Schedulable: true, LastHeartbeatTime: &metav1.Time{Time: now}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
		objects = append(objects, node, cn)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ext.ContainerAllocation{}, &ext.ContainerNode{}, task).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*ext.ContainerAllocation); ok {
			obj.SetUID(types.UID("allocation-" + obj.GetName()))
			obj.SetGeneration(1)
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return Planner{Allocator: companion.Manager{Client: c, Now: func() time.Time { return now }}}, Request{Binding: client.ObjectKeyFromObject(binding), Task: client.ObjectKeyFromObject(task)}
}

func TestPlannerSkipsUnavailableNodeAndRecoversPinnedTarget(t *testing.T) {
	ctx := context.Background()
	p, req := fixture(t)
	c := p.Allocator.Client
	var first ext.ContainerNode
	if err := c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "a"}, &first); err != nil {
		t.Fatal(err)
	}
	first.Status.Busy = true
	if err := c.Status().Update(ctx, &first); err != nil {
		t.Fatal(err)
	}
	a, err := p.Acquire(ctx, req)
	if err != nil || a.Spec.Node.Name != "b" {
		t.Fatalf("did not skip busy node: %+v %v", a, err)
	}
	// Heartbeat loss must not cause a second node to be chosen on restart.
	var second ext.ContainerNode
	if err := c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "b"}, &second); err != nil {
		t.Fatal(err)
	}
	second.Status.LastHeartbeatTime = nil
	if err := c.Status().Update(ctx, &second); err != nil {
		t.Fatal(err)
	}
	recovered, err := (Planner{Allocator: p.Allocator}).Acquire(ctx, req)
	if err != nil || recovered.UID != a.UID {
		t.Fatalf("did not recover pinned allocation after restart: %+v %v", recovered, err)
	}
}

func TestPlannerNeverFallsBackAfterUncertainCreate(t *testing.T) {
	ctx := context.Background()
	p, req := fixture(t)
	base := p.Allocator.Client
	p.Allocator.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		return errors.New("accepted Create response lost")
	}})
	if _, err := p.Acquire(ctx, req); err == nil {
		t.Fatal("expected lost Create response")
	}
	var locks ext.ContainerAllocationList
	if err := base.List(ctx, &locks); err != nil || len(locks.Items) != 1 {
		t.Fatalf("uncertain result selected another node: %d %v", len(locks.Items), err)
	}
	p.Allocator.Client = base
	a, err := p.Acquire(ctx, req)
	if err != nil || a.UID != locks.Items[0].UID {
		t.Fatalf("lost response was not recovered: %+v %v", a, err)
	}
}

func TestPlannerPreservesSelectorAndMalformedTargets(t *testing.T) {
	ctx := context.Background()
	p, req := fixture(t)
	var task native.Task
	if err := p.Allocator.Client.Get(ctx, req.Task, &task); err != nil {
		t.Fatal(err)
	}
	task.Spec.Placement.NodeSelector = map[string]string{"pool": "missing"}
	if err := p.Allocator.Client.Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(ctx, req); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("selector was ignored: %v", err)
	}
	task.Annotations = map[string]string{companion.TargetAnnotation: `{}`}
	if err := p.Allocator.Client.Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(ctx, req); !errors.Is(err, companion.ErrIdentity) {
		t.Fatalf("malformed target was ignored: %v", err)
	}
}
