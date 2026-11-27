package clusterupgrade

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	runtimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterOnlyRunnerChecksPodIdentityAndKeepsTemplate(t *testing.T) {
	const childUID = "10000000-0000-4000-8000-000000000001"
	image := "registry.example/upgrade@" + digest
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "upgrade-attempt-" + childUID, Namespace: Namespace, UID: "local-job",
		Labels: map[string]string{"rlark.io/cluster-upgrade-job-uid": childUID},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "executor", Namespace: Namespace, UID: "local-pod",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec: corev1.PodSpec{
			ServiceAccountName: JobServiceAccount,
			Containers:         []corev1.Container{{Name: "upgrade", Image: image}},
		},
	}
	object := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "site-agent", Namespace: Namespace, UID: "stable-deployment"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			HostNetwork: true, Containers: []corev1.Container{{
				Name: "agent", Image: "old-agent", Args: []string{"--mode=cluster", "--image=old-tools", "--external-config=keep"},
			}},
		}}},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: PolicyConfigMap, Namespace: Namespace},
		Data:       map[string]string{"mode": ClusterOnlyMode, "deployment": "site-agent", "executorImage": image},
	}
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: CheckpointConfigMap, Namespace: Namespace}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace}}
	members := fake.NewClientset(job, pod, object, checkpoint, lease, policy)
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, coordinationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	reader := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	req := JobRequest{JobUID: childUID, PodName: pod.Name, PodUID: "wrong", Deployment: object.Name, DeployConfig: imagesConfig, AutoRollback: true, HealthTimeout: time.Second}
	if err := runImageJob(t.Context(), members, reader, req); err == nil {
		t.Fatal("Job with wrong Pod UID was accepted")
	}
	lease.Spec.HolderIdentity = ptr.To("upgrade-job/" + childUID)
	lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Hour)))
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(300))
	lease.Annotations = map[string]string{executorAnnotation: "missing-executor/old-pod/local-job"}
	_, err := members.CoordinationV1().Leases(Namespace).Update(t.Context(), lease, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req.PodUID = string(pod.UID)
	if err := runImageJob(t.Context(), members, reader, req); err != nil {
		t.Fatal(err)
	}
	got, err := members.AppsV1().Deployments(Namespace).Get(t.Context(), object.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	container := got.Spec.Template.Spec.Containers[0]
	if !got.Spec.Template.Spec.HostNetwork || container.Image != "registry.example/agent@"+digest ||
		container.Args[1] != "--image=registry.example/tools@"+digest || container.Args[2] != "--external-config=keep" {
		t.Fatalf("unexpected template %+v", got.Spec.Template.Spec)
	}
	record, err := members.CoreV1().ConfigMaps(Namespace).Get(t.Context(), CheckpointConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	completion, found, err := ReadCheckpoint(record, childUID)
	if err != nil || !found || !completion.Completed || completion.RollingBack || completion.Deployment.UID != object.UID {
		t.Fatalf("completed attempt was not recorded: %+v, found=%v, err=%v", completion, found, err)
	}
	read, err := members.CoordinationV1().Leases(Namespace).Get(t.Context(), LeaseName, metav1.GetOptions{})
	if err != nil || read.Spec.HolderIdentity != nil {
		t.Fatalf("member lease was not released: %v, %v", read, err)
	}
}

func TestClusterOnlyPolicyRejectsNodeAgentDaemonSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rlark-agent-node", "rlark-node-agent"} {
		t.Run(name, func(t *testing.T) {
			reader := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(&appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
			}).Build()
			if err := ValidateClusterOnly(t.Context(), reader); err == nil || !strings.Contains(err.Error(), "node Agents") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestUnconfiguredMembersResolveNoPolicyOnlyWithoutAstribotLease(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	standard := runtimefake.NewClientBuilder().WithScheme(scheme).Build()
	if policy, err := ResolveJobPolicy(t.Context(), standard); policy != nil || err != nil {
		t.Fatalf("unexpected member policy: %v, %v", policy, err)
	}
	managed := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace},
	}).Build()
	if _, err := ResolveJobPolicy(t.Context(), managed); err == nil || !strings.Contains(err.Error(), "no enabled executor policy") {
		t.Fatalf("a managed member could use the stock installer: %v", err)
	}
}

func TestVanishedUpgradePodUsesOriginalJobEvidence(t *testing.T) {
	original := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: JobName("child"), Namespace: Namespace, UID: "job-uid", Labels: map[string]string{ChildUIDLabel: "child"}}}
	failed := original.DeepCopy()
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	for _, test := range []struct {
		name, holder, job, executor string
		objects                     []runtime.Object
		want                        bool
	}{
		{"same job resumes", "upgrade-job/child", "job-uid", "missing/pod-uid/job-uid", []runtime.Object{original}, true},
		{"another attempt waits for active job", "", "", "missing/pod-uid/job-uid", []runtime.Object{original}, false},
		{"terminal original job", "", "", "missing/pod-uid/job-uid", []runtime.Object{failed}, true},
		{"job and pod garbage collected", "", "", "missing/pod-uid/job-uid", nil, true},
		{"legacy executor with no job UID", "", "", "missing/pod-uid", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			member := fake.NewClientset(test.objects...)
			ok, err := executorExited(t.Context(), member, "upgrade-job/child", test.executor, test.holder, test.job)
			if err != nil || ok != test.want {
				t.Fatalf("allowed = %v, err = %v", ok, err)
			}
		})
	}
}
