package clusterupgrade

import (
	"errors"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestMemberLeaseOnKubernetesAPI(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for the isolated Kubernetes API test")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx := t.Context()
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	leases := client.CoordinationV1().Leases(Namespace)
	if _, err := leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	first, held, err := Acquire(ctx, leases, "upgrade-job/one", "pod-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Acquire(ctx, leases, "rlarkctl-release/two", "run-two", nil)
	var conflict *LeaseConflict
	if !errors.As(err, &conflict) || conflict.Holder != "upgrade-job/one" {
		t.Fatalf("concurrent release got %v", err)
	}
	if err := first.Check(held); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second, held, err := Acquire(ctx, leases, "rlarkctl-release/two", "run-two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); !errors.As(err, &conflict) {
		t.Fatalf("stale writer cleared the successor's lease: %v", err)
	}
	if err := second.Check(held); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}

	stale, staleWork, err := Acquire(ctx, leases, "rlarkctl-release/stale", "stale", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stale.Release(ctx) }()
	record, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	record.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Hour)))
	if _, err := leases.Update(ctx, record, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	takeover, takeoverWork, err := Acquire(ctx, leases, "upgrade-job/recovery", "recovery/pod-uid", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = takeover.Release(ctx) }()
	if err := stale.Check(staleWork); !errors.As(err, &conflict) {
		t.Fatalf("stale release could still write: %v", err)
	}
	if err := takeover.Check(takeoverWork); err != nil {
		t.Fatal(err)
	}
}
