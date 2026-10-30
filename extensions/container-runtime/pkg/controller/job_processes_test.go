package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func processesFixture(t *testing.T, nodes int) (JobReservationReconciler, ctrl.Request) {
	t.Helper()
	r, req := jobFixture(t)
	ctx := context.Background()
	job := readExecutionJob(t, r, req)
	second := *job.Spec.Tasks[0].DeepCopy()
	second.Name = "worker-b"
	job.Spec.Tasks = append(job.Spec.Tasks, second)
	if err := r.Client.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < nodes; i++ {
		var nn native.Node
		var cn ext.ContainerNode
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn); err != nil {
			t.Fatal(err)
		}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &cn); err != nil {
			t.Fatal(err)
		}
		nn.Name, nn.UID, nn.ResourceVersion = fmt.Sprintf("worker-%d", i), "", ""
		if err := r.Client.Create(ctx, &nn); err != nil {
			t.Fatal(err)
		}
		cn.Name, cn.UID, cn.ResourceVersion = nn.Name, "", ""
		cn.Spec.Node = ext.NativeObjectReference{Namespace: nn.Namespace, Name: nn.Name, UID: string(nn.UID)}
		cn.Spec.InstanceID = fmt.Sprintf("container-%d", i)
		cn.Spec.PrivateAddress = fmt.Sprintf("10.0.3.%d", 90+i)
		cn.Spec.AuthorizedGPUUUIDs = []string{fmt.Sprintf("GPU-%d", i)}
		if err := r.Client.Create(ctx, &cn); err != nil {
			t.Fatal(err)
		}
	}
	return r, req
}

func processObjects(t *testing.T, r JobReservationReconciler) ([]native.Task, []ext.ContainerAllocation) {
	t.Helper()
	var tasks native.TaskList
	var locks ext.ContainerAllocationList
	if err := r.Client.List(context.Background(), &tasks); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.List(context.Background(), &locks); err != nil {
		t.Fatal(err)
	}
	return tasks.Items, locks.Items
}

func TestProcessesRecoverSeparateTargetsAndAggregateAfterAllCleanup(t *testing.T) {
	for _, result := range []native.TaskPhase{native.TaskPhaseSucceeded, native.TaskPhaseFailed} {
		t.Run(string(result), func(t *testing.T) {
			r, req := processesFixture(t, 2)
			reconcileJob(t, r, req, 7)
			tasks, locks := processObjects(t, r)
			if len(tasks) != 2 || len(locks) != 2 || locks[0].Spec.Node.UID == locks[1].Spec.Node.UID {
				t.Fatalf("not separately allocated: %+v", locks)
			}
			recovered := JobReservationReconciler{Client: r.Client, Now: r.Now}
			reconcileJob(t, recovered, req, 3)
			_, same := processObjects(t, r)
			if same[0].UID != locks[0].UID || same[1].UID != locks[1].UID {
				t.Fatal("recovery changed targets")
			}
			observeExecution(t, r, &tasks[0], result)
			proveCleanup(t, r, &locks[0])
			observeExecution(t, r, &tasks[1], native.TaskPhaseRunning)
			reconcileJob(t, r, req, 2)
			j := readExecutionJob(t, r, req)
			if j.Status.Phase != native.JobPhaseRunning || j.Status.EndTime != nil || len(j.Status.Tasks) != 2 {
				t.Fatalf("premature terminal status: %+v", j.Status)
			}
			observeExecution(t, r, &tasks[1], native.TaskPhaseSucceeded)
			reconcileJob(t, r, req, 1)
			if j = readExecutionJob(t, r, req); j.Status.Phase == native.JobPhaseSucceeded || j.Status.Phase == native.JobPhaseFailed {
				t.Fatal("published terminal before cleanup")
			}
			proveCleanup(t, r, &locks[1])
			reconcileJob(t, r, req, 3)
			j = readExecutionJob(t, r, req)
			if j.Status.Phase != native.JobPhase(result) || !meta.IsStatusConditionTrue(j.Status.Conditions, jobAllocationReleasedCondition) {
				t.Fatalf("bad final aggregate: %+v", j.Status)
			}
			// Even direct deletion must retain terminal identities and prevent replay.
			for i := range tasks {
				if err := r.Client.Delete(context.Background(), &tasks[i]); err != nil {
					t.Fatal(err)
				}
			}
			reconcileJob(t, recovered, req, 3)
			retained, remaining := processObjects(t, r)
			if len(retained) != 2 || len(remaining) != 0 || retained[0].UID != tasks[0].UID || retained[1].UID != tasks[1].UID {
				t.Fatal("terminal member replayed")
			}
		})
	}
}

