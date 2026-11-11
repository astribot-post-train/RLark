package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// SequenceName is the namespace-local StationSequence singleton name.
	SequenceName = "sequence"
	// DefaultOfflineAfter is the heartbeat freshness window in the API contract.
	DefaultOfflineAfter = 90 * time.Second
	sequenceCASAttempts = 8
)

var (
	// ErrInvalidRegistration indicates that a namespace is not mapped to the supplied cluster.
	ErrInvalidRegistration = errors.New("invalid cluster registration")
	// ErrSequenceOwnerConflict prevents allocating IDs from another cluster's sequence.
	ErrSequenceOwnerConflict = errors.New("station sequence belongs to another cluster")
	// ErrStationNameCollision is returned when a deterministic ID is occupied by another binding.
	ErrStationNameCollision = errors.New("deterministic station ID collides with another binding")
)

// ClusterRegistration is the trusted mapping from the agent namespace to a stable cluster ID.
type ClusterRegistration struct {
	ClusterID string
	Namespace string
}

// ClusterResolver returns only trusted server-side registration data.
type ClusterResolver interface {
	ResolveNamespace(context.Context, string) (ClusterRegistration, error)
}

// NodeReadiness reads the RLark Node projection for the registered cluster.
type NodeReadiness interface {
	Ready(context.Context, string, string, string) (bool, error)
}

// StationReconciler turns the latest server-validated observation into durable Station state.
type StationReconciler struct {
	client        client.Client
	reader        client.Reader
	registrations ClusterResolver
	identityOps   IdentityOperationCoordinator
	nodeReadiness NodeReadiness
	offlineAfter  time.Duration
	now           func() time.Time
}

// NewStationReconciler constructs a reconciler with explicit writer and uncached reader dependencies.
func NewStationReconciler(writer client.Client, reader client.Reader, registrations ClusterResolver, identityOps IdentityOperationCoordinator, readiness NodeReadiness, offlineAfter time.Duration, now func() time.Time) (*StationReconciler, error) {
	if writer == nil || reader == nil || registrations == nil || identityOps == nil {
		return nil, errors.New("client, uncached reader, cluster resolver and identity operation coordinator are required")
	}
	if offlineAfter <= 0 {
		return nil, errors.New("offline timeout must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &StationReconciler{
		client: writer, reader: reader, registrations: registrations, identityOps: identityOps,
		nodeReadiness: readiness, offlineAfter: offlineAfter, now: now,
	}, nil
}

// SetupWithManager watches observation changes and uses uncached reads for reconciliation.
func (r *StationReconciler) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("manager is required")
	}
	r.client = mgr.GetClient()
	r.reader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&stationv1alpha1.StationObservation{}).
		Watches(&stationv1alpha1.Station{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
			station, ok := object.(*stationv1alpha1.Station)
			if !ok || station.Spec.Binding.NodeName == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: station.Namespace, Name: station.Spec.Binding.NodeName}}}
		})).
		Complete(r)
}

// Compile-time assertion: StationObservation events are the reconciliation key;
// the handler always rereads the current object and related Stations.
var _ reconcile.Reconciler = (*StationReconciler)(nil)

