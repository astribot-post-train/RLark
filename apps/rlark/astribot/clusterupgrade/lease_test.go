package clusterupgrade

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestMemberUpgradeLeaseExcludesWritersOnlyOnSameCluster(t *testing.T) {
	ctx := t.Context()
	object := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace}}
	first := fake.NewClientset(object.DeepCopy()).CoordinationV1().Leases(Namespace)
	second := fake.NewClientset(object.DeepCopy()).CoordinationV1().Leases(Namespace)
	guard, work, err := Acquire(ctx, first, "upgrade-job/one", "pod-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Release(context.Background()) })
	_, _, err = Acquire(ctx, first, "rlarkctl-release/one", "run-one", nil)
	var conflict *LeaseConflict
	if !errors.As(err, &conflict) || conflict.Holder != "upgrade-job/one" {
		t.Fatalf("holder was not reported: %v", err)
	}
	other, _, err := Acquire(ctx, second, "upgrade-job/two", "pod-two", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Release(context.Background()) })
	if err := guard.Check(work); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredMemberUpgradeLeaseNeedsExitEvidence(t *testing.T) {
	ctx := t.Context()
	object := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace, Annotations: map[string]string{executorAnnotation: "old-pod"}},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("upgrade-job/old"), RenewTime: ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Hour))),
			LeaseDurationSeconds: ptr.To(int32(300)),
		},
	}
	leases := fake.NewClientset(object).CoordinationV1().Leases(Namespace)
	if guard, _, err := Acquire(ctx, leases, "upgrade-job/new", "new-pod", nil); err == nil {
		_ = guard.Release(ctx)
		t.Fatal("expiry was treated as evidence of exit")
	}
	guard, _, err := Acquire(ctx, leases, "upgrade-job/new", "new-pod", func(_ context.Context, holder, executor string) (bool, error) {
		return holder == "upgrade-job/old" && executor == "old-pod", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Release(context.Background()) })
	if err := guard.Release(ctx); err != nil {
		t.Fatal(err)
	}
	released, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil || released.Spec.HolderIdentity != nil {
		t.Fatalf("lease was not released: %v, %v", released, err)
	}
}
