package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func jobFixture(t *testing.T) (JobReservationReconciler, ctrl.Request) {
	t.Helper()
	now := time.Now()
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster"}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	ref := ext.NativeObjectReference{Name: cluster.Name, UID: string(cluster.UID)}
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job", Generation: 1, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group}}, Spec: native.JobSpec{Tasks: []native.JobTaskTemplate{{Name: "worker", TaskSpec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Placement: native.TaskPlacementSpec{Environment: "prepared"}, Workload: native.TaskWorkloadSpec{Kind: "raw.rlark.io/process", Version: "v1", Parameters: extensions.JSON{Raw: []byte(`{"argv":["/usr/bin/python3","train.py"],"env":[],"directory":"/work"}`)}, Carrier: native.TaskWorkloadCarrier{Type: "raw.rlark.io/process", Version: "v1"}}}}}}}
	binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "agents", UID: "binding"}, Spec: ext.ContainerJobSpec{Cluster: ref, Job: ext.NativeObjectReference{Name: job.Name, UID: string(job.UID)}, TenantID: tenant.DefaultID, Runtime: "Raw"}}
	cc := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "agents", UID: "cc"}, Spec: ext.ContainerClusterSpec{Cluster: ref, Runtime: "Raw", AgentNamespace: "agents"}}
	node := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "native-node"}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	cn := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "companion-node", Generation: 1, Annotations: map[string]string{ext.AgentCredentialAnnotation: "credential"}}, Spec: ext.ContainerNodeSpec{Cluster: ref, Node: ext.NativeObjectReference{Name: node.Name, Namespace: node.Namespace, UID: string(node.UID)}, Runtime: "Raw", InstanceID: "container", PrivateAddress: "10.0.3.85", Environment: "prepared", NetworkScope: "cloud", AuthorizedGPUUUIDs: []string{"GPU-a"}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, AgentCredentialID: "credential", Schedulable: true, LastHeartbeatTime: &metav1.Time{Time: now}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	sequence := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(job, &native.Task{}, &ext.ContainerAllocation{}).WithObjects(cluster, job, binding, cc, node, cn).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		sequence++
		obj.SetUID(types.UID(obj.GetName() + "-" + time.Unix(int64(sequence), 0).Format("150405")))
		obj.SetGeneration(1)
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return JobReservationReconciler{Client: c, Now: func() time.Time { return now }}, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(job)}
}

func reconcileJob(t *testing.T, r JobReservationReconciler, req ctrl.Request, count int) {
	t.Helper()
	for range count {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
}

func allocated(t *testing.T, r JobReservationReconciler) (*native.Task, *ext.ContainerAllocation) {
	t.Helper()
	var tasks native.TaskList
	var locks ext.ContainerAllocationList
	if err := r.Client.List(context.Background(), &tasks); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.List(context.Background(), &locks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || len(locks.Items) != 1 {
		t.Fatalf("want one Task/allocation, got %d/%d", len(tasks.Items), len(locks.Items))
	}
	return &tasks.Items[0], &locks.Items[0]
}

func proveCleanup(t *testing.T, r JobReservationReconciler, a *ext.ContainerAllocation) {
	t.Helper()
	a.Status.CleanupConfirmed = true
	a.Status.ObservedGeneration = a.Generation
	a.Status.Conditions = []metav1.Condition{{Type: "CleanupConfirmed", Status: metav1.ConditionTrue, ObservedGeneration: a.Generation}}
	if err := r.Client.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func TestJobReservationPersistsOneOwnedTaskAndRecovers(t *testing.T) {
	r, req := jobFixture(t)
	reconcileJob(t, r, req, 5)
	task, lock := allocated(t, r)
	if !taskOwnedBy(task, &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job"}}) || lock.Spec.Task.UID != string(task.UID) || lock.Spec.Node.UID != "native-node" {
		t.Fatal("Task or allocation ownership was not persisted")
	}
	reconcileJob(t, JobReservationReconciler{Client: r.Client, Now: r.Now}, req, 3)
	_, recovered := allocated(t, r)
	if recovered.UID != lock.UID {
		t.Fatal("restart allocated a new execution")
	}
	var job native.Job
	if err := r.Client.Get(context.Background(), req.NamespacedName, &job); err != nil {
		t.Fatal(err)
	}
	if job.Status.Phase != native.JobPhasePending || job.Status.Conditions[0].Reason != "ReservedAwaitingDispatch" {
		t.Fatalf("reservation advertised execution: %+v", job.Status)
	}
}

func TestJobStopRetainsUnknownCleanupThenDeletesProvenAllocation(t *testing.T) {
	r, req := jobFixture(t)
	reconcileJob(t, r, req, 3)
	task, lock := allocated(t, r)
	var job native.Job
	ctx := context.Background()
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
		t.Fatal(err)
	}
	job.Spec.Suspended = true
	job.Generation++
	if err := r.Client.Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 3)
	retained, current := allocated(t, r)
	if !retained.Spec.Suspended || current.UID != lock.UID || retained.UID != task.UID {
		t.Fatal("stop released unknown execution")
	}
	proveCleanup(t, r, current)
	reconcileJob(t, r, req, 4)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(task), &native.Task{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Task remained after cleanup: %v", err)
	}
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil || job.Status.Phase != native.JobPhaseStopped {
		t.Fatalf("stop did not finish: %+v %v", job.Status, err)
	}
}

func TestSpecChangeReplacesTaskOnlyAfterCleanup(t *testing.T) {
	r, req := jobFixture(t)
	reconcileJob(t, r, req, 3)
	oldTask, oldLock := allocated(t, r)
	var job native.Job
	ctx := context.Background()
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
		t.Fatal(err)
	}
	job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3","other.py"],"env":[],"directory":"/work"}`)
	job.Generation++
	if err := r.Client.Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 3)
	retained, lock := allocated(t, r)
	if retained.UID != oldTask.UID || lock.UID != oldLock.UID {
		t.Fatal("configuration replaced a live execution")
	}
	proveCleanup(t, r, lock)
	reconcileJob(t, r, req, 5)
	newTask, newLock := allocated(t, r)
	if newTask.UID == oldTask.UID || newLock.UID == oldLock.UID || newLock.Spec.Task.UID != string(newTask.UID) {
		t.Fatal("configuration change did not create a new fenced execution")
	}
}

