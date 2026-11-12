package stationidentity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type observationNamespaceMap map[string]string

func (m observationNamespaceMap) ResolveObservationNamespace(_ context.Context, clusterID string) (string, error) {
	namespace := m[clusterID]
	if namespace == "" {
		return "", ErrClusterNotRegistered
	}
	return namespace, nil
}

func testDynamicClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationObservationGVR: "StationObservationList",
		stationGVR:            "StationList",
		rlarkClusterGVR:       "ClusterList",
	}, objects...)
}

func TestKubernetesObservationSinkCreatesAndRefreshesOwnedSnapshot(t *testing.T) {
	client := testDynamicClient()
	emptyBeforeStatusPatch := false
	client.PrependReactor("patch", stationObservationResource, func(ktesting.Action) (bool, runtime.Object, error) {
		obj, err := client.Tracker().Get(stationObservationGVR, "agent-a", "node-a")
		if err != nil {
			return true, nil, err
		}
		unstructuredObject := obj.(*unstructured.Unstructured)
		_, found, _ := unstructured.NestedMap(unstructuredObject.Object, "status")
		if !found {
			emptyBeforeStatusPatch = true
		}
		return false, nil, nil
	})
	receivedAt := time.Date(2026, 11, 11, 1, 2, 3, 4, time.FixedZone("test", 8*60*60))
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"cluster-a": "agent-a"}, Now: func() time.Time { return receivedAt }}
	evidence := validEvidence()
	evidence.ClusterID = "cluster-a"
	evidence.RegisteredClusterID = "cluster-a"
	evidence.NodeName = "node-a"
	evidence.PodNodeName = "node-a"
	evidence.StationUID = "station-uid-a"
	evidence.RegisteredStationUID = "station-uid-a"
	observation := Observation{
		SystemUUID: "system-a",
		BootTime:   &receivedAt,
		Roles:      []string{"worker"},
		Wireguard:  json.RawMessage(`{"interface":"st-wg0","addresses":["10.254.240.10/32"]}`),
		Probes:     []Probe{{Target: "nas:445", OK: true}},
	}
	if err := sink.Store(context.Background(), evidence, observation); err != nil {
		t.Fatal(err)
	}
	resource := client.Resource(stationObservationGVR).Namespace("agent-a")
	created, err := resource.Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := created.GetAnnotations()[observationClusterIDKey]; got != "cluster-a" {
		t.Fatalf("cluster ownership annotation = %q", got)
	}
	if got := created.GetAnnotations()[observationStationUIDKey]; got != "station-uid-a" {
		t.Fatalf("Station ownership annotation = %q", got)
	}
	if err := sink.Store(context.Background(), evidence, observation); err != nil {
		t.Fatal(err)
	}
	if !emptyBeforeStatusPatch {
		t.Fatal("new observation was not created empty before status write")
	}
	stored, err := resource.Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reportedAt, _, _ := unstructured.NestedString(stored.Object, "status", "reportedAt")
	if reportedAt != receivedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("reportedAt = %q", reportedAt)
	}
	if got, _, _ := unstructured.NestedString(stored.Object, "status", "nodeUID"); got != evidence.NodeUID {
		t.Fatalf("nodeUID = %q", got)
	}
	if got, _, _ := unstructured.NestedString(stored.Object, "status", "wireguard", "interface"); got != "st-wg0" {
		t.Fatalf("wireguard interface = %q", got)
	}
	if err := sink.Store(context.Background(), evidence, Observation{}); err != nil {
		t.Fatal(err)
	}
	stored, err = resource.Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "status", "wireguard"); found {
		t.Fatal("empty latest snapshot retained old WireGuard data")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "status", "bootTime"); found {
		t.Fatal("empty latest snapshot retained old boot time")
	}
}

func TestObservationBindingVersionReplacesPreviousSnapshot(t *testing.T) {
	client := testDynamicClient()
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"cluster-a": "agent-a"}}
	e := validEvidence()
	e.ClusterID, e.RegisteredClusterID = "cluster-a", "cluster-a"
	e.NodeName, e.PodNodeName = "node-a", "node-a"
	e.StationUID, e.RegisteredStationUID = "station-a", "station-a"
	wg := Observation{Wireguard: json.RawMessage(`{"interface":"st-wg0","addresses":["10.254.240.1/32"]}`)}
	if err := sink.Store(context.Background(), e, wg); err != nil {
		t.Fatal(err)
	}
	e.Generation++
	e.RegisteredGeneration = e.Generation
	if err := sink.Store(context.Background(), e, Observation{}); err != nil {
		t.Fatal(err)
	}
	got, err := client.Resource(stationObservationGVR).Namespace("agent-a").Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetAnnotations()[observationBindingVersionKey] != "2" {
		t.Fatalf("binding version = %q", got.GetAnnotations()[observationBindingVersionKey])
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(got.Object, "status", "wireguard"); found {
		t.Fatal("old WireGuard observation survived new binding")
	}
}

