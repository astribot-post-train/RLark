package clusterupgrade

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	runtimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLockedStockSharesReleaseLeaseAndFencesState(t *testing.T) {
	ctx := t.Context()
	image := "registry.example/upgrade@" + digest
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: PolicyConfigMap, Namespace: Namespace},
		Data: map[string]string{"mode": DriverStock, "executorImage": image}}
	state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StockStateName, Namespace: Namespace, ResourceVersion: "12"},
		Data: map[string]string{"deploy-config": "original", "other": "preserved"}}
	identity := ExecutorIdentity{ChildUID: "child", PodName: "stock-pod", PodUID: "pod"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: LockedStockJobName(identity.ChildUID), Namespace: Namespace,
		UID: "job", Labels: map[string]string{ChildUIDLabel: identity.ChildUID}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: identity.PodName, Namespace: Namespace, UID: "pod",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec: corev1.PodSpec{ServiceAccountName: StockJobServiceAccount, Containers: []corev1.Container{{Name: "upgrade", Image: image}}}}
	member := fake.NewClientset(job, pod, state, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace}})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	leases := member.CoordinationV1().Leases(Namespace)
	releaseGuard, _, err := Acquire(ctx, leases, "rlarkctl-release/run", "run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdStock(ctx, member, reader, identity); err == nil {
		t.Fatal("stock installer entered while release held the Lease")
	}
	if err := releaseGuard.Release(ctx); err != nil {
		t.Fatal(err)
	}
	stock, err := holdStock(ctx, member, reader, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer stock.Release()
	if guard, _, err := Acquire(ctx, leases, "rlarkctl-release/next", "next", nil); err == nil {
		_ = guard.Release(ctx)
		t.Fatal("release entered while stock installer held the Lease")
	}
	baseline, err := stock.ReadState(stock.Context)
	if err != nil {
		t.Fatal(err)
	}
	if err := stock.SaveState(stock.Context, baseline, "effective"); err != nil {
		t.Fatal(err)
	}
	read, err := stock.ReadState(stock.Context)
	if err != nil || read.Data["deploy-config"] != "effective" || read.Data["other"] != "preserved" || read.Data[StockAttemptKey] != identity.ChildUID {
		t.Fatalf("state = %v, %v", read, err)
	}
	if err := member.CoreV1().Pods(Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stock.SaveState(stock.Context, read, "not permitted"); err == nil {
		t.Fatal("deleted executor wrote state")
	}
	stock.Release()
	if err := stock.Check(ctx); err == nil {
		// The stale Pod is fenced independently of the caller's context.
		t.Fatalf("stale Pod check = %v", err)
	}
}