// Reconcile rereads the current observation, registration and Station set before writing state.
func (r *StationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	release, err := r.identityOps.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("acquire Station identity operation lock: %w", err)
	}
	defer release()

	registration, err := r.registrations.ResolveNamespace(ctx, req.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	if registration.ClusterID == "" || registration.Namespace != req.Namespace {
		return reconcile.Result{}, ErrInvalidRegistration
	}

	var observation stationv1alpha1.StationObservation
	err = r.reader.Get(ctx, req.NamespacedName, &observation)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, r.markNodeOffline(ctx, req.Namespace, req.Name)
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("read station observation: %w", err)
	}
	if observation.Status.NodeUID == "" || observation.Status.ReportedAt.IsZero() {
		return reconcile.Result{}, errors.New("observation is missing server-validated identity or receipt time")
	}

	var stations stationv1alpha1.StationList
	if err := r.reader.List(ctx, &stations, client.InNamespace(req.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("list stations: %w", err)
	}
	byUID, byNodeName := make([]stationv1alpha1.Station, 0, 1), make([]stationv1alpha1.Station, 0, 1)
	var deterministic *stationv1alpha1.Station
	name := StationName(registration.ClusterID, observation.Status.NodeUID)
	for i := range stations.Items {
		station := &stations.Items[i]
		if station.Spec.Binding.NodeUID == observation.Status.NodeUID {
			byUID = append(byUID, *station.DeepCopy())
		}
		if station.Spec.Binding.NodeName == observation.Name {
			byNodeName = append(byNodeName, *station.DeepCopy())
		}
		if station.Name == name {
			deterministic = station.DeepCopy()
		}
	}

	if len(byUID) > 1 || len(byNodeName) > 1 {
		conflicts := uniqueStations(append(byUID, byNodeName...))
		for i := range conflicts {
			if err := r.setConflict(ctx, &conflicts[i], "AmbiguousBinding", "multiple Stations claim this Node identity"); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	if len(byUID) == 1 {
		station := &byUID[0]
		if station.Spec.Binding.NodeName != observation.Name || len(byNodeName) != 1 {
			if err := r.setConflict(ctx, station, "NodeIdentityMismatch", "verified Node identity conflicts with the existing binding"); err != nil {
				return reconcile.Result{}, err
			}
			for i := range byNodeName {
				if byNodeName[i].UID != station.UID {
					if err := r.setConflict(ctx, &byNodeName[i], "NodeIdentityMismatch", "another Station is bound to this Node name"); err != nil {
						return reconcile.Result{}, err
					}
				}
			}
			return reconcile.Result{}, nil
		}
		if station.Spec.Retired {
			return reconcile.Result{}, r.setPhase(ctx, station, stationv1alpha1.StationPhaseRetired)
		}
		return r.observeStation(ctx, registration, station, &observation)
	}
	if len(byNodeName) != 0 {
		for i := range byNodeName {
			if err := r.setConflict(ctx, &byNodeName[i], "NodeUIDChanged", "same Node name reported a different Kubernetes UID; administrator action is required"); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	if deterministic != nil {
		if deterministic.Spec.Binding.NodeUID != observation.Status.NodeUID || deterministic.Spec.Binding.NodeName != observation.Name {
			return reconcile.Result{}, ErrStationNameCollision
		}
		return r.observeStation(ctx, registration, deterministic, &observation)
	}

	return r.createStation(ctx, registration, &observation)
}

func (r *StationReconciler) observeStation(ctx context.Context, registration ClusterRegistration, station *stationv1alpha1.Station, observation *stationv1alpha1.StationObservation) (reconcile.Result, error) {
	now := r.now().UTC()
	age := now.Sub(observation.Status.ReportedAt.Time)
	fresh := age >= 0 && age < r.offlineAfter
	want := station.Status.DeepCopy()
	if fresh {
		want.Phase = stationv1alpha1.StationPhaseBound
	} else {
		want.Phase = stationv1alpha1.StationPhaseOffline
	}
	want.LastSeenTime = observation.Status.ReportedAt.DeepCopy()
	if observation.Status.BootTime != nil {
		want.BootTime = observation.Status.BootTime.DeepCopy()
	}
	observedStatus, observedReason, observedMessage := metav1.ConditionFalse, "HeartbeatExpired", "last authenticated observation is outside the freshness window"
	if fresh {
		observedStatus, observedReason, observedMessage = metav1.ConditionTrue, "HeartbeatFresh", "a fresh authenticated observation is available"
	}
	setCondition(want, now, "Observed", observedStatus, observedReason, observedMessage)
	setCondition(want, now, "IdentityMatched", metav1.ConditionTrue, "NodeUIDMatched", "observation Node UID matches the existing binding")
	if len(observation.Status.Probes) == 0 {
		setCondition(want, now, "NetworkVerified", metav1.ConditionUnknown, "NoProbeResults", "no network probes were reported")
	} else {
		allOK := true
		for _, probe := range observation.Status.Probes {
			allOK = allOK && probe.OK
		}
		if allOK {
			setCondition(want, now, "NetworkVerified", metav1.ConditionTrue, "AllProbesSucceeded", "all reported network probes succeeded")
		} else {
			setCondition(want, now, "NetworkVerified", metav1.ConditionFalse, "ProbeFailed", "one or more reported network probes failed")
		}
	}
	if r.nodeReadiness == nil {
		setCondition(want, now, "RLarkLinked", metav1.ConditionUnknown, "NodeReaderUnavailable", "RLark Node readiness is not configured")
	} else {
		ready, err := r.nodeReadiness.Ready(ctx, registration.ClusterID, registration.Namespace, observation.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("read RLark Node readiness: %w", err)
		}
		if ready {
			setCondition(want, now, "RLarkLinked", metav1.ConditionTrue, "NodeReady", "the corresponding RLark Node is Ready")
		} else {
			setCondition(want, now, "RLarkLinked", metav1.ConditionFalse, "NodeNotReady", "the corresponding RLark Node is missing or not Ready")
		}
	}
	if err := r.patchStationStatus(ctx, station, *want); err != nil {
		return reconcile.Result{}, err
	}
	if fresh {
		return reconcile.Result{RequeueAfter: r.offlineAfter - age}, nil
	}
	return reconcile.Result{}, nil
}

func (r *StationReconciler) createStation(ctx context.Context, registration ClusterRegistration, observation *stationv1alpha1.StationObservation) (reconcile.Result, error) {
	sequence, err := r.nextSequence(ctx, registration)
	if err != nil {
		return reconcile.Result{}, err
	}
	station := &stationv1alpha1.Station{
		ObjectMeta: metav1.ObjectMeta{Name: StationName(registration.ClusterID, observation.Status.NodeUID), Namespace: registration.Namespace},
		Spec: stationv1alpha1.StationSpec{
			DisplayName: fmt.Sprintf("Station-%04d", sequence),
			Sequence:    sequence,
			Binding: stationv1alpha1.StationBinding{
				NodeName: observation.Name, NodeUID: observation.Status.NodeUID,
				SystemUUID: observation.Status.SystemUUID, Version: 1, BoundBy: "system",
			},
		},
	}
	err = r.client.Create(ctx, station)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return reconcile.Result{}, fmt.Errorf("create Station: %w", err)
	}
	if apierrors.IsAlreadyExists(err) {
		if getErr := r.reader.Get(ctx, types.NamespacedName{Namespace: registration.Namespace, Name: station.Name}, station); getErr != nil {
			return reconcile.Result{}, fmt.Errorf("read Station after create race: %w", getErr)
		}
		if station.Spec.Binding.NodeUID != observation.Status.NodeUID || station.Spec.Binding.NodeName != observation.Name {
			return reconcile.Result{}, ErrStationNameCollision
		}
	}
	return r.observeStation(ctx, registration, station, observation)
}

func (r *StationReconciler) nextSequence(ctx context.Context, registration ClusterRegistration) (int64, error) {
	key := types.NamespacedName{Namespace: registration.Namespace, Name: SequenceName}
	for attempt := 0; attempt < sequenceCASAttempts; attempt++ {
		var sequence stationv1alpha1.StationSequence
		err := r.reader.Get(ctx, key, &sequence)
		if apierrors.IsNotFound(err) {
			sequence = stationv1alpha1.StationSequence{ObjectMeta: metav1.ObjectMeta{Namespace: registration.Namespace, Name: SequenceName}, Spec: stationv1alpha1.StationSequenceSpec{ClusterID: registration.ClusterID}}
			if createErr := r.client.Create(ctx, &sequence); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
				return 0, fmt.Errorf("create StationSequence: %w", createErr)
			}
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read StationSequence: %w", err)
		}
		if sequence.Spec.ClusterID != registration.ClusterID {
			return 0, ErrSequenceOwnerConflict
		}
		base := sequence.DeepCopy()
		sequence.Status.LastAssigned++
		if err := r.client.Status().Patch(ctx, &sequence, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return 0, fmt.Errorf("reserve Station sequence: %w", err)
		}
		return sequence.Status.LastAssigned, nil
	}
	return 0, errors.New("Station sequence remained contended after bounded retries")
}

func (r *StationReconciler) markNodeOffline(ctx context.Context, namespace, nodeName string) error {
	var stations stationv1alpha1.StationList
	if err := r.reader.List(ctx, &stations, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Stations for missing observation: %w", err)
	}
	bound := make([]*stationv1alpha1.Station, 0, 1)
	for i := range stations.Items {
		station := &stations.Items[i]
		if station.Spec.Binding.NodeName == nodeName {
			bound = append(bound, station)
		}
	}
	if len(bound) > 1 {
		for _, station := range bound {
			if err := r.setConflict(ctx, station, "DuplicateNodeNameBinding", "multiple Stations are bound to this Node name"); err != nil {
				return err
			}
		}
		return nil
	}
	for _, station := range bound {
		if station.Spec.Retired {
			continue
		}
		want := station.Status.DeepCopy()
		want.Phase = stationv1alpha1.StationPhaseOffline
		now := r.now().UTC()
		setCondition(want, now, "Observed", metav1.ConditionFalse, "ObservationMissing", "no current observation exists for this Node name")
		setCondition(want, now, "NetworkVerified", metav1.ConditionUnknown, "NoObservation", "network state is unknown because the observation is missing")
		if err := r.patchStationStatus(ctx, station, *want); err != nil {
			return err
		}
	}
	return nil
}

func (r *StationReconciler) setConflict(ctx context.Context, station *stationv1alpha1.Station, reason, message string) error {
	want := station.Status.DeepCopy()
	want.Phase = stationv1alpha1.StationPhaseConflict
	setCondition(want, r.now().UTC(), "IdentityMatched", metav1.ConditionFalse, reason, message)
	return r.patchStationStatus(ctx, station, *want)
}

func (r *StationReconciler) setPhase(ctx context.Context, station *stationv1alpha1.Station, phase stationv1alpha1.StationPhase) error {
	want := station.Status.DeepCopy()
	want.Phase = phase
	return r.patchStationStatus(ctx, station, *want)
}

func (r *StationReconciler) patchStationStatus(ctx context.Context, original *stationv1alpha1.Station, desired stationv1alpha1.StationStatus) error {
	key := types.NamespacedName{Namespace: original.Namespace, Name: original.Name}
	for attempt := 0; attempt < sequenceCASAttempts; attempt++ {
		var current stationv1alpha1.Station
		if err := r.reader.Get(ctx, key, &current); err != nil {
			return fmt.Errorf("read Station status target: %w", err)
		}
		if reflect.DeepEqual(current.Status, desired) {
			return nil
		}
		base := current.DeepCopy()
		current.Status = *desired.DeepCopy()
		if err := r.client.Status().Patch(ctx, &current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("patch Station status: %w", err)
		}
		return nil
	}
	return errors.New("Station status remained contended after bounded retries")
}

// StationName derives an idempotent business ID from trusted cluster and Node identities.
func StationName(clusterID, nodeUID string) string {
	digest := sha256.Sum256([]byte(clusterID + "\x00" + nodeUID))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:16])
	return "stn-" + strings.ToLower(encoded)
}

func setCondition(status *stationv1alpha1.StationStatus, now time.Time, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, Reason: reason,
		Message: message, LastTransitionTime: metav1.NewTime(now),
	})
}

func uniqueStations(stations []stationv1alpha1.Station) []stationv1alpha1.Station {
	seen := make(map[types.NamespacedName]struct{}, len(stations))
	result := make([]stationv1alpha1.Station, 0, len(stations))
	for i := range stations {
		key := types.NamespacedName{Namespace: stations[i].Namespace, Name: stations[i].Name}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, stations[i])
	}
	return result
}
