package clusterupgradejob

import (
	"encoding/json"
	"strings"
	"testing"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckJobResultReportsOnlyOwnedUpgradeTerminations(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"job-name": "upgrade-example"}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: StateStoreNamespace, Name: "upgrade-example", UID: "job-uid"},
		Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	pod := func(name string, ownerUID types.UID, exitCode int32, message string) *corev1.Pod {
		ref := metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))
		ref.UID = ownerUID
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{*ref}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 8}}},
				{Name: "upgrade", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}}},
			}},
		}
	}
	foreign := pod("earlier-job", "different-job-uid", 9, "secret")
	owned := pod("owned-job", job.UID, 2, `{"kind":"rollout-failed"}`)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, foreign, owned).Build()
	result, err := NewK8sDriver(kube, "example").CheckJobResult(t.Context(), job.Name)
	if err != nil || result.Status != JobStatusFailed || result.ExitCode != 2 || result.Message != "the updated Agent failed to become ready" {
		t.Fatalf("unexpected job result %+v, %v", result, err)
	}
}

func TestImageOnlyJobUseIsolatedIdentityAndImmutableConfiguration(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	driver := NewK8sDriver(kube, "test-member")
	image := "registry.example/rlark-upgrade@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	driver.policy = &clusterupgrade.JobPolicy{ExecutorImage: image, Deployment: "site-agent"}
	child := &rlarkv1alpha1.ClusterUpgradeJob{
		ObjectMeta: metav1.ObjectMeta{Name: "image-upgrade", UID: "00000000-0000-4000-8000-000000000123"},
		Spec:       rlarkv1alpha1.ClusterUpgradeJobSpec{Image: image, AutoRollback: true},
	}
	conf := "apiVersion: rlark.io/v1alpha1\nkind: DeployConfig\nplane: data\nkubernetes:\n  agent-image: " + image + "\n  image: " + image + "\n"
	if _, err := driver.PrepareConfig(t.Context(), child, conf+"  image-pull-policy: Always\n"); err == nil {
		t.Fatal("forbidden field was accepted")
	}
	if !driver.SnapshotsLiveState() {
		t.Fatal("first upgrade would erroneously disable rollback")
	}
	configRef, err := driver.PrepareConfig(t.Context(), child, conf)
	if err != nil {
		t.Fatal(err)
	}
	first, err := driver.StartUpgrade(t.Context(), child, "", configRef)
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.StartUpgrade(t.Context(), child, "", configRef)
	if err != nil || second != first {
		t.Fatalf("reconciliation made a duplicate Job: %q, %v", second, err)
	}
	var job batchv1.Job
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: StateStoreNamespace, Name: first}, &job); err != nil {
		t.Fatal(err)
	}
	template := job.Spec.Template.Spec
	if template.ServiceAccountName != clusterupgrade.JobServiceAccount || len(template.Volumes) != 1 ||
		!template.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatalf("upgrade Job used unsafe identity or mounts: %+v", template)
	}
	var saved corev1.ConfigMap
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: StateStoreNamespace, Name: configRef}, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Immutable == nil || !*saved.Immutable || saved.Labels[childUIDLabel] != string(child.UID) {
		t.Fatalf("upgrade configuration was not bound to its child UID: %+v", saved)
	}
}

func TestVerifyMissingImageOnlyJobRequiresCompletedAttemptAndLiveTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	image := "registry.example/agent@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	config := "apiVersion: rlark.io/v1alpha1\nkind: DeployConfig\nplane: data\nkubernetes:\n  agent-image: " + image + "\n  image: " + image + "\n"
	job := &rlarkv1alpha1.ClusterUpgradeJob{
		ObjectMeta: metav1.ObjectMeta{UID: "current-attempt"},
		Spec:       rlarkv1alpha1.ClusterUpgradeJobSpec{Image: "shared-executor", ConfigPatch: config},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: StateStoreNamespace, Name: "agent", UID: "agent-uid", Generation: 2},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: image, Args: []string{"--image", image}}},
		}}},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1,
		},
	}
	for _, tt := range []struct {
		name                   string
		receiptUID, liveUID    types.UID
		completed, ready, want bool
	}{
		{name: "prior attempt with same executor", receiptUID: "old-attempt", liveUID: deployment.UID, completed: true, ready: true},
		{name: "snapshot only", receiptUID: job.UID, liveUID: deployment.UID, ready: true},
		{name: "replaced deployment", receiptUID: job.UID, liveUID: "replacement", completed: true, ready: true},
		{name: "unready workload", receiptUID: job.UID, liveUID: deployment.UID, completed: true},
		{name: "confirmed", receiptUID: job.UID, liveUID: deployment.UID, completed: true, ready: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkpoint := clusterupgrade.Checkpoint{
				Deployment: clusterupgrade.DeploymentTarget{Name: deployment.Name, UID: deployment.UID, Container: "agent"},
				Target:     clusterupgrade.Images{Agent: image, Tools: image}, Completed: tt.completed,
			}
			payload, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			receipt := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: StateStoreNamespace, Name: clusterupgrade.CheckpointConfigMap},
				Data:       map[string]string{"jobUID": string(tt.receiptUID), "snapshot.json": string(payload)},
			}
			live := deployment.DeepCopy()
			live.UID = tt.liveUID
			if !tt.ready {
				live.Status.ReadyReplicas = 0
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, receipt).Build()
			verified, err := NewK8sDriver(kube, "member").VerifyMissingJobSuccess(t.Context(), job)
			if err != nil || verified != tt.want {
				t.Fatalf("expected verified=%v, got %v, %v", tt.want, verified, err)
			}
		})
	}
}

func TestAgentRequiresStockOptInForDirectUpgradeJob(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{rlarkv1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	member := &rlarkv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "member"}}
	management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(member).Build()
	controller := NewClusterUpgradeJobController(base.Controller{
		AgentType: string(rlarkv1alpha1.AgentTypeKubernetes), ManagementNamespace: member.Name,
		ManagementClient: management, LocalKubeClient: fake.NewClientBuilder().WithScheme(scheme).Build(),
	})
	reconciler := &pullReconciler{c: controller}
	job := &rlarkv1alpha1.ClusterUpgradeJob{}
	if _, err := reconciler.newDriver(t.Context(), job); err == nil || !strings.Contains(err.Error(), "explicitly declare") {
		t.Fatalf("unmarked cluster could select the stock driver: %v", err)
	}
	if err := management.Get(t.Context(), client.ObjectKeyFromObject(member), member); err != nil {
		t.Fatal(err)
	}
	member.Annotations = map[string]string{clusterupgrade.DriverAnnotation: clusterupgrade.DriverStock}
	if err := management.Update(t.Context(), member); err != nil {
		t.Fatal(err)
	}
	driver, err := reconciler.newDriver(t.Context(), job)
	if err != nil || driver == nil || driver.SnapshotsLiveState() {
		t.Fatalf("explicit stock opt-in did not select stock driver: %v", err)
	}
}