func TestUnsupportedOrCrossTenantJobNeverCreatesTasks(t *testing.T) {
	for _, mutation := range []string{"ray", "multiple", "tenant", "gpu-env", "legacy"} {
		t.Run(mutation, func(t *testing.T) {
			r, req := jobFixture(t)
			var job native.Job
			ctx := context.Background()
			if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "ray":
				job.Spec.Tasks[0].Workload.Kind = "ray.rlark.io/worker"
			case "multiple":
				job.Spec.Tasks = append(job.Spec.Tasks, job.Spec.Tasks[0])
			case "tenant":
				tenant.SetIdentity(&job, "other", "")
			case "gpu-env":
				job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3"],"env":["CUDA_VISIBLE_DEVICES=all"],"directory":"/work"}`)
			case "legacy":
				job.Spec.Tasks[0].Placement.Binding = &native.RawTaskBinding{NodeName: "worker"}
			}
			if err := r.Client.Update(ctx, &job); err != nil {
				t.Fatal(err)
			}
			reconcileJob(t, r, req, 2)
			var tasks native.TaskList
			if err := r.Client.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
				t.Fatalf("invalid Job materialized Tasks: %d %v", len(tasks.Items), err)
			}
		})
	}
}

func TestDeletionCleanupReceiptRecoversLostAllocationDeleteResponse(t *testing.T) {
	r, req := jobFixture(t)
	reconcileJob(t, r, req, 3)
	task, lock := allocated(t, r)
	ctx := context.Background()
	var job native.Job
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Delete(ctx, &job); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 1)
	proveCleanup(t, r, lock)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*ext.ContainerAllocation); ok {
			return errors.New("allocation finalizer patch response lost")
		}
		return nil
	}})
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected lost allocation deletion response")
	}
	if err := base.Get(ctx, client.ObjectKeyFromObject(lock), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("allocation was not deleted: %v", err)
	}
	r.Client = base
	reconcileJob(t, r, req, 4)
	if err := base.Get(ctx, client.ObjectKeyFromObject(task), &native.Task{}); !apierrors.IsNotFound(err) {
		t.Fatalf("receipt failed to recover Task cleanup: %v", err)
	}
	if err := base.Get(ctx, req.NamespacedName, &native.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job finalizer failed to finish: %v", err)
	}
}

func TestUnownedTaskNameCollisionIsNeverAdopted(t *testing.T) {
	r, req := jobFixture(t)
	ctx := context.Background()
	collision := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: "job-worker", Namespace: "agents", UID: "foreign"}}
	if err := r.Client.Create(ctx, collision); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 3)
	var current native.Task
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(collision), &current); err != nil || current.UID != collision.UID || metav1.GetControllerOf(&current) != nil || len(current.Finalizers) != 0 {
		t.Fatalf("foreign Task was adopted or mutated: %+v %v", current.ObjectMeta, err)
	}
	var locks ext.ContainerAllocationList
	if err := r.Client.List(ctx, &locks); err != nil || len(locks.Items) != 0 {
		t.Fatalf("foreign Task triggered reservation: %d %v", len(locks.Items), err)
	}
}

func TestOrphanAllocationRetainsDeletingJobIdentity(t *testing.T) {
	r, req := jobFixture(t)
	reconcileJob(t, r, req, 1)
	ctx := context.Background()
	orphan := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "agents"}, Spec: ext.ContainerAllocationSpec{Job: ext.NativeObjectReference{Name: "job", UID: "job"}, Task: ext.NativeObjectReference{Name: "missing", UID: "missing"}}}
	if err := r.Client.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	var job native.Job
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Delete(ctx, &job); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 3)
	if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil || len(job.Finalizers) == 0 || job.Status.Conditions[0].Reason != "CleanupPending" {
		t.Fatalf("orphan allocation lost deleting Job identity: %+v %v", job, err)
	}
}
