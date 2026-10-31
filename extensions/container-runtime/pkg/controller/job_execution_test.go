package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func observeExecution(t *testing.T, r JobReservationReconciler, task *native.Task, phase native.TaskPhase) {
	t.Helper()
	task.Status = native.TaskStatus{Phase: phase, ObservedGeneration: task.Generation, Message: "observed result", Conditions: []metav1.Condition{
		{Type: "ExecutionKnown", Status: metav1.ConditionTrue, ObservedGeneration: task.Generation},
		{Type: "ExecutionCleaned", Status: metav1.ConditionTrue, ObservedGeneration: task.Generation},
	}}
	if err := r.Client.Status().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

func readExecutionJob(t *testing.T, r JobReservationReconciler, req ctrl.Request) *native.Job {
	t.Helper()
	var j native.Job
	if err := r.Client.Get(context.Background(), req.NamespacedName, &j); err != nil {
		t.Fatal(err)
	}
	return &j
}

func TestJobExecutionResultReleasesCapacityAndKeepsIdentity(t *testing.T) {
	for _, phase := range []native.TaskPhase{native.TaskPhaseSucceeded, native.TaskPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			r, req := jobFixture(t)
			reconcileJob(t, r, req, 3)
			task, lock := allocated(t, r)
			observeExecution(t, r, task, native.TaskPhaseRunning)
			reconcileJob(t, r, req, 1)
			running := readExecutionJob(t, r, req)
			if running.Status.Phase != native.JobPhaseRunning || running.Status.StartTime == nil {
				t.Fatalf("running not aggregated: %+v", running.Status)
			}
			observeExecution(t, r, task, phase)
			reconcileJob(t, r, req, 2)
			pending := readExecutionJob(t, r, req)
			if pending.Status.Phase != native.JobPhasePending || pending.Status.EndTime != nil || pending.Status.Tasks[0].Phase != phase || meta.FindStatusCondition(pending.Status.Conditions, "Scheduled").Reason != "TerminalCleanupPending" {
				t.Fatalf("unproven cleanup published completion: %+v", pending.Status)
			}
			proveCleanup(t, r, lock)
			reconcileJob(t, r, req, 3)
			finished := readExecutionJob(t, r, req)
			if finished.Status.Phase != native.JobPhase(phase) || finished.Status.EndTime == nil || !reflect.DeepEqual(finished.Status.StartTime, running.Status.StartTime) || !meta.IsStatusConditionTrue(finished.Status.Conditions, jobAllocationReleasedCondition) {
				t.Fatalf("completion not aggregated: %+v", finished.Status)
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(lock), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
				t.Fatalf("allocation not deleted: %v", err)
			}
			// A new controller and a direct Task deletion must not create a second
			// execution; the terminal Task's finalizer protects its result identity.
			if err := r.Client.Delete(ctx, task); err != nil {
				t.Fatal(err)
			}
			reconcileJob(t, JobReservationReconciler{Client: r.Client, Now: r.Now}, req, 4)
			var retained native.Task
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(task), &retained); err != nil || retained.UID != task.UID || retained.Annotations[cleanupReceiptAnnotation] == "" {
				t.Fatalf("terminal identity lost: %+v %v", retained, err)
			}
			if got := readExecutionJob(t, r, req); !reflect.DeepEqual(got.Status, finished.Status) {
				t.Fatalf("repeated completion changed status: %+v", got.Status)
			}
			var allocations ext.ContainerAllocationList
			if err := r.Client.List(ctx, &allocations); err != nil || len(allocations.Items) != 0 {
				t.Fatalf("completed Task reacquired: %+v %v", allocations, err)
			}
			// A different Job can now take the same node while this result remains.
			second := readExecutionJob(t, r, req).DeepCopy()
			second.Name, second.UID, second.ResourceVersion, second.Finalizers = "next", "next-job", "", nil
			second.Status = native.JobStatus{}
			if err := r.Client.Create(ctx, second); err != nil {
				t.Fatal(err)
			}
			binding := &ext.ContainerJob{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, binding); err != nil {
				t.Fatal(err)
			}
			binding.Name, binding.UID, binding.ResourceVersion = "next", "next-binding", ""
			binding.Spec.Job.Name, binding.Spec.Job.UID = second.Name, string(second.UID)
			if err := r.Client.Create(ctx, binding); err != nil {
				t.Fatal(err)
			}
			reconcileJob(t, r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(second)}, 3)
			if err := r.Client.List(ctx, &allocations); err != nil || len(allocations.Items) != 1 || allocations.Items[0].Spec.Job.Name != "next" || allocations.Items[0].Spec.Node != lock.Spec.Node {
				t.Fatalf("released node unavailable to next Job: %+v %v", allocations, err)
			}
			reconcileJob(t, r, req, 2)
		})
	}
}

