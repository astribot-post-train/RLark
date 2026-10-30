package agent

import (
	"context"
	"errors"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// This exercises the actual reservation controller, mTLS Task transport and
// Host/Coordinator together. Controller-only observations cannot prove that the
// Agent permits partial admission or that cleanup fences the next execution.
func TestIndependentProcessesShareOneNodeOnlyAfterConfirmedCleanup(t *testing.T) {
	for _, firstResult := range []rt.InstancePhase{"Succeeded", "Failed"} {
		t.Run(string(firstResult), func(t *testing.T) {
			ctx := context.Background()
			s, id, first := taskFixtureWithJob(t, []byte(`{"argv":["/usr/bin/true"],"env":[],"directory":"/work"}`), func(j *native.Job) {
				second := *j.Spec.Tasks[0].DeepCopy()
				second.Name = "worker-b"
				j.Spec.Tasks = append(j.Spec.Tasks, second)
			})
			// Emulate API-assigned instance identity for the newly expanded child.
			s.Reporter.Client = interceptor.NewClient(s.Reporter.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if task, ok := obj.(*native.Task); ok {
					task.UID, task.Generation = types.UID(task.Name+"-uid"), 1
				}
				return c.Create(ctx, obj, opts...)
			}})
			reconcile := func() {
				t.Helper()
				// Reconstruct the controller on each pass; no in-memory scheduling
				// decision is allowed to be necessary for eventual progress.
				_, err := (controller.JobReservationReconciler{Client: s.Reporter.Client}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "job"}})
				must(t, err)
			}
			for range 6 {
				reconcile()
			}
			var tasks native.TaskList
			must(t, s.Reporter.Client.List(ctx, &tasks, client.InNamespace(id.Namespace)))
			if len(tasks.Items) != 2 {
				t.Fatalf("expected both children, got %d", len(tasks.Items))
			}
			var waiting native.Task
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: "job-worker-b"}, &waiting))
			assertAssignment := func(uid string) *Assignment {
				t.Helper()
				a, err := s.Handle(ctx, id, TaskRequest{Action: "assignment"})
				must(t, err)
				if a == nil || string(a.Task.UID) != uid {
					t.Fatalf("wrong assignment for %s: %+v", uid, a)
				}
				return a
			}
			assertAssignment(string(first.Task.UID))
			transportClient := transport(t, s, id)
			supervisor := &memorySupervisor{}
			execution := ExecutionObserver{Client: transportClient, Supervisor: supervisor}
			// The waiting member has no allocation; it must not block this start.
			must(t, execution.Reconcile(ctx))
			if supervisor.ensures != 1 || supervisor.record.Task.ObjectUID != string(first.Task.UID) {
				t.Fatal("partial admission did not start exactly the assigned member")
			}
			supervisor.record.Phase, supervisor.record.Cleaned = firstResult, true
			must(t, execution.Reconcile(ctx)) // Publish the terminal observation.
			for range 3 {
				reconcile()
			}
			// A terminal result alone does not transfer the node lock.
			assertAssignment(string(first.Task.UID))
			supervisor.stopErr = errors.New("supervisor unavailable")
			if err := execution.reconcile(ctx, false); err == nil {
				t.Fatal("unconfirmed cleanup was accepted")
			}
			for range 3 {
				reconcile()
			}
			assertAssignment(string(first.Task.UID))
			supervisor.stopErr = nil
			must(t, execution.reconcile(ctx, false)) // Prove durable cancellation.
			for range 6 {
				reconcile()
			}
			second := assertAssignment(string(waiting.UID))
			if second.Allocation.Spec.Node != first.Allocation.Spec.Node {
				t.Fatal("waiting member did not reuse the sole node")
			}
			var retained native.Task
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&first.Task), &retained))
			if retained.UID != first.Task.UID || !taskcontract.Completed(&retained) {
				t.Fatal("completed first member identity was lost or replayed")
			}
			// A late request from the old execution cannot act on the reused lock.
			if _, err := s.Handle(ctx, id, requestFor(first, "cleanup")); err == nil {
				t.Fatal("old member could clean the new member's allocation")
			}
			// A fresh execution observer must admit the second member even though
			// its sibling's allocation has already been released.
			secondSupervisor := &memorySupervisor{}
			execution = ExecutionObserver{Client: transportClient, Supervisor: secondSupervisor}
			must(t, execution.Reconcile(ctx))
			if secondSupervisor.ensures != 1 || secondSupervisor.record.Task.ObjectUID != string(waiting.UID) {
				t.Fatal("released sibling blocked waiting member's start")
			}
			secondSupervisor.record.Phase, secondSupervisor.record.Cleaned = "Succeeded", true
			must(t, execution.Reconcile(ctx))
			must(t, execution.reconcile(ctx, false))
			for range 6 {
				reconcile()
			}
			var job native.Job
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &job))
			if string(job.Status.Phase) != string(firstResult) || len(job.Status.Tasks) != 2 {
				t.Fatalf("unexpected aggregate after both cleanups: %+v", job.Status)
			}
			if err := s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&second.Allocation), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
				t.Fatalf("terminal allocation remains: %v", err)
			}
			must(t, execution.Reconcile(ctx))
			if supervisor.ensures != 1 || secondSupervisor.ensures != 1 {
				t.Fatal("reconciliation replayed a terminal member")
			}
		})
	}
}
