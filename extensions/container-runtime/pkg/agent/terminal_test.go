package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestScopedNaturalCompletionCleansBeforeJobRelease(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Failed"} {
		for _, failure := range []string{"supervisor unavailable", "terminal response lost", "cleanup response lost"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				s, id, a := taskFixture(t)
				ctx := context.Background()
				supervisor := &memorySupervisor{}
				c := transport(t, s, id)
				must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
				supervisor.record.Phase = rt.InstancePhase(phase)
				supervisor.record.Cleaned = true
				lost := false
				s.Reporter.Client = interceptor.NewClient(s.Reporter.Client.(client.WithWatch), interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						if err := c.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
							return err
						}
						_, task := obj.(*native.Task)
						_, allocation := obj.(*ext.ContainerAllocation)
						if !lost && (failure == "terminal response lost" && task || failure == "cleanup response lost" && allocation) {
							lost = true
							return errors.New("committed response lost")
						}
						return nil
					},
				})
				c = transport(t, s, id)
				err := (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx)
				if failure == "terminal response lost" && (err == nil || !lost) {
					t.Fatal("did not lose terminal response")
				}
				var observed native.Task
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &observed))
				if !taskcontract.Completed(&observed) || string(observed.Status.Phase) != phase {
					t.Fatalf("terminal result not reported: %+v", observed.Status)
				}
				result := observed.Status.DeepCopy()
				if _, err := s.Handle(ctx, id, requestFor(a, "authorize")); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("terminal start authorized: %v", err)
				}
				req := requestFor(a, "status")
				req.ResourceVersion = observed.ResourceVersion
				req.Status = &host.StatusIntent{TaskUID: string(observed.UID), Generation: observed.Generation, Phase: "Running"}
				if _, err := s.Handle(ctx, id, req); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("terminal status regressed: %v", err)
				}
				r := controller.JobReservationReconciler{Client: s.Reporter.Client}
				jobReq := ctrl.Request{NamespacedName: client.ObjectKey{Name: "job"}}
				for range 2 {
					_, err := r.Reconcile(ctx, jobReq)
					must(t, err)
				}
				var job native.Job
				must(t, s.Reporter.Client.Get(ctx, jobReq.NamespacedName, &job))
				if job.Status.Phase != native.JobPhasePending {
					t.Fatal("Job finished before cancellation")
				}
				if failure == "supervisor unavailable" {
					supervisor.stopErr = errors.New("offline")
				}
				// No local observation is needed to persist terminal cancellation.
				err = (ExecutionObserver{Client: c, Supervisor: supervisor}).reconcile(ctx, false)
				if failure != "terminal response lost" && err == nil {
					t.Fatal("injected cleanup failure was hidden")
				}
				supervisor.stopErr = nil
				must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).reconcile(ctx, false))
				for range 3 {
					_, err := r.Reconcile(ctx, jobReq)
					must(t, err)
				}
				must(t, s.Reporter.Client.Get(ctx, jobReq.NamespacedName, &job))
				if string(job.Status.Phase) != phase || job.Status.EndTime == nil {
					t.Fatalf("Job result lost: %+v", job.Status)
				}
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &observed))
				if !reflect.DeepEqual(observed.Status, *result) {
					t.Fatal("cleanup changed process result")
				}
				if err := s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &ext.ContainerAllocation{}); !apierrors.IsNotFound(err) {
					t.Fatalf("allocation remained: %v", err)
				}
				must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
				if supervisor.ensures != 1 || !supervisor.record.Deleted {
					t.Fatal("terminal run restarted")
				}
			})
		}
	}
}

func TestScopedTerminalStatusRequiresCurrentCleanupConditions(t *testing.T) {
	for _, mode := range []string{"missing known", "missing cleaned", "false cleaned", "remove cleaned", "stale known"} {
		t.Run(mode, func(t *testing.T) {
			s, id, a := taskFixture(t)
			must(t, (ExecutionObserver{Client: transport(t, s, id), Supervisor: &memorySupervisor{}}).Reconcile(context.Background()))
			var task native.Task
			must(t, s.Reporter.Client.Get(context.Background(), client.ObjectKeyFromObject(&a.Task), &task))
			req := requestFor(a, "status")
			req.ResourceVersion = task.ResourceVersion
			req.Status = &host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: "Succeeded", Conditions: []host.ConditionMutation{
				{Condition: metav1.Condition{Type: "ExecutionKnown", Status: metav1.ConditionTrue, Reason: "Observed", ObservedGeneration: task.Generation}},
				{Condition: metav1.Condition{Type: "ExecutionCleaned", Status: metav1.ConditionTrue, Reason: "Observed", ObservedGeneration: task.Generation}},
			}}
			switch mode {
			case "missing known":
				req.Status.Conditions[0].Remove = true
			case "missing cleaned":
				req.Status.Conditions = req.Status.Conditions[:1]
			case "false cleaned":
				req.Status.Conditions[1].Condition.Status = metav1.ConditionFalse
			case "remove cleaned":
				req.Status.Conditions[1].Remove = true
			case "stale known":
				req.Status.Conditions[0].Condition.ObservedGeneration--
			}
			if _, err := s.Handle(context.Background(), id, req); !errors.Is(err, ErrCapability) {
				t.Fatalf("unproven terminal status accepted: %v", err)
			}
			if _, err := s.Handle(context.Background(), id, requestFor(a, "cleanup")); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("active Task cleanup accepted: %v", err)
			}
		})
	}
}