func TestProcessesCapacityWaitDoesNotStarveTerminalCleanup(t *testing.T) {
	r, req := processesFixture(t, 1)
	reconcileJob(t, r, req, 6)
	tasks, locks := processObjects(t, r)
	if len(tasks) != 2 || len(locks) != 1 {
		t.Fatalf("bad partial admission: %d/%d", len(tasks), len(locks))
	}
	var running *native.Task
	for i := range tasks {
		if string(tasks[i].UID) == locks[0].Spec.Task.UID {
			running = &tasks[i]
		}
	}
	if running == nil {
		t.Fatal("missing locked member")
	}
	observeExecution(t, r, running, native.TaskPhaseSucceeded)
	proveCleanup(t, r, &locks[0])
	reconcileJob(t, r, req, 4)
	_, current := processObjects(t, r)
	if len(current) != 1 || current[0].Spec.Task.UID == locks[0].Spec.Task.UID {
		t.Fatalf("capacity not handed to waiting member: %+v", current)
	}
	j := readExecutionJob(t, r, req)
	if len(j.Status.Tasks) != 2 || j.Status.Phase != native.JobPhasePending {
		t.Fatalf("partial status lost: %+v", j.Status)
	}
}

func TestProcessesReplacementWaitsForEntireRetiredSet(t *testing.T) {
	r, req := processesFixture(t, 2)
	reconcileJob(t, r, req, 7)
	tasks, locks := processObjects(t, r)
	j := readExecutionJob(t, r, req)
	j.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/true"],"directory":"/work"}`)
	j.Generation++
	if err := r.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 1)
	stopped, _ := processObjects(t, r)
	for _, task := range stopped {
		if !task.Spec.Suspended {
			t.Fatal("one blocked member prevented stop fan-out")
		}
	}
	proveCleanup(t, r, &locks[0])
	reconcileJob(t, r, req, 2)
	r = JobReservationReconciler{Client: r.Client, Now: r.Now}
	reconcileJob(t, r, req, 3)
	partial, _ := processObjects(t, r)
	if len(partial) != 1 || partial[0].UID != tasks[1].UID {
		t.Fatal("partially retired member recreated before sibling cleanup")
	}
	proveCleanup(t, r, &locks[1])
	reconcileJob(t, r, req, 9)
	fresh, current := processObjects(t, r)
	if len(fresh) != 2 || len(current) != 2 {
		t.Fatalf("replacement did not finish: %d/%d", len(fresh), len(current))
	}
	for _, task := range fresh {
		for _, old := range tasks {
			if task.UID == old.UID {
				t.Fatal("old set retained")
			}
		}
	}
	for _, lock := range current {
		if lock.Spec.JobGeneration != j.Generation {
			t.Fatal("old Job generation allocated")
		}
	}
}