func TestKubernetesObservationSinkRejectsForeignOwnershipAndUnverifiedEvidence(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "StationObservation",
		"metadata": map[string]interface{}{
			"name": "node", "namespace": "agent",
			"annotations": map[string]interface{}{observationClusterIDKey: "other", observationStationUIDKey: "station"},
		},
		"spec": map[string]interface{}{},
	}}
	client := testDynamicClient(foreign)
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"c": "agent"}}
	evidence := validEvidence()
	if err := sink.Store(context.Background(), evidence, Observation{}); !errors.Is(err, ErrObservationOwnershipConflict) {
		t.Fatalf("foreign observation ownership error = %v", err)
	}
	evidence.TokenPodUID = "replaced"
	if err := sink.Store(context.Background(), evidence, Observation{}); err == nil {
		t.Fatal("unverified evidence was accepted")
	}
}

func TestKubernetesObservationSinkSupportsUnboundBootstrapAndBindsOnce(t *testing.T) {
	client := testDynamicClient()
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"c": "agent"}}
	evidence := validEvidence()
	evidence.StationUID = ""
	evidence.RegisteredStationUID = ""
	evidence.Generation = 0
	evidence.RegisteredGeneration = 0
	if err := sink.Store(context.Background(), evidence, Observation{}); err != nil {
		t.Fatalf("bootstrap observation rejected: %v", err)
	}
	resource := client.Resource(stationObservationGVR).Namespace("agent")
	current, err := resource.Get(context.Background(), "node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.GetAnnotations()[observationNodeUIDKey] != evidence.NodeUID || current.GetAnnotations()[observationStationUIDKey] != "" {
		t.Fatalf("bootstrap ownership annotations = %#v", current.GetAnnotations())
	}
	evidence.StationUID = "station-object-uid"
	evidence.RegisteredStationUID = evidence.StationUID
	evidence.Generation = 1
	evidence.RegisteredGeneration = 1
	if err := sink.Store(context.Background(), evidence, Observation{}); err != nil {
		t.Fatalf("Station binding did not adopt its verified Observation: %v", err)
	}
	current, err = resource.Get(context.Background(), "node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.GetAnnotations()[observationStationUIDKey] != evidence.StationUID {
		t.Fatalf("Station ownership annotation = %#v", current.GetAnnotations())
	}
	evidence.StationUID = "replacement-station-uid"
	evidence.RegisteredStationUID = evidence.StationUID
	if err := sink.Store(context.Background(), evidence, Observation{}); !errors.Is(err, ErrObservationOwnershipConflict) {
		t.Fatalf("replaced Station identity error = %v", err)
	}
}

