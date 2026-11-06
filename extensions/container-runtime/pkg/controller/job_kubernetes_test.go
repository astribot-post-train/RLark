package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func mixedFixture(t *testing.T) (JobReservationReconciler, ctrl.Request) {
	t.Helper()
	r, req := jobFixture(t)
	ctx := context.Background()
	robot := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "robot"}, Spec: native.ClusterSpec{AgentNamespace: "robot-agents"}}
	if err := r.Client.Create(ctx, robot); err != nil {
		t.Fatal(err)
	}
	var b ext.ContainerJob
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b); err != nil {
		t.Fatal(err)
	}
	b.Spec.KubernetesTargets = []ext.KubernetesTaskTarget{{Template: "env", Cluster: ext.NativeObjectReference{Name: robot.Name, UID: string(robot.UID)}}}
	if err := r.Client.Update(ctx, &b); err != nil {
		t.Fatal(err)
	}
	j := readExecutionJob(t, r, req)
	j.Spec.Tasks = append(j.Spec.Tasks, native.JobTaskTemplate{Name: "env", TaskSpec: native.TaskSpec{
		Runtime:  native.TaskRuntimeSpec{Kind: "Kubernetes"},
		Workload: native.TaskWorkloadSpec{Kind: "rlinf.io/ray", Version: "v1", Parameters: apiextensions.JSON{Raw: []byte(`{"role":"worker","headTaskName":"fleet-head","headAddress":"10.1.3.42","totalNodes":2}`)}, Carrier: native.TaskWorkloadCarrier{Type: "rlinf.io/kubernetes-deployment", Version: "v1"}, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "env", Image: "test.invalid/fleet:v1"}}}}},
	}})
	if err := r.Client.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	return r, req
}

func mixedObjects(t *testing.T, r JobReservationReconciler) (*native.Task, *native.Task, *ext.ContainerAllocation) {
	t.Helper()
	tasks, locks := processObjects(t, r)
	var raw, k8s *native.Task
	for i := range tasks {
		if tasks[i].Spec.Runtime.Kind == "Kubernetes" {
			k8s = &tasks[i]
		} else {
			raw = &tasks[i]
		}
	}
	if len(tasks) != 2 || len(locks) != 1 || raw == nil || k8s == nil {
		t.Fatalf("want Raw + Kubernetes Tasks and one allocation, got %d/%d", len(tasks), len(locks))
	}
	return raw, k8s, &locks[0]
}

func observeNative(t *testing.T, r JobReservationReconciler, task *native.Task, phase native.TaskPhase) {
	t.Helper()
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	task.Status.ObservedGeneration, task.Status.Phase = task.Generation, phase
	if err := r.Client.Status().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

func TestMixedJobPlacementAndRecoveredCompletion(t *testing.T) {
	for _, phase := range []native.TaskPhase{native.TaskPhaseSucceeded, native.TaskPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			r, req := mixedFixture(t)
			reconcileJob(t, r, req, 7)
			raw, k8s, lock := mixedObjects(t, r)
			j := readExecutionJob(t, r, req)
			if raw.Namespace != "agents" || k8s.Namespace != "robot-agents" || lock.Spec.Task.UID != string(raw.UID) || !taskOwnedBy(k8s, j) || k8s.Annotations[targetClusterUID] == "" || !controllerutil.ContainsFinalizer(k8s, kubernetesTaskProtection) {
				t.Fatal("mixed placement or ownership lost")
			}
			observeExecution(t, r, raw, native.TaskPhaseSucceeded)
			proveCleanup(t, r, lock)
			observeNative(t, r, k8s, phase)
			reconcileJob(t, r, req, 1)
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(k8s), k8s); err != nil {
				t.Fatal(err)
			}
			if !k8s.Spec.Suspended || k8s.Annotations[kubernetesResultAnnotation] == "" {
				t.Fatal("did not save result before cleanup")
			}
			// The API server increments Spec generation; the fake client does not.
			k8s.Generation++
			if err := r.Client.Update(context.Background(), k8s); err != nil {
				t.Fatal(err)
			}
			r = JobReservationReconciler{Client: r.Client, Now: r.Now}
			reconcileJob(t, r, req, 2)
			j = readExecutionJob(t, r, req)
			if j.Status.Phase == native.JobPhaseSucceeded || j.Status.Phase == native.JobPhaseFailed {
				t.Fatal("published terminal before native cleanup")
			}
			observeNative(t, r, k8s, native.TaskPhaseStopped)
			reconcileJob(t, r, req, 3)
			j = readExecutionJob(t, r, req)
			if j.Status.Phase != native.JobPhase(phase) || len(j.Status.Tasks) != 2 {
				t.Fatalf("lost native result after stop: %+v", j.Status)
			}
			retained, locks := processObjects(t, r)
			if len(retained) != 2 || len(locks) != 0 {
				t.Fatal("completed identity not retained or allocation not released")
			}
			if err := r.Client.Delete(context.Background(), k8s); err != nil {
				t.Fatal(err)
			}
			reconcileJob(t, r, req, 3)
			var same native.Task
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(k8s), &same); err != nil || same.UID != k8s.UID {
				t.Fatal("terminal deletion replayed native Task")
			}
		})
	}
}

