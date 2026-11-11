package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type unavailableAuditSink struct{}

func (unavailableAuditSink) Record(context.Context, RebindAuditEvent) error {
	return errors.New("simulated audit outage")
}

// Run only against a disposable workspace with the Station CRDs installed.
func TestLiveStationReconcileAndRebind(t *testing.T) {
	kubeconfig := os.Getenv("STATION_TEST_ISOLATED_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set STATION_TEST_ISOLATED_KUBECONFIG to a disposable workspace")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := stationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	observation := testObservation("node-a", "node-a-uid", now)
	status := observation.Status
	observation.Status = stationv1alpha1.StationObservationStatus{}
	if err := c.Create(ctx, observation); err != nil {
		t.Fatal(err)
	}
	observation.Status = status
	if err := c.Status().Update(ctx, observation); err != nil {
		t.Fatal(err)
	}
	registrations := testRegistrations{testNamespace: {ClusterID: "cluster-a", Namespace: testNamespace}}
	gate := NewSerializedIdentityOperations()
	stationController, err := NewStationReconciler(c, c, registrations, gate, testNodeReadiness{ready: true}, DefaultOfflineAfter, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: observation.Name}}
	if _, err := stationController.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := stationController.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	station := &stationv1alpha1.Station{}
	stationKey := types.NamespacedName{Namespace: testNamespace, Name: StationName("cluster-a", "node-a-uid")}
	if err := c.Get(ctx, stationKey, station); err != nil {
		t.Fatal(err)
	}
	if station.Spec.Binding.Version != 1 || station.Status.Phase != stationv1alpha1.StationPhaseBound {
		t.Fatalf("unexpected initial Station: %#v", station)
	}
	sequence := &stationv1alpha1.StationSequence{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: SequenceName}, sequence); err != nil {
		t.Fatal(err)
	}
	first, stale := sequence.DeepCopy(), sequence.DeepCopy()
	first.Status.LastAssigned++
	stale.Status.LastAssigned++
	if err := c.Status().Patch(ctx, first, client.MergeFromWithOptions(sequence, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatalf("first sequence allocation: %v", err)
	}
	if err := c.Status().Patch(ctx, stale, client.MergeFromWithOptions(sequence, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
		t.Fatalf("stale sequence allocation should conflict, got %v", err)
	}
	const concurrentNodes = 6
	for i := 0; i < concurrentNodes; i++ {
		item := testObservation(fmt.Sprintf("node-concurrent-%d", i), fmt.Sprintf("node-uid-%d", i), now)
		itemStatus := item.Status
		item.Status = stationv1alpha1.StationObservationStatus{}
		if err := c.Create(ctx, item); err != nil {
			t.Fatal(err)
		}
		item.Status = itemStatus
		if err := c.Status().Update(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, concurrentNodes)
	var workers sync.WaitGroup
	for i := 0; i < concurrentNodes; i++ {
		controller, err := NewStationReconciler(c, c, registrations, NewSerializedIdentityOperations(), testNodeReadiness{ready: true}, DefaultOfflineAfter, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			_, err := controller.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: fmt.Sprintf("node-concurrent-%d", index)}})
			errs <- err
		}(i)
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent allocation: %v", err)
		}
	}
	var allocated stationv1alpha1.StationList
	if err := c.List(ctx, &allocated, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool)
	for _, item := range allocated.Items {
		if seen[item.Spec.Sequence] {
			t.Fatalf("duplicate Station sequence %d", item.Spec.Sequence)
		}
		seen[item.Spec.Sequence] = true
	}
	if len(allocated.Items) != concurrentNodes+1 {
		t.Fatalf("Station count = %d", len(allocated.Items))
	}
	operation := &stationv1alpha1.StationRebind{
		ObjectMeta: metav1.ObjectMeta{Name: "live-rebind", Namespace: testNamespace},
		Spec:       stationv1alpha1.StationRebindSpec{RequestID: "live-request", StationRef: stationv1alpha1.StationReference{Name: station.Name, UID: string(station.UID)}, ExpectedBindingVersion: 1, TargetNodeName: "node-b", TargetNodeUID: "node-b-uid", Reason: "isolated test", RequestedBy: "test-admin"},
	}
	if err := c.Create(ctx, operation); err != nil {
		t.Fatal(err)
	}
	audit, err := NewKubernetesAuditSink(c, c)
	if err != nil {
		t.Fatal(err)
	}
	rebinder, err := NewRebindReconciler(c, c, registrations, gate, testRebindTarget{uid: "node-b-uid", exists: true}, unavailableAuditSink{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	opKey := types.NamespacedName{Namespace: testNamespace, Name: operation.Name}
	result, err := rebinder.Reconcile(ctx, reconcile.Request{NamespacedName: opKey})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("audit outage should schedule a retry: result=%v err=%v", result, err)
	}
	if err := c.Get(ctx, stationKey, station); err != nil {
		t.Fatal(err)
	}
	if station.Spec.Binding.NodeUID != "node-b-uid" || station.Spec.Binding.Version != 2 {
		t.Fatalf("binding not persisted before audit failure: %#v", station.Spec.Binding)
	}
	if err := c.Get(ctx, opKey, operation); err != nil {
		t.Fatal(err)
	}
	if operation.Status.Phase == stationv1alpha1.StationRebindPhaseApplied || operation.Status.PreviousBinding == nil || operation.Status.PreviousBinding.NodeUID != "node-a-uid" {
		t.Fatalf("audit outage incorrectly finished operation: %#v", operation.Status)
	}
	// Reconstruct all in-process reconciliation state before retrying.
	rebinder, err = NewRebindReconciler(c, c, registrations, NewSerializedIdentityOperations(), testRebindTarget{uid: "node-b-uid", exists: true}, audit, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := rebinder.Reconcile(ctx, reconcile.Request{NamespacedName: opKey}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, stationKey, station); err != nil {
		t.Fatal(err)
	}
	if station.Spec.Binding.NodeUID != "node-b-uid" || station.Spec.Binding.Version != 2 {
		t.Fatalf("unexpected rebound Station: %#v", station.Spec.Binding)
	}
	if err := c.Get(ctx, opKey, operation); err != nil {
		t.Fatal(err)
	}
	if operation.Status.Phase != stationv1alpha1.StationRebindPhaseApplied || operation.Status.PreviousBinding == nil || operation.Status.PreviousBinding.NodeUID != "node-a-uid" {
		t.Fatalf("unexpected operation: %#v", operation.Status)
	}
	var events stationv1alpha1.StationAuditEventList
	if err := c.List(ctx, &events, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("audit event count = %d, want 1", len(events.Items))
	}
	station.Spec.Retired = true
	if err := c.Update(ctx, station); err != nil {
		t.Fatalf("retire Station: %v", err)
	}
	station.Spec.Retired = false
	if err := c.Update(ctx, station); !apierrors.IsInvalid(err) {
		t.Fatalf("retirement reversal should be rejected, got %v", err)
	}
}
