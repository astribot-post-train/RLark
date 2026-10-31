package job

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/common"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/dockerallocation"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func dockerTemplate(t *testing.T, name string, head bool, replicas int32) api.JobTaskTemplate {
	t.Helper()
	parameters := taskapi.RayExecutionParametersV1{Role: taskapi.RayRoleWorker}
	if head {
		parameters.Role = taskapi.RayRoleHead
	}
	pod := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Image: "registry.example/ray:latest", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: *resourceQuantity("2"), corev1.ResourceName("nvidia.com/gpu"): *resourceQuantity("1")}}}}}}
	template := api.JobTaskTemplate{Name: name, Head: head, TaskSpec: api.TaskSpec{
		Runtime:   api.TaskRuntimeSpec{Kind: "Docker"},
		Placement: api.TaskPlacementSpec{NodeSelector: map[string]string{"pool": "gpu"}},
		Workload:  api.TaskWorkloadSpec{Kind: taskapi.WorkloadTypeRay, Version: taskapi.WorkloadVersionRayV1, Carrier: api.TaskWorkloadCarrier{Type: taskapi.CarrierTypeDockerContainer, Version: "v1"}, Template: pod},
	}}
	if replicas != 1 {
		template.Workload.Carrier.Parameters.Raw = mustJSON(t, taskapi.DockerReplicatedCarrierParametersV1{Replicas: &replicas})
	}
	template.Workload.Parameters.Raw = mustJSON(t, parameters)
	return template
}

func resourceQuantity(value string) *resource.Quantity {
	q := resource.MustParse(value)
	return &q
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDockerTaskRefsExpandReplicasAndValidationRequiresOneHead(t *testing.T) {
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: types.UID("job-uid")}, Spec: api.JobSpec{Tasks: []api.JobTaskTemplate{dockerTemplate(t, "head", true, 1), dockerTemplate(t, "worker", false, 3)}}}
	refs := dockerTaskRefs(job)
	if len(refs) != 4 || refs[0].name != "train-head-0" || refs[3].name != "train-worker-2" {
		t.Fatalf("unexpected expanded refs: %#v", refs)
	}
	if err := validateDockerJob(job); err != nil {
		t.Fatal(err)
	}
	job.Spec.Tasks[0].Head = false
	if err := validateDockerJob(job); err == nil || err.Error() != "Docker Ray Job requires exactly one head template" {
		t.Fatalf("expected one-head validation, got %v", err)
	}
}

func TestResolveDockerNamespaceRequiresEligibleMatchingNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: types.UID("job-uid")}, Spec: api.JobSpec{Tasks: []api.JobTaskTemplate{dockerTemplate(t, "head", true, 1)}}}
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", UID: types.UID("cluster-uid")}, Spec: api.ClusterSpec{AgentNamespace: "agents-a"}}
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Namespace: "agents-a", UID: types.UID("node-uid"), Labels: map[string]string{"pool": "gpu"}}, Spec: api.NodeSpec{AgentType: api.AgentTypeDocker}, Status: api.NodeStatus{Phase: api.NodeOnline, NodeInfo: api.NodeInfo{Incarnation: "engine-1"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, cluster, node).Build()
	r := &Reconciler{RawClient: c}
	resolved, err := r.resolveDockerNamespace(context.Background(), job)
	if err != nil || resolved != "agents-a" {
		t.Fatalf("resolved namespace = %q, err = %v", resolved, err)
	}
	node.Status.Phase = api.NodeOffline
	if err := c.Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolveDockerNamespace(context.Background(), job); !errors.Is(err, dockerallocation.ErrUnavailable) {
		t.Fatalf("offline node should be unavailable, got %v", err)
	}
}

func TestBuildDockerTasksConsumesReplicasAndUsesExpandedHeadIdentity(t *testing.T) {
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: "job-uid"}, Spec: api.JobSpec{Tasks: []api.JobTaskTemplate{dockerTemplate(t, "head", true, 1), dockerTemplate(t, "worker", false, 3)}}}
	for rank, ref := range dockerTaskRefs(job) {
		task, err := buildDockerTask(job, ref, "agents")
		if err != nil {
			t.Fatal(err)
		}
		if len(task.Spec.Workload.Carrier.Parameters.Raw) != 0 {
			t.Fatal("template replicas leaked to execution Task")
		}
		params, err := taskapi.DecodeParametersStrict[taskapi.RayExecutionParametersV1](task.Spec.Workload.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		if params.NodeRankStart != int32(rank) || params.TotalNodes != 4 {
			t.Fatalf("rank parameters: %#v", params)
		}
		if !ref.template.Head && params.HeadTaskName != "train-head-0" {
			t.Fatalf("head=%q", params.HeadTaskName)
		}
	}
}

func TestDockerGPUResourcesRequireWholeCards(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"0", true}, {"1", true}, {"2", true}, {"500m", false}, {"-1", false}} {
		t.Run(tc.value, func(t *testing.T) {
			task := &api.Task{Spec: dockerTemplate(t, "head", true, 1).TaskSpec}
			task.Spec.Workload.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse(tc.value)
			_, _, _, err := dockerTaskResources(task)
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v valid=%v", err, tc.valid)
			}
		})
	}
}

func TestDockerCleanupWaitsForAgentBeforeReleasingAllocation(t *testing.T) {
	ctx := context.Background()
	scheme := jobTestScheme(t)
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: "job-uid", Finalizers: []string{CleanupFinalizer}}}
	task, err := buildDockerTask(job, dockerTaskRef{template: dockerTemplate(t, "head", true, 1), name: "train-head-0"}, "agents")
	if err != nil {
		t.Fatal(err)
	}
	task.UID = "task-uid"
	task.Finalizers = []string{DockerAllocationFinalizer, common.TaskAgentCleanupFinalizer}
	task.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptrBool(true)}}
	binding := api.DockerTaskBinding{TaskUID: "task-uid", NodeName: "docker-1", NodeUID: "node-uid", Incarnation: "engine", JobUID: "job-uid", ClusterUID: "cluster-uid", AllocationID: "allocation"}
	task.Spec.Placement.DockerBinding = &binding
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "docker-1", Namespace: "agents", UID: "node-uid"}, Status: api.NodeStatus{NodeInfo: api.NodeInfo{Incarnation: "engine"}, DockerReservations: []api.DockerNodeReservation{{TaskUID: "task-uid", JobUID: "job-uid", NodeUID: "node-uid", ClusterUID: "cluster-uid", AllocationID: "allocation"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(job, task, node).Build()
	r := &Reconciler{Client: c, RawClient: c, Scheme: scheme}
	if err := c.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cleanupDockerJob(ctx, job, []*api.Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if task.DeletionTimestamp.IsZero() {
		t.Fatal("Task deletion was not requested")
	}
	if _, err := r.cleanupDockerJob(ctx, job, []*api.Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if len(node.Status.DockerReservations) != 1 {
		t.Fatal("reservation released before Agent cleanup")
	}
	controllerutil.RemoveFinalizer(task, common.TaskAgentCleanupFinalizer)
	if err := c.Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cleanupDockerJob(ctx, job, []*api.Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if len(node.Status.DockerReservations) != 0 {
		t.Fatal("reservation not released after cleanup")
	}
}
