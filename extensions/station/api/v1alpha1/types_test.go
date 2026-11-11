package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToSchemeRegistersResourcesAndLists(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{
		"Station", "StationList",
		"StationObservation", "StationObservationList",
		"StationSequence", "StationSequenceList",
		"StationRebind", "StationRebindList",
	} {
		gvk := GroupVersion.WithKind(kind)
		if _, err := scheme.New(gvk); err != nil {
			t.Errorf("scheme does not register %s: %v", gvk, err)
		}
	}
}

func TestObservationDeepCopyIsIndependent(t *testing.T) {
	rtt := int64(25)
	observedAt := metav1.NewTime(time.Unix(10, 0).UTC())
	station := &StationObservation{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"source": "agent"}},
		Status: StationObservationStatus{
			ReportedAt: observedAt,
			NodeUID:    "node-uid",
			Roles:      []string{"worker"},
			Wireguard: &WireguardObservation{
				Interface: "st-wg0",
				Addresses: []string{"10.254.240.7/32"},
				Peers:     []WireguardPeerObservation{{Name: "hub", LatestHandshake: &observedAt}},
			},
			Probes: []StationProbe{{Target: "nas:445", OK: true, RTTMillis: &rtt}},
		},
	}
	copy := station.DeepCopy()
	copy.Labels["source"] = "changed"
	copy.Status.Roles[0] = "gateway"
	copy.Status.Wireguard.Addresses[0] = "192.0.2.1/32"
	copy.Status.Wireguard.Peers[0].Name = "changed"
	*copy.Status.Probes[0].RTTMillis = 999
	if station.Labels["source"] != "agent" || station.Status.Roles[0] != "worker" || station.Status.Wireguard.Addresses[0] != "10.254.240.7/32" || station.Status.Wireguard.Peers[0].Name != "hub" || *station.Status.Probes[0].RTTMillis != 25 {
		t.Fatal("deep copy shared mutable observation data")
	}
}

func TestRebindRequestRoundTripsAsStableOperation(t *testing.T) {
	request := StationRebind{
		ObjectMeta: metav1.ObjectMeta{Name: "rebind-req-123", Namespace: "agent-space"},
		Spec: StationRebindSpec{
			RequestID:              "req-123",
			StationRef:             StationReference{Name: "stn-a1b2c3", UID: "station-object-uid"},
			ExpectedBindingVersion: 4,
			TargetNodeName:         "node-b",
			TargetNodeUID:          "node-b-uid",
			Reason:                 "node was reinstalled",
			RequestedBy:            "user-42",
		},
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StationRebind
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Spec.RequestID != request.Spec.RequestID || decoded.Spec.ExpectedBindingVersion != 4 || decoded.Spec.StationRef.UID != "station-object-uid" {
		t.Fatalf("rebind identity was not preserved: %#v", decoded.Spec)
	}
	if decoded.DeepCopy().Spec.Reason != request.Spec.Reason {
		t.Fatal("deep copy lost immutable operation request")
	}
}