func TestMixedJobRejectsUnprovenTargetsBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "stale-uid", "wrong-tenant", "ambiguous-namespace", "container-namespace", "unknown-template", "head", "bad-parameters", "docker", "suspended-worker"} {
		t.Run(mode, func(t *testing.T) {
			r, req := mixedFixture(t)
			ctx := context.Background()
			var b ext.ContainerJob
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b); err != nil {
				t.Fatal(err)
			}
			j := readExecutionJob(t, r, req)
			var robot native.Cluster
			if err := r.Client.Get(ctx, client.ObjectKey{Name: "robot"}, &robot); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				b.Spec.KubernetesTargets = nil
			case "duplicate":
				b.Spec.KubernetesTargets = append(b.Spec.KubernetesTargets, b.Spec.KubernetesTargets[0])
			case "stale-uid":
				b.Spec.KubernetesTargets[0].Cluster.UID = "replaced"
			case "wrong-tenant":
				tenant.SetIdentity(&robot, "other", "")
			case "ambiguous-namespace":
				other := robot.DeepCopy()
				other.Name, other.UID, other.ResourceVersion = "other", "", ""
				if err := r.Client.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
			case "container-namespace":
				robot.Spec.AgentNamespace = "agents"
			case "unknown-template":
				b.Spec.KubernetesTargets[0].Template = "unknown"
			case "head":
				j.Spec.Tasks[1].Workload.Parameters.Raw = []byte(`{"role":"head"}`)
			case "bad-parameters":
				j.Spec.Tasks[1].Workload.Parameters.Raw = []byte(`{"role":"worker","typo":true}`)
			case "docker":
				j.Spec.Tasks[0].Runtime.Kind = "Docker"
			case "suspended-worker":
				j.Spec.Tasks[1].Suspended = true
			}
			for _, obj := range []client.Object{&b, j, &robot} {
				if err := r.Client.Update(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}
			reconcileJob(t, r, req, 3)
			tasks, locks := processObjects(t, r)
			if len(tasks) != 0 || len(locks) != 0 {
				t.Fatal("invalid target dispatched work")
			}
		})
	}
}

func TestMixedStopWaitsForNativeHostAndRawCleanupAfterRestart(t *testing.T) {
	r, req := mixedFixture(t)
	reconcileJob(t, r, req, 7)
	raw, k8s, lock := mixedObjects(t, r)
	ctx := context.Background()
	controllerutil.AddFinalizer(k8s, host.TaskFinalizer)
	if err := r.Client.Update(ctx, k8s); err != nil {
		t.Fatal(err)
	}
	j := readExecutionJob(t, r, req)
	j.Spec.Suspended = true
	j.Generation++
	if err := r.Client.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 2)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(raw), raw); err != nil || !raw.Spec.Suspended {
		t.Fatal("native cleanup blocked Raw stop fan-out")
	}
	proveCleanup(t, r, lock)
	r = JobReservationReconciler{Client: r.Client, Now: r.Now}
	reconcileJob(t, r, req, 4)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(k8s), k8s); err != nil || k8s.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(k8s, host.TaskFinalizer) || !controllerutil.ContainsFinalizer(k8s, kubernetesTaskProtection) {
		t.Fatal("removed native cleanup protection")
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(raw), &native.Task{}); !apierrors.IsNotFound(err) {
		t.Fatal("Raw cleanup starved")
	}
	controllerutil.RemoveFinalizer(k8s, host.TaskFinalizer)
	if err := r.Client.Update(ctx, k8s); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 5)
	tasks, locks := processObjects(t, r)
	if len(tasks) != 0 || len(locks) != 0 {
		t.Fatal("stop did not finish")
	}
}

