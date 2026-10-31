package docker

import (
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func groupJob(t *testing.T) *native.Job {
	t.Helper()
	replicas := int32(2)
	mk := func(name string, head bool, p *int32) native.JobTaskTemplate {
		carrier := native.TaskWorkloadCarrier{Type: taskapi.CarrierTypeDockerContainer, Version: "v1"}
		if p != nil {
			carrier.Parameters = apiextensionsv1.JSON{Raw: []byte(`{"replicas":2}`)}
		}
		params := `{"role":"worker"}`
		if head {
			params = `{"role":"head","run":{"inline":"echo train"}}`
		}
		return native.JobTaskTemplate{Name: name, Head: head, TaskSpec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Docker"}, Placement: native.TaskPlacementSpec{Environment: "ray"}, Workload: native.TaskWorkloadSpec{Kind: taskapi.WorkloadTypeRay, Version: "v1", Parameters: apiextensionsv1.JSON{Raw: []byte(params)}, Carrier: carrier, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: "main", Image: "ray:2.56.1"}}}}}}}
	}
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job", Generation: 1}, Spec: native.JobSpec{Tasks: []native.JobTaskTemplate{mk("head", true, nil), mk("worker", false, &replicas)}}}
	tenant.SetIdentity(job, tenant.DefaultID, "")
	return job
}

func TestExpandStableRanksAndNoMutation(t *testing.T) {
	job := groupJob(t)
	before := job.DeepCopy()
	members, err := Expand(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 || !members[0].Head || members[0].Rank != 0 || members[1].Rank != 1 || members[2].Rank != 2 {
		t.Fatalf("members=%+v", members)
	}
	if job.Spec.Tasks[1].Workload.Carrier.Parameters.Raw == nil || string(job.Spec.Tasks[1].Workload.Carrier.Parameters.Raw) != string(before.Spec.Tasks[1].Workload.Carrier.Parameters.Raw) {
		t.Fatal("Expand mutated Job")
	}
}

func TestValidatePlanRejectsReplacementAndOverlappingHosts(t *testing.T) {
	job := groupJob(t)
	members, err := Expand(job)
	if err != nil {
		t.Fatal(err)
	}
	tasks := Tasks(job, "agents", members)
	p := ext.RayGroupPlan{Binding: ext.NativeObjectReference{Namespace: "agents", Name: "binding", UID: "binding"}, Job: ext.NativeObjectReference{Name: job.Name, UID: string(job.UID)}, JobGeneration: 1, Cluster: ext.NativeObjectReference{Name: "cluster", UID: "cluster"}, TenantID: tenant.DefaultID, NetworkScope: "vpc", Ports: ext.RayPortPlan{Base: 24000, WorkerCount: 32}}
	for i, t := range tasks {
		p.Members = append(p.Members, ext.GroupMember{Task: ext.NativeObjectReference{Namespace: t.Namespace, Name: t.Name, UID: t.Name}, Generation: 1, Node: ext.NativeObjectReference{Namespace: "agents", Name: "node-" + string(rune('a'+i)), UID: "node-" + string(rune('a'+i))}, NodeRecord: ext.NativeObjectReference{Namespace: "agents", Name: "record-" + string(rune('a'+i)), UID: "record-" + string(rune('a'+i))}, InstanceID: "instance-" + string(rune('a'+i)), CredentialID: "cred", PrivateAddr: "10.0.0." + string(rune('1'+i)), Rank: int32(i), GPUUUIDs: []string{"GPU-" + string(rune('a'+i))}})
	}
	if err := ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	p.Members[1].PrivateAddr = p.Members[0].PrivateAddr
	if ValidatePlan(p) == nil {
		t.Fatal("overlap accepted")
	}
}
