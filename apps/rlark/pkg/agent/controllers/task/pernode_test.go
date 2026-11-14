package task

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	agentruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
)

func perNodeExecutionTask() *rlarkv1alpha1.Task {
	task := claimedExecutionTask(rlarkv1alpha1.TaskPhasePending)
	task.Annotations[host.TaskClaimedCarrierTypeAnnotation] = taskapi.CarrierTypeKubernetesDaemonSet
	task.Spec.Placement = rlarkv1alpha1.TaskPlacementSpec{NodeSelector: map[string]string{"rlark.io/node-category": "robot"}, PerNode: true}
	task.Spec.Workload.Carrier.Type = taskapi.CarrierTypeKubernetesDaemonSet
	task.Spec.Workload.Carrier.Parameters.Raw = nil
	task.Spec.Workload.Template.Spec.Containers = []corev1.Container{{Name: "main", Image: "station:v1"}}
	return task
}

// perNodePod is the Pod the DaemonSet controller would place on node.
func perNodePod(daemonSet *appsv1.DaemonSet, node string, ready bool, waiting string) *corev1.Pod {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: daemonSet.Name + "-" + node, Namespace: daemonSet.Namespace, UID: types.UID("pod-" + node),
			Labels:          map[string]string{ManagementTaskUIDLabel: daemonSet.Spec.Selector.MatchLabels[ManagementTaskUIDLabel]},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: daemonSet.Name, UID: daemonSet.UID, Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "main", Image: "station:v1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if waiting != "" {
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}}}
	}
	return pod
}