func TestKubernetesResultReceiptRejectsReplacementAndStaleGeneration(t *testing.T) {
	for _, mode := range []string{"uid", "generation", "phase", "unsuspended"} {
		t.Run(mode, func(t *testing.T) {
			task := &native.Task{ObjectMeta: metav1.ObjectMeta{UID: "task", Generation: 4, Annotations: map[string]string{}}, Spec: native.TaskSpec{Suspended: true}}
			result := kubernetesResult{TaskUID: "task", Generation: 3, Phase: native.TaskPhaseSucceeded}
			switch mode {
			case "uid":
				result.TaskUID = "other"
			case "generation":
				result.Generation = 2
			case "phase":
				result.Phase = native.TaskPhaseRunning
			case "unsuspended":
				task.Spec.Suspended = false
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			task.Annotations[kubernetesResultAnnotation] = string(data)
			if _, err := readKubernetesResult(task); err == nil {
				t.Fatal("accepted invalid receipt")
			}
		})
	}
}

func TestMixedCompletionRecoversLostNativeReceiptResponse(t *testing.T) {
	r, req := mixedFixture(t)
	reconcileJob(t, r, req, 7)
	raw, k8s, lock := mixedObjects(t, r)
	observeExecution(t, r, raw, native.TaskPhaseSucceeded)
	proveCleanup(t, r, lock)
	observeNative(t, r, k8s, native.TaskPhaseFailed)
	base := r.Client
	lost := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		if task, ok := obj.(*native.Task); ok && task.UID == k8s.UID && task.Annotations[kubernetesResultAnnotation] != "" && !lost {
			lost = true
			return errors.New("native result committed but response lost")
		}
		return nil
	}})
	if _, err := r.Reconcile(context.Background(), req); err == nil || !lost {
		t.Fatal("did not inject lost native result response")
	}
	r = JobReservationReconciler{Client: base, Now: r.Now}
	reconcileJob(t, r, req, 2)
	observeNative(t, r, k8s, native.TaskPhaseStopped)
	reconcileJob(t, r, req, 3)
	j := readExecutionJob(t, r, req)
	if j.Status.Phase != native.JobPhaseFailed {
		t.Fatalf("failed result lost across restart: %+v", j.Status)
	}
	var current native.Task
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(k8s), &current); err != nil || current.UID != k8s.UID {
		t.Fatal("completion response loss replaced native execution")
	}
}

func TestMixedCorruptReceiptBlocksReplayUntilExplicitStop(t *testing.T) {
	r, req := mixedFixture(t)
	reconcileJob(t, r, req, 7)
	raw, k8s, lock := mixedObjects(t, r)
	k8s.Annotations[kubernetesResultAnnotation] = `{"taskUID":"foreign","generation":1,"phase":"Succeeded"}`
	if err := r.Client.Update(context.Background(), k8s); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err == nil {
			t.Fatal("corrupt result was treated as ordinary spec drift")
		}
	}
	var current native.Task
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(k8s), &current); err != nil || current.UID != k8s.UID || !current.DeletionTimestamp.IsZero() {
		t.Fatal("corrupt result retired the original execution")
	}
	j := readExecutionJob(t, r, req)
	j.Spec.Suspended = true
	j.Generation++
	if err := r.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 2)
	proveCleanup(t, r, lock)
	reconcileJob(t, r, req, 5)
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(raw), &current); !apierrors.IsNotFound(err) {
		t.Fatal("explicit stop did not retain a cleanup recovery path")
	}
}

func TestMixedStopPreservesForeignFinalizer(t *testing.T) {
	r, req := mixedFixture(t)
	reconcileJob(t, r, req, 7)
	_, k8s, lock := mixedObjects(t, r)
	controllerutil.AddFinalizer(k8s, "example.org/foreign-cleanup")
	if err := r.Client.Update(context.Background(), k8s); err != nil {
		t.Fatal(err)
	}
	j := readExecutionJob(t, r, req)
	j.Spec.Suspended = true
	j.Generation++
	if err := r.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 2)
	proveCleanup(t, r, lock)
	reconcileJob(t, r, req, 5)
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(k8s), k8s); err != nil || !controllerutil.ContainsFinalizer(k8s, "example.org/foreign-cleanup") || k8s.DeletionTimestamp.IsZero() {
		t.Fatal("unknown cleanup owner was modified")
	}
	if j := readExecutionJob(t, r, req); j.Status.Phase == native.JobPhaseStopped {
		t.Fatal("reported stopped before foreign cleanup completed")
	}
}