func TestProcessesRecoverLostChildCreateResponse(t *testing.T) {
	r, req := processesFixture(t, 2)
	base := r.Client
	lost := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*native.Task); ok && !lost {
			lost = true
			return errors.New("child create response lost")
		}
		return nil
	}})
	reconcileJob(t, r, req, 1)
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("lost response not injected")
	}
	r.Client = base
	reconcileJob(t, r, req, 7)
	tasks, locks := processObjects(t, r)
	if len(tasks) != 2 || len(locks) != 2 {
		t.Fatalf("retry duplicated or lost member: %d/%d", len(tasks), len(locks))
	}
	for _, task := range tasks {
		if metav1.GetControllerOf(&task) == nil {
			t.Fatal("ownership lost")
		}
	}
}

func TestProcessesBlockedFirstMemberStillCleansLaterMember(t *testing.T) {
	r, req := processesFixture(t, 1)
	j := readExecutionJob(t, r, req)
	j.Spec.Tasks[0].Placement.NodeSelector = map[string]string{"rack": "unavailable"}
	if err := r.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileJob(t, r, req, 7)
	tasks, locks := processObjects(t, r)
	if len(locks) != 1 || locks[0].Spec.Task.Name != "job-worker-b" {
		t.Fatalf("later member not admitted: %+v", locks)
	}
	for i := range tasks {
		if tasks[i].Name == "job-worker-b" {
			observeExecution(t, r, &tasks[i], native.TaskPhaseSucceeded)
		}
	}
	proveCleanup(t, r, &locks[0])
	reconcileJob(t, r, req, 3)
	_, remaining := processObjects(t, r)
	if len(remaining) != 0 {
		t.Fatal("blocked first member starved later cleanup")
	}
	j = readExecutionJob(t, r, req)
	if len(j.Status.Tasks) != 2 || j.Status.Tasks[0].Phase != native.TaskPhasePending || j.Status.Tasks[1].Phase != native.TaskPhaseSucceeded || j.Status.Phase != native.JobPhasePending {
		t.Fatalf("incomplete aggregate: %+v", j.Status)
	}
}

func TestProcessesStaleResultNeverReacquiresOrCompletes(t *testing.T) {
	r, req := processesFixture(t, 2)
	reconcileJob(t, r, req, 7)
	tasks, locks := processObjects(t, r)
	observeExecution(t, r, &tasks[0], native.TaskPhaseSucceeded)
	tasks[0].Status.ObservedGeneration--
	if err := r.Client.Status().Update(context.Background(), &tasks[0]); err != nil {
		t.Fatal(err)
	}
	observeExecution(t, r, &tasks[1], native.TaskPhaseSucceeded)
	proveCleanup(t, r, &locks[1])
	reconcileJob(t, r, req, 3)
	j := readExecutionJob(t, r, req)
	_, remaining := processObjects(t, r)
	if j.Status.Phase != native.JobPhasePending || j.Status.Tasks[0].Phase != native.TaskPhasePending || len(remaining) != 1 || remaining[0].UID != locks[0].UID {
		t.Fatalf("stale result released or reexecuted: %+v %+v", j.Status, remaining)
	}
}

func TestProcessesStopFanoutSurvivesFirstPatchError(t *testing.T) {
	r, req := processesFixture(t, 2)
	reconcileJob(t, r, req, 7)
	j := readExecutionJob(t, r, req)
	j.Spec.Suspended = true
	j.Generation++
	if err := r.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if task, ok := obj.(*native.Task); ok && task.Name == "job-worker" {
			return errors.New("first stop patch unavailable")
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("injected stop error lost")
	}
	tasks, locks := processObjects(t, r)
	if len(locks) != 2 || !tasks[1].Spec.Suspended {
		t.Fatal("first stop error starved sibling cancellation")
	}
	j = readExecutionJob(t, r, req)
	if !meta.IsStatusConditionTrue(j.Status.Conditions, jobTaskSetRetiringCondition) {
		t.Fatal("retirement fence not durable before effects")
	}
	r.Client = base
	reconcileJob(t, r, req, 2)
	tasks, _ = processObjects(t, r)
	for _, task := range tasks {
		if !task.Spec.Suspended {
			t.Fatal("stop did not recover")
		}
	}
}
