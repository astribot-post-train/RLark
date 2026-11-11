package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testNamespace = "rlark-agent-test"

type testRegistrations map[string]ClusterRegistration

func (m testRegistrations) ResolveNamespace(_ context.Context, namespace string) (ClusterRegistration, error) {
	registration, ok := m[namespace]
	if !ok {
		return ClusterRegistration{}, fmt.Errorf("unregistered namespace %q", namespace)
	}
	return registration, nil
}

type testNodeReadiness struct {
	ready bool
	err   error
}

func (r testNodeReadiness) Ready(context.Context, string, string, string) (bool, error) {
	return r.ready, r.err
}

func newTestReconciler(t *testing.T, now *time.Time, objects ...client.Object) (*StationReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := stationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&stationv1alpha1.Station{}, &stationv1alpha1.StationObservation{}, &stationv1alpha1.StationSequence{}, &stationv1alpha1.StationRebind{}).
		WithObjects(objects...).Build()
	reconciler, err := NewStationReconciler(c, c, testRegistrations{
		testNamespace: {ClusterID: "cluster-a", Namespace: testNamespace},
	}, NewSerializedIdentityOperations(), testNodeReadiness{ready: true}, DefaultOfflineAfter, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, c
}

func testObservation(name, nodeUID string, at time.Time) *stationv1alpha1.StationObservation {
	return &stationv1alpha1.StationObservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       stationv1alpha1.StationObservationSpec{},
		Status: stationv1alpha1.StationObservationStatus{
			ReportedAt: metav1.NewTime(at),
			NodeUID:    nodeUID,
			SystemUUID: "system-a",
			BootTime:   &metav1.Time{Time: at.Add(-time.Hour)},
			Probes:     []stationv1alpha1.StationProbe{{Target: "nas:445", OK: true}},
		},
	}
}

func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

func getStation(t *testing.T, c client.Client, name string) *stationv1alpha1.Station {
	t.Helper()
	station := &stationv1alpha1.Station{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, station); err != nil {
		t.Fatal(err)
	}
	return station
}

func TestReconcileCreatesDeterministicStationAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	reconciler, c := newTestReconciler(t, &now, testObservation("node-a", "node-uid-a", now))
	result, err := reconciler.Reconcile(context.Background(), requestFor("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	name := StationName("cluster-a", "node-uid-a")
	station := getStation(t, c, name)
	if station.Spec.Sequence != 1 || station.Spec.Binding.Version != 1 || station.Status.Phase != stationv1alpha1.StationPhaseBound {
		t.Fatalf("unexpected Station after first observation: %#v", station)
	}
	if result.RequeueAfter != DefaultOfflineAfter {
		t.Fatalf("fresh observation requeue=%s, want %s", result.RequeueAfter, DefaultOfflineAfter)
	}
	if condition := findCondition(station, "Observed"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatal("fresh observation did not set Observed=True")
	}
	if condition := findCondition(station, "RLarkLinked"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatal("ready RLark Node did not set RLarkLinked=True")
	}

	_, err = reconciler.Reconcile(context.Background(), requestFor("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 || stations.Items[0].Name != name {
		t.Fatalf("duplicate notification created Stations: %#v", stations.Items)
	}
	sequence := &stationv1alpha1.StationSequence{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: SequenceName}, sequence); err != nil {
		t.Fatal(err)
	}
	if sequence.Status.LastAssigned != 1 {
		t.Fatalf("idempotent reconcile advanced sequence to %d", sequence.Status.LastAssigned)
	}
}

func TestConcurrentFirstObservationsCreateOneStableStation(t *testing.T) {
	now := time.Now().UTC()
	reconciler, c := newTestReconciler(t, &now, testObservation("node-a", "node-uid-a", now))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.Reconcile(context.Background(), requestFor("node-a"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reconcile failed: %v", err)
		}
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 || stations.Items[0].Name != StationName("cluster-a", "node-uid-a") {
		t.Fatalf("concurrent reconcile created duplicate or unstable Station IDs: %#v", stations.Items)
	}
}

func TestChangedNodeUIDConflictsWithoutAutoAdoption(t *testing.T) {
	now := time.Now().UTC()
	old := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: "stn-previous", Namespace: testNamespace},
		Spec:       stationv1alpha1.StationSpec{DisplayName: "Station-0001", Sequence: 1, Binding: stationv1alpha1.StationBinding{NodeName: "node-a", NodeUID: "old-uid", Version: 1, BoundBy: "system"}},
		Status:     stationv1alpha1.StationStatus{Phase: stationv1alpha1.StationPhaseBound},
	}
	reconciler, c := newTestReconciler(t, &now, old, testObservation("node-a", "new-uid", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	station := getStation(t, c, old.Name)
	if station.Spec.Binding.NodeUID != "old-uid" || station.Status.Phase != stationv1alpha1.StationPhaseConflict {
		t.Fatalf("changed Node UID was adopted: spec=%#v status=%#v", station.Spec, station.Status)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 {
		t.Fatalf("replacement Station was automatically created: %d Stations", len(stations.Items))
	}
}

func TestDuplicateNodeBindingsBecomeConflict(t *testing.T) {
	now := time.Now().UTC()
	first := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: "stn-first", Namespace: testNamespace, UID: "first-uid"},
		Spec:       stationv1alpha1.StationSpec{DisplayName: "First", Sequence: 1, Binding: stationv1alpha1.StationBinding{NodeName: "node-a", NodeUID: "old-a", Version: 1, BoundBy: "system"}},
	}
	second := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: "stn-second", Namespace: testNamespace, UID: "second-uid"},
		Spec:       stationv1alpha1.StationSpec{DisplayName: "Second", Sequence: 2, Binding: stationv1alpha1.StationBinding{NodeName: "node-a", NodeUID: "old-b", Version: 1, BoundBy: "system"}},
	}
	reconciler, c := newTestReconciler(t, &now, first, second, testObservation("node-a", "new-uid", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stn-first", "stn-second"} {
		if got := getStation(t, c, name).Status.Phase; got != stationv1alpha1.StationPhaseConflict {
			t.Fatalf("Station %s phase=%s, want Conflict", name, got)
		}
	}
}

func TestDeterministicNameCollisionFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	name := StationName("cluster-a", "node-uid-a")
	existing := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       stationv1alpha1.StationSpec{DisplayName: "Unrelated", Sequence: 9, Binding: stationv1alpha1.StationBinding{NodeName: "node-z", NodeUID: "node-uid-z", Version: 1, BoundBy: "system"}},
	}
	reconciler, c := newTestReconciler(t, &now, existing, testObservation("node-a", "node-uid-a", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); !errors.Is(err, ErrStationNameCollision) {
		t.Fatalf("collision error=%v, want ErrStationNameCollision", err)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 || stations.Items[0].Spec.Binding.NodeUID != "node-uid-z" {
		t.Fatalf("collision modified or adopted foreign Station: %#v", stations.Items)
	}
}

func TestHeartbeatExpiresAndRecoversFromCurrentObservation(t *testing.T) {
	now := time.Now().UTC()
	observation := testObservation("node-a", "node-uid-a", now)
	reconciler, c := newTestReconciler(t, &now, observation)
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	name := StationName("cluster-a", "node-uid-a")
	now = now.Add(DefaultOfflineAfter)
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	if got := getStation(t, c, name).Status.Phase; got != stationv1alpha1.StationPhaseOffline {
		t.Fatalf("stale observation phase=%s, want Offline", got)
	}
	var current stationv1alpha1.StationObservation
	key := types.NamespacedName{Namespace: testNamespace, Name: "node-a"}
	if err := c.Get(context.Background(), key, &current); err != nil {
		t.Fatal(err)
	}
	current.Status.ReportedAt = metav1.NewTime(now)
	if err := c.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	if got := getStation(t, c, name).Status.Phase; got != stationv1alpha1.StationPhaseBound {
		t.Fatalf("fresh observation failed to recover Station, phase=%s", got)
	}
}

func TestMissingObservationMarksStationOfflineButPreservesBinding(t *testing.T) {
	now := time.Now().UTC()
	reconciler, c := newTestReconciler(t, &now, testObservation("node-a", "node-uid-a", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &stationv1alpha1.StationObservation{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Namespace: testNamespace}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	station := getStation(t, c, StationName("cluster-a", "node-uid-a"))
	if station.Status.Phase != stationv1alpha1.StationPhaseOffline || station.Spec.Binding.NodeUID != "node-uid-a" {
		t.Fatalf("missing observation changed binding or failed to mark offline: %#v", station)
	}
}

func TestNodeReadinessFailureRecoversWithoutAllocatingAnotherStation(t *testing.T) {
	now := time.Now().UTC()
	reconciler, c := newTestReconciler(t, &now, testObservation("node-a", "node-uid-a", now))
	reconciler.nodeReadiness = testNodeReadiness{err: errors.New("temporary read failure")}
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err == nil {
		t.Fatal("readiness dependency failure was ignored")
	}
	name := StationName("cluster-a", "node-uid-a")
	station := &stationv1alpha1.Station{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, station); err != nil {
		t.Fatalf("expected idempotently created Station before status retry: %v", err)
	}
	reconciler.nodeReadiness = testNodeReadiness{ready: true}
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 || stations.Items[0].Status.Phase != stationv1alpha1.StationPhaseBound {
		t.Fatalf("recovery created duplicate or failed to set status: %#v", stations.Items)
	}
	sequence := &stationv1alpha1.StationSequence{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: SequenceName}, sequence); err != nil {
		t.Fatal(err)
	}
	if sequence.Status.LastAssigned != 1 {
		t.Fatalf("retry allocated another sequence: %d", sequence.Status.LastAssigned)
	}
}

func TestRetiredStationIsNotReactivated(t *testing.T) {
	now := time.Now().UTC()
	retired := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: "stn-retired", Namespace: testNamespace},
		Spec:       stationv1alpha1.StationSpec{DisplayName: "Retired", Sequence: 1, Retired: true, Binding: stationv1alpha1.StationBinding{NodeName: "node-a", NodeUID: "node-uid-a", Version: 1, BoundBy: "system"}},
		Status:     stationv1alpha1.StationStatus{Phase: stationv1alpha1.StationPhaseRetired},
	}
	reconciler, c := newTestReconciler(t, &now, retired, testObservation("node-a", "node-uid-a", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); err != nil {
		t.Fatal(err)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 1 || stations.Items[0].Status.Phase != stationv1alpha1.StationPhaseRetired {
		t.Fatalf("heartbeat reactivated or replaced retired Station: %#v", stations.Items)
	}
}

func TestSequenceOwnerMismatchFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	sequence := &stationv1alpha1.StationSequence{
		ObjectMeta: metav1.ObjectMeta{Name: SequenceName, Namespace: testNamespace},
		Spec:       stationv1alpha1.StationSequenceSpec{ClusterID: "other-cluster"},
	}
	reconciler, c := newTestReconciler(t, &now, sequence, testObservation("node-a", "node-uid-a", now))
	if _, err := reconciler.Reconcile(context.Background(), requestFor("node-a")); !errors.Is(err, ErrSequenceOwnerConflict) {
		t.Fatalf("sequence ownership error=%v, want ErrSequenceOwnerConflict", err)
	}
	var stations stationv1alpha1.StationList
	if err := c.List(context.Background(), &stations, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(stations.Items) != 0 {
		t.Fatalf("created Station despite sequence ownership conflict: %d", len(stations.Items))
	}
}

func findCondition(station *stationv1alpha1.Station, conditionType string) *metav1.Condition {
	return apimeta.FindStatusCondition(station.Status.Conditions, conditionType)
}