// TestPerNodeTaskFollowsItsNodes runs a per-node Task through the Agent: the
// pull reconcile renders a DaemonSet, and each DaemonSet change the push
// controller sees is projected into the Task's phase and per-node instances as
// nodes join, fail and leave. The Task is never Failed.
func TestPerNodeTaskFollowsItsNodes(t *testing.T) {
	scheme := executionScheme(t)
	task := perNodeExecutionTask()
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{ManagementTaskUIDLabel: string(task.UID)}}
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: task.Name, Namespace: tenant.DefaultNamespace, UID: "daemonset-uid", ResourceVersion: "1", Generation: 1, Annotations: map[string]string{
			ManagementTaskNameAnnotation: task.Name, ManagementTaskNamespaceAnnotation: task.Namespace,
			ManagementTaskUIDAnnotation: string(task.UID), ManagementTaskGenerationAnnotation: fmt.Sprintf("%d", task.Generation),
		}},
		Spec: appsv1.DaemonSetSpec{Selector: selector, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector.MatchLabels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "station:v1"}}}}},
	}
	management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(task).Build()
	local := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(existing).WithObjects(existing).
		WithIndex(&corev1.Event{}, "involvedObject.uid", func(object client.Object) []string {
			return []string{string(object.(*corev1.Event).InvolvedObject.UID)}
		}).Build()
	execution, err := NewKubernetesExecutionCoordinator(local, "rlark:test")
	if err != nil {
		t.Fatal(err)
	}
	controller := newHostedTestController(t, base.Controller{AgentType: string(rlarkv1alpha1.AgentTypeKubernetes), ManagementNamespace: "cluster", ManagementClient: management, LocalKubeClient: local}, execution)
	pull := &pullReconciler{c: controller}
	key := client.ObjectKeyFromObject(task)
	if _, err := pull.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var daemonSet appsv1.DaemonSet
	if err := local.Get(context.Background(), client.ObjectKeyFromObject(existing), &daemonSet); err != nil {
		t.Fatal(err)
	}
	if daemonSet.Spec.Template.Spec.NodeSelector["rlark.io/node-category"] != "robot" || *daemonSet.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable != intstr.FromInt32(1) {
		t.Fatalf("rendered DaemonSet spec=%#v", daemonSet.Spec)
	}

	// observe stands in for the DaemonSet controller: it sets the DaemonSet's
	// counts and the Pods on the nodes, then lets the push and pull controllers
	// report them.
	observe := func(desired, updated, available int32, pods ...*corev1.Pod) rlarkv1alpha1.TaskStatus {
		t.Helper()
		var list corev1.PodList
		if err := local.List(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			if err := local.Delete(context.Background(), &list.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
		for _, pod := range pods {
			if err := local.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
		}
		if err := local.Get(context.Background(), client.ObjectKeyFromObject(existing), &daemonSet); err != nil {
			t.Fatal(err)
		}
		daemonSet.Status = appsv1.DaemonSetStatus{ObservedGeneration: daemonSet.Generation, DesiredNumberScheduled: desired, CurrentNumberScheduled: desired, UpdatedNumberScheduled: updated, NumberReady: available, NumberAvailable: available, NumberUnavailable: desired - available}
		if err := local.Status().Update(context.Background(), &daemonSet); err != nil {
			t.Fatal(err)
		}
		if _, err := (&pushDaemonSetReconciler{c: controller}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(existing)}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-controller.triggers.events:
		case <-time.After(time.Second):
			t.Fatal("runtime trigger was not emitted")
		}
		if _, err := pull.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		var got rlarkv1alpha1.Task
		if err := management.Get(context.Background(), key, &got); err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	running := func(node string) rlarkv1alpha1.TaskInstanceStatus {
		return rlarkv1alpha1.TaskInstanceStatus{Node: node, Name: "task-" + node, Phase: rlarkv1alpha1.TaskInstanceRunning, Image: "station:v1"}
	}

	status := observe(2, 2, 2, perNodePod(&daemonSet, "robot-a", true, ""), perNodePod(&daemonSet, "robot-b", true, ""))
	if status.Phase != rlarkv1alpha1.TaskPhaseRunning || !reflect.DeepEqual(status.ObservedNodes, []string{"robot-a", "robot-b"}) ||
		!reflect.DeepEqual(status.Instances, []rlarkv1alpha1.TaskInstanceStatus{running("robot-a"), running("robot-b")}) {
		t.Fatalf("two nodes: status=%#v", status)
	}

	// A node joins the class: its instance is starting, the Task waits for it.
	status = observe(3, 3, 2, perNodePod(&daemonSet, "robot-c", false, "ContainerCreating"), perNodePod(&daemonSet, "robot-a", true, ""), perNodePod(&daemonSet, "robot-b", true, ""))
	joining := rlarkv1alpha1.TaskInstanceStatus{Node: "robot-c", Name: "task-robot-c", Phase: rlarkv1alpha1.TaskInstancePending, Image: "station:v1", Message: "ContainerCreating"}
	if status.Phase != rlarkv1alpha1.TaskPhasePending || status.Message != "3 of 3 instances updated, 2 available" ||
		!reflect.DeepEqual(status.Instances, []rlarkv1alpha1.TaskInstanceStatus{running("robot-a"), running("robot-b"), joining}) {
		t.Fatalf("node joined: status=%#v", status)
	}

	// Its instance keeps crashing: the instance is Failed, the Task only Pending.
	status = observe(3, 3, 2, perNodePod(&daemonSet, "robot-a", true, ""), perNodePod(&daemonSet, "robot-b", true, ""), perNodePod(&daemonSet, "robot-c", false, "CrashLoopBackOff"))
	failing := rlarkv1alpha1.TaskInstanceStatus{Node: "robot-c", Name: "task-robot-c", Phase: rlarkv1alpha1.TaskInstanceFailed, Image: "station:v1", Message: "pod task-robot-c container main: CrashLoopBackOff"}
	if status.Phase != rlarkv1alpha1.TaskPhasePending || status.Message != failing.Message ||
		!reflect.DeepEqual(status.Instances, []rlarkv1alpha1.TaskInstanceStatus{running("robot-a"), running("robot-b"), failing}) {
		t.Fatalf("node failing: status=%#v", status)
	}

	// A node that stops reporting keeps its Pod, which is no longer ready.
	unreachable := perNodePod(&daemonSet, "robot-b", false, "")
	status = observe(3, 3, 2, perNodePod(&daemonSet, "robot-a", true, ""), unreachable, perNodePod(&daemonSet, "robot-c", true, ""))
	if status.Phase != rlarkv1alpha1.TaskPhasePending || status.Instances[1].Phase != rlarkv1alpha1.TaskInstancePending || status.Instances[1].Message != "not ready" {
		t.Fatalf("node unreachable: status=%#v", status)
	}

	// The node leaves the class and takes its instance with it.
	status = observe(2, 2, 2, perNodePod(&daemonSet, "robot-a", true, ""), perNodePod(&daemonSet, "robot-c", true, ""))
	if status.Phase != rlarkv1alpha1.TaskPhaseRunning || !reflect.DeepEqual(status.Instances, []rlarkv1alpha1.TaskInstanceStatus{running("robot-a"), running("robot-c")}) {
		t.Fatalf("node left: status=%#v", status)
	}
}

func TestPerNodeInstancesAreSortedByNodeAndKeepTheMainImage(t *testing.T) {
	instances := agentruntime.InstanceSnapshot{Instances: []agentruntime.Instance{
		{Name: "task-z", Phase: "Pending", Conditions: []agentruntime.InstanceCondition{{Scope: "instance", Subject: "task-z", Reason: "FailedScheduling"}, {Scope: "image", Subject: "main", Message: "station:v2"}}},
		{Name: "task-y", Phase: "Running", Conditions: []agentruntime.InstanceCondition{{Scope: "node", Subject: "a"}, {Scope: "image", Subject: "sidecar", Message: "tools:v1"}, {Scope: "image", Subject: "main", Message: "station:v1"}, {Scope: "instance", Subject: "task-y", Reason: "Ready"}}},
	}}
	got := perNodeInstances(instances)
	want := []rlarkv1alpha1.TaskInstanceStatus{
		{Name: "task-z", Phase: rlarkv1alpha1.TaskInstancePending, Image: "station:v2", Message: "FailedScheduling"},
		{Node: "a", Name: "task-y", Phase: rlarkv1alpha1.TaskInstanceRunning, Image: "station:v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("instances=%#v, want %#v", got, want)
	}
}