func TestJobExecutionUnprovenResultNeverReleases(t *testing.T) {
	for _, mode := range []string{"stale status", "stale known", "missing known", "missing cleaned", "stale cleaned", "host claim", "missing allocation", "foreign receipt"} {
		t.Run(mode, func(t *testing.T) {
			r, req := jobFixture(t)
			reconcileJob(t, r, req, 3)
			task, lock := allocated(t, r)
			observeExecution(t, r, task, native.TaskPhaseSucceeded)
			proveCleanup(t, r, lock)
			switch mode {
			case "stale status":
				task.Status.ObservedGeneration--
			case "stale known":
				task.Status.Conditions[0].ObservedGeneration--
			case "missing known":
				task.Status.Conditions = task.Status.Conditions[1:]
			case "missing cleaned":
				task.Status.Conditions = task.Status.Conditions[:1]
			case "stale cleaned":
				task.Status.Conditions[1].ObservedGeneration--
			case "host claim":
				task.Finalizers = append(task.Finalizers, host.TaskFinalizer)
				if err := r.Client.Update(context.Background(), task); err != nil {
					t.Fatal(err)
				}
			case "missing allocation":
				if err := (companion.Manager{Client: r.Client}).Release(context.Background(), client.ObjectKeyFromObject(lock), string(lock.UID)); err != nil {
					t.Fatal(err)
				}
			case "foreign receipt":
				task.Annotations[cleanupReceiptAnnotation] = `{"allocation":{"uid":"foreign"}}`
				if err := r.Client.Update(context.Background(), task); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Client.Status().Update(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			_, err := r.Reconcile(context.Background(), req)
			if mode == "foreign receipt" && err == nil {
				t.Fatal("foreign receipt accepted")
			}
			j := readExecutionJob(t, r, req)
			if j.Status.Phase == native.JobPhaseSucceeded || j.Status.EndTime != nil || meta.IsStatusConditionTrue(j.Status.Conditions, jobAllocationReleasedCondition) {
				t.Fatalf("unproven result released: %+v", j.Status)
			}
			if mode != "missing allocation" {
				_, current := allocated(t, r)
				if current.UID != lock.UID {
					t.Fatal("allocation replaced")
				}
			}
		})
	}
}

func TestJobCompletionRecoversLostWritesAndDelayedDeletion(t *testing.T) {
	for _, mode := range []string{"receipt response", "delete response", "status response", "delayed deletion"} {
		t.Run(mode, func(t *testing.T) {
			r, req := jobFixture(t)
			reconcileJob(t, r, req, 3)
			task, lock := allocated(t, r)
			observeExecution(t, r, task, native.TaskPhaseSucceeded)
			proveCleanup(t, r, lock)
			if mode == "delayed deletion" {
				lock.Finalizers = append(lock.Finalizers, "example.com/retention")
				if err := r.Client.Update(context.Background(), lock); err != nil {
					t.Fatal(err)
				}
			}
			base := r.Client
			lost := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, o, p, opts...); err != nil {
						return err
					}
					_, taskPatch := o.(*native.Task)
					_, allocationPatch := o.(*ext.ContainerAllocation)
					if !lost && (mode == "receipt response" && taskPatch || mode == "delete response" && allocationPatch) {
						lost = true
						return errors.New("committed response lost")
					}
					return nil
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
					if err := c.SubResource(sub).Patch(ctx, o, p, opts...); err != nil {
						return err
					}
					if !lost && mode == "status response" {
						lost = true
						return errors.New("committed status response lost")
					}
					return nil
				},
			})
			_, err := r.Reconcile(context.Background(), req)
			if mode != "delayed deletion" && (err == nil || !lost) {
				t.Fatalf("did not exercise response loss: %v", err)
			}
			r.Client = base
			if mode == "delayed deletion" {
				if readExecutionJob(t, r, req).Status.Phase != native.JobPhasePending {
					t.Fatal("delete acceptance treated as completion")
				}
				if err := base.Get(context.Background(), client.ObjectKeyFromObject(lock), lock); err != nil {
					t.Fatal(err)
				}
				lock.Finalizers = nil
				if err := base.Update(context.Background(), lock); err != nil {
					t.Fatal(err)
				}
			}
			reconcileJob(t, JobReservationReconciler{Client: base, Now: r.Now}, req, 3)
			if readExecutionJob(t, r, req).Status.Phase != native.JobPhaseSucceeded {
				t.Fatal("completion failed to recover")
			}
		})
	}
}

func TestCompletedJobCanStopDeleteOrRestart(t *testing.T) {
	for _, mode := range []string{"stop", "delete", "restart", "template"} {
		t.Run(mode, func(t *testing.T) {
			r, req := jobFixture(t)
			reconcileJob(t, r, req, 3)
			task, lock := allocated(t, r)
			observeExecution(t, r, task, native.TaskPhaseFailed)
			proveCleanup(t, r, lock)
			reconcileJob(t, r, req, 2)
			j := readExecutionJob(t, r, req)
			switch mode {
			case "stop":
				j.Spec.Suspended = true
				j.Generation++
			case "delete":
				if err := r.Client.Delete(context.Background(), j); err != nil {
					t.Fatal(err)
				}
			case "restart":
				j.Annotations[nativejob.RestartedAtAnnotation] = "new-run"
			case "template":
				j.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/true"],"env":[],"directory":"/work"}`)
				j.Generation++
			}
			if mode != "delete" {
				if err := r.Client.Update(context.Background(), j); err != nil {
					t.Fatal(err)
				}
			}
			reconcileJob(t, r, req, 6)
			switch mode {
			case "delete":
				if err := r.Client.Get(context.Background(), req.NamespacedName, &native.Job{}); !apierrors.IsNotFound(err) {
					t.Fatalf("Job deletion blocked: %v", err)
				}
			case "stop":
				if readExecutionJob(t, r, req).Status.Phase != native.JobPhaseStopped {
					t.Fatal("Job did not stop")
				}
			default:
				current, _ := allocated(t, r)
				if current.UID == task.UID {
					t.Fatal("new execution reused completed Task")
				}
				j := readExecutionJob(t, r, req)
				if j.Status.Phase != native.JobPhasePending || j.Status.EndTime != nil || j.Status.StartTime != nil {
					t.Fatalf("new execution retained timestamps: %+v", j.Status)
				}
			}
		})
	}
}