func TestKubernetesObservationSinkRetriesCreateAndStatusConflicts(t *testing.T) {
	client := testDynamicClient()
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"c": "agent"}}
	evidence := validEvidence()
	created := false
	client.PrependReactor("create", stationObservationResource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if created {
			return false, nil, nil
		}
		created = true
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if err := client.Tracker().Create(stationObservationGVR, obj, "agent"); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: stationObservationGroup, Resource: stationObservationResource}, "node")
	})
	conflicted := false
	client.PrependReactor("patch", stationObservationResource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: stationObservationGroup, Resource: stationObservationResource}, "node", errors.New("test conflict"))
	})
	if err := sink.Store(context.Background(), evidence, Observation{Roles: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	if !created || !conflicted {
		t.Fatalf("create race=%t patch conflict=%t", created, conflicted)
	}
	stored, err := client.Resource(stationObservationGVR).Namespace("agent").Get(context.Background(), "node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	roles, found, err := unstructured.NestedStringSlice(stored.Object, "status", "roles")
	if err != nil || !found || len(roles) != 1 || roles[0] != "worker" {
		t.Fatalf("status after retry roles=%v found=%t err=%v", roles, found, err)
	}
}

func TestKubernetesClusterNamespaceResolverRequiresUniqueAuthoritativeScope(t *testing.T) {
	cluster := func(name, namespace string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "rlinf.io/v1alpha1", "kind": "Cluster",
			"metadata": map[string]interface{}{"name": name},
			"spec":     map[string]interface{}{"agentNamespace": namespace},
		}}
	}
	t.Run("resolves by cluster object name", func(t *testing.T) {
		resolver := KubernetesClusterNamespaceResolver{Client: testDynamicClient(cluster("cluster-a", "agent-a"))}
		namespace, err := resolver.ResolveObservationNamespace(context.Background(), "cluster-a")
		if err != nil || namespace != "agent-a" {
			t.Fatalf("namespace=%q err=%v", namespace, err)
		}
	})
	t.Run("rejects duplicate namespace claim", func(t *testing.T) {
		resolver := KubernetesClusterNamespaceResolver{Client: testDynamicClient(cluster("cluster-a", "agent-a"), cluster("cluster-b", "agent-a"))}
		if _, err := resolver.ResolveObservationNamespace(context.Background(), "cluster-a"); err == nil {
			t.Fatal("accepted duplicate authoritative namespace")
		}
	})
	t.Run("rejects unknown cluster", func(t *testing.T) {
		resolver := KubernetesClusterNamespaceResolver{Client: testDynamicClient(cluster("cluster-a", "agent-a"))}
		if _, err := resolver.ResolveObservationNamespace(context.Background(), "missing"); !errors.Is(err, ErrClusterNotRegistered) {
			t.Fatalf("unknown cluster error=%v", err)
		}
	})
}

func TestKubernetesObservationSinkPreservesExistingObjectUID(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "StationObservation",
		"metadata": map[string]interface{}{
			"name": "node", "namespace": "agent", "uid": "object-uid", "resourceVersion": "1",
			"annotations": map[string]interface{}{observationClusterIDKey: "c", observationNodeUIDKey: "n", observationStationUIDKey: "s"},
		},
		"spec": map[string]interface{}{},
	}}
	client := testDynamicClient(existing)
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"c": "agent"}}
	if err := sink.Store(context.Background(), validEvidence(), Observation{}); err != nil {
		t.Fatal(err)
	}
	stored, err := client.Resource(stationObservationGVR).Namespace("agent").Get(context.Background(), "node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if types.UID(stored.GetUID()) != "object-uid" {
		t.Fatalf("observation UID changed to %q", stored.GetUID())
	}
}

// A machine reinstalled under the same name has a new Node UID. It must be
// able to report, or the Station never shows the conflict an administrator
// rebinds; it gets a fresh snapshot and nothing of the previous Node's.
func TestReinstalledNodeReplacesThePreviousNodesSnapshot(t *testing.T) {
	client := testDynamicClient()
	sink := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"c": "agent"}}
	old := validEvidence()
	boot := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := sink.Store(context.Background(), old, Observation{BootTime: &boot, Wireguard: json.RawMessage(`{"interface":"st-wg0","addresses":["10.254.240.5/32"]}`)}); err != nil {
		t.Fatal(err)
	}
	rebuilt := validEvidence()
	rebuilt.NodeUID, rebuilt.RegisteredNodeUID = "n-rebuilt", "n-rebuilt"
	rebuilt.StationUID, rebuilt.RegisteredStationUID, rebuilt.Generation, rebuilt.RegisteredGeneration = "", "", 0, 0
	if err := sink.Store(context.Background(), rebuilt, Observation{}); err != nil {
		t.Fatalf("reinstalled node could not report: %v", err)
	}
	stored, err := client.Resource(stationObservationGVR).Namespace("agent").Get(context.Background(), "node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	annotations := stored.GetAnnotations()
	uid, _, _ := unstructured.NestedString(stored.Object, "status", "nodeUID")
	_, hasTunnel, _ := unstructured.NestedMap(stored.Object, "status", "wireguard")
	if uid != "n-rebuilt" || annotations[observationNodeUIDKey] != "n-rebuilt" || annotations[observationStationUIDKey] != "" || hasTunnel {
		t.Fatalf("snapshot still carries the previous Node: %v %v", annotations, stored.Object["status"])
	}
	// Another cluster's node of the same name still cannot take the object.
	foreign := validEvidence()
	foreign.ClusterID, foreign.RegisteredClusterID = "d", "d"
	other := &KubernetesObservationSink{Client: client, Namespaces: observationNamespaceMap{"d": "agent"}}
	if err := other.Store(context.Background(), foreign, Observation{}); !errors.Is(err, ErrObservationOwnershipConflict) {
		t.Fatalf("foreign cluster error = %v", err)
	}
}
