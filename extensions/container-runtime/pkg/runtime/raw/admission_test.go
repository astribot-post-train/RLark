package raw

import (
	"context"
	"testing"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/rawallocation"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeAdmissionRevalidatesTaskAndWholeGroup(t *testing.T) {
	for _, variant := range []string{"valid", "direct", "tenant", "generation", "uid", "owner", "suspended", "unassigned-peer", "partial-commit", "stale-peer", "replacement", "missing-peer", "missing-peer-task", "suspended-peer-task", "unbound-peer-task", "foreign-peer-owner"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
			job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "training", UID: "job"}}
			objects := []client.Object{cluster, job}
			for i, name := range []string{"a", "b"} {
				objects = append(objects, &api.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents", UID: types.UID(name)}, Spec: api.NodeSpec{AgentType: api.AgentTypeRaw}, Status: api.NodeStatus{Phase: api.NodeOnline, LastHeartbeatTime: &metav1.Time{Time: time.Now()}, Raw: &api.RawNodeStatus{Incarnation: "container", TrainingIP: []string{"10.0.0.1", "10.0.0.2"}[i], NetworkScope: "private", Environment: "env", AuthorizedGPUUUIDs: []string{"GPU-" + name}}}})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(objects...).Build()
			manager := rawallocation.Manager{Client: c}
			keys := []client.ObjectKey{{Namespace: "agents", Name: "a"}, {Namespace: "agents", Name: "b"}}
			bindings, err := manager.Reserve(ctx, rawallocation.Request{Cluster: client.ObjectKey{Name: "cloud"}, JobUID: "job", TenantID: tenant.DefaultID, ExecutionID: "run", Environment: "env", Nodes: keys})
			if err != nil {
				t.Fatal(err)
			}
			for i := range bindings {
				bindings[i], err = manager.AssignTask(ctx, keys[i], bindings[i], tenant.DefaultID, "task-"+keys[i].Name)
				if err != nil {
					t.Fatal(err)
				}
			}
			controller := true
			task := &api.Task{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "task-a", Generation: 1, Annotations: map[string]string{utils.ParentUIDAnnotation: "job"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}}
			task.Spec.Runtime.Kind = "Raw"
			task.Spec.Placement.Binding = &bindings[0]
			peerTask := task.DeepCopy()
			peerTask.Name, peerTask.UID = "peer", "task-b"
			peerTask.Spec.Placement.Binding = &bindings[1]
			switch variant {
			case "suspended-peer-task":
				peerTask.Spec.Suspended = true
			case "unbound-peer-task":
				peerTask.Spec.Placement.Binding = nil
			case "foreign-peer-owner":
				peerTask.OwnerReferences[0].UID = "foreign"
			}
			if variant != "missing-peer-task" {
				if err := c.Create(ctx, peerTask); err != nil {
					t.Fatal(err)
				}
			}
			identity := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "worker"}, ObjectUID: "task-a", Generation: 1}
			switch variant {
			case "direct":
				task.OwnerReferences = nil
			case "tenant":
				task.Spec.Placement.Binding.JobUID = "another"
			case "generation":
				identity.Generation = 2
			case "uid":
				identity.ObjectUID = "replacement"
			case "owner":
				task.Annotations[utils.ParentUIDAnnotation] = "forged"
			case "suspended":
				task.Spec.Suspended = true
			}
			if err := c.Create(ctx, task); err != nil {
				t.Fatal(err)
			}
			if variant == "unassigned-peer" || variant == "partial-commit" || variant == "stale-peer" || variant == "replacement" || variant == "missing-peer" {
				var peer api.Node
				if err := c.Get(ctx, keys[1], &peer); err != nil {
					t.Fatal(err)
				}
				switch variant {
				case "unassigned-peer":
					peer.Status.Reservation.TaskUID = ""
				case "partial-commit":
					peer.Status.Reservation.Committed = false
				case "stale-peer":
					peer.Status.LastHeartbeatTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
				case "replacement":
					peer.Status.Raw.Incarnation = "new"
				}
				if variant == "missing-peer" {
					err = c.Delete(ctx, &peer)
				} else {
					err = c.Status().Update(ctx, &peer)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			admission := NodeAdmission{Client: c, Node: keys[0], NodeUID: "a", Incarnation: "container", TenantID: tenant.DefaultID}
			err = admission.Authorize(ctx, identity)
			if (err == nil) != (variant == "valid") {
				t.Fatalf("admission %s: %v", variant, err)
			}
		})
	}
}
