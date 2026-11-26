package clusterupgradejob

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
