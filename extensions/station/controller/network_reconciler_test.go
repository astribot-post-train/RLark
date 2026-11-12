package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	"github.com/rlinf/rlark/extensions/station/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

var testNetworkKey = types.NamespacedName{Namespace: "station-system", Name: "default"}

func testSiteNetwork() *stationv1alpha1.SiteNetwork {
	return &stationv1alpha1.SiteNetwork{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNetworkKey.Namespace, Name: testNetworkKey.Name, Generation: 1},
		Spec: stationv1alpha1.SiteNetworkSpec{
			Mode: stationv1alpha1.SiteNetworkModeTunnel, Pool: "10.254.240.0/24", MTU: 1380, Interface: "st-wg0", ListenPort: 51820, KeepaliveSeconds: 25,
			Sites: []stationv1alpha1.SiteNetworkSite{
				{Name: "office", Clusters: []string{"dev"}, Mesh: true},
				{Name: "lab", Clusters: []string{"robot-01", "robot-02"}, Reaches: []string{"office"}},
			},
			Nodes: []stationv1alpha1.SiteNetworkNode{
				{ClusterID: "dev", NodeName: "cailiang", Role: "Hub", Relay: true, Address: "10.254.240.1",
					Endpoints: []stationv1alpha1.SiteNetworkEndpoint{{Address: "10.11.14.128:51820", Scope: "Site"}}},
				{ClusterID: "dev", NodeName: "alexfu", Role: "Hub", Endpoints: []stationv1alpha1.SiteNetworkEndpoint{{Scope: "Site"}}},
			},
		},
	}
}

// testMember returns the Station and the NetworkEnrollment of one node.
func testMember(cluster, node, key string, addresses ...string) []client.Object {
	uid := types.UID("station-" + node)
	return []client.Object{
		&stationv1alpha1.Station{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-" + cluster, Name: "stn-" + node, UID: uid},
			Spec: stationv1alpha1.StationSpec{DisplayName: node, Sequence: 1, Binding: stationv1alpha1.StationBinding{
				NodeName: node, NodeUID: "node-" + node, Version: 1, BoundBy: "test"}},
			Status: stationv1alpha1.StationStatus{Phase: stationv1alpha1.StationPhaseBound},
		},
		&stationv1alpha1.NetworkEnrollment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-" + cluster, Name: node, Generation: 1},
			Spec: stationv1alpha1.NetworkEnrollmentSpec{ClusterID: cluster, NodeUID: "node-" + node, StationUID: string(uid),
				BindingVersion: 1, PublicKey: key, Addresses: addresses},
		},
	}
}

func newNetworkTest(t *testing.T, objects ...client.Object) (*NetworkReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := stationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&stationv1alpha1.SiteNetwork{}, &stationv1alpha1.NetworkEnrollment{}, &stationv1alpha1.Station{}).
		WithObjects(objects...).Build()
	reconciler, err := NewNetworkReconciler(c, c, testNetworkKey, func() time.Time { return time.Unix(1_800_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, c
}

func reconcileNetwork(t *testing.T, r *NetworkReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: testNetworkKey}); err != nil {
		t.Fatal(err)
	}
}

func enrollmentStatus(t *testing.T, c client.Client, cluster, node string) stationv1alpha1.NetworkEnrollmentStatus {
	t.Helper()
	var enrollment stationv1alpha1.NetworkEnrollment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + cluster, Name: node}, &enrollment); err != nil {
		t.Fatal(err)
	}
	return enrollment.Status
}

func configOf(t *testing.T, status stationv1alpha1.NetworkEnrollmentStatus) network.NodeConfig {
	t.Helper()
	var config network.NodeConfig
	if err := json.Unmarshal([]byte(status.Config), &config); err != nil {
		t.Fatalf("config %q: %v", status.Config, err)
	}
	return config
}

func TestNetworkEnrollsNodesAndAllocatesAddresses(t *testing.T) {
	objects := []client.Object{testSiteNetwork()}
	objects = append(objects, testMember("dev", "cailiang", "key-dev", "10.11.14.128")...)
	objects = append(objects, testMember("dev", "alexfu", "key-3090", "10.11.13.192")...)
	objects = append(objects, testMember("robot-01", "s1u-01-x86", "key-robot-1", "192.168.0.10")...)
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)

	dev := enrollmentStatus(t, c, "dev", "cailiang")
	desk := enrollmentStatus(t, c, "dev", "alexfu")
	robot := enrollmentStatus(t, c, "robot-01", "s1u-01-x86")
	if dev.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || dev.Address != "10.254.240.1" || dev.Role != "Hub" || dev.Site != "office" {
		t.Fatalf("declared hub: %+v", dev)
	}
	// Allocation follows cluster/node order: dev/alexfu, then robot-01/...
	if desk.Address != "10.254.240.2" || robot.Address != "10.254.240.3" || robot.Role != "Leaf" || robot.Site != "lab" {
		t.Fatalf("allocated %s and %s, robot %+v", desk.Address, robot.Address, robot)
	}
	robotConfig := configOf(t, robot)
	if len(robotConfig.Peers) != 2 || robotConfig.Peers[0].Endpoint != "10.11.14.128:51820" || robotConfig.Peers[1].Endpoint != "10.11.13.192:51820" {
		t.Fatalf("the robot dials both hubs, the second at the address it reported: %+v", robotConfig.Peers)
	}
	if robotConfig.Peers[0].AllowedIPs[1] != "10.254.240.0/24" {
		t.Fatalf("the relay carries the pool: %+v", robotConfig.Peers[0])
	}

	// A second robot joins: one new object, no edit anywhere else.
	for _, object := range testMember("robot-02", "s1u-02-x86", "key-robot-2", "192.168.0.10") {
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	reconcileNetwork(t, r)
	second := enrollmentStatus(t, c, "robot-02", "s1u-02-x86")
	if second.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || second.Address != "10.254.240.4" {
		t.Fatalf("second robot: %+v", second)
	}
	if got := configOf(t, enrollmentStatus(t, c, "dev", "cailiang")); len(got.Peers) != 3 {
		t.Fatalf("the hub learns the new robot: %+v", got.Peers)
	}
	if unchanged := enrollmentStatus(t, c, "robot-01", "s1u-01-x86"); unchanged.ConfigHash != robot.ConfigHash {
		t.Fatal("a leaf's configuration must not change when another leaf joins")
	}
	var siteNetwork stationv1alpha1.SiteNetwork
	if err := c.Get(context.Background(), testNetworkKey, &siteNetwork); err != nil {
		t.Fatal(err)
	}
	if siteNetwork.Status.Members != 4 || len(siteNetwork.Status.Allocations) != 3 || len(siteNetwork.Status.Unreachable) != 0 {
		t.Fatalf("status %+v", siteNetwork.Status)
	}

	// The DHCP address of the 3090 changes; the robot follows it.
	var enrollment stationv1alpha1.NetworkEnrollment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns-dev", Name: "alexfu"}, &enrollment); err != nil {
		t.Fatal(err)
	}
	enrollment.Spec.Addresses = []string{"10.11.13.77"}
	if err := c.Update(context.Background(), &enrollment); err != nil {
		t.Fatal(err)
	}
	reconcileNetwork(t, r)
	if got := configOf(t, enrollmentStatus(t, c, "robot-01", "s1u-01-x86")); got.Peers[1].Endpoint != "10.11.13.77:51820" {
		t.Fatalf("endpoint did not follow the reported address: %+v", got.Peers[1])
	}
}

func TestNetworkRejectsStaleIdentityAndReleasesRetiredAddress(t *testing.T) {
	objects := []client.Object{testSiteNetwork()}
	objects = append(objects, testMember("dev", "cailiang", "key-dev", "10.11.14.128")...)
	objects = append(objects, testMember("robot-01", "s1u-01-x86", "key-robot-1")...)
	objects = append(objects, testMember("robot-02", "s1u-02-x86", "key-robot-2")...)
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)
	if got := enrollmentStatus(t, c, "robot-01", "s1u-01-x86").Address; got != "10.254.240.2" {
		t.Fatalf("first robot address %s", got)
	}

	// The node was reinstalled and rebound: the old enrollment names binding 1.
	var station stationv1alpha1.Station
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns-robot-01", Name: "stn-s1u-01-x86"}, &station); err != nil {
		t.Fatal(err)
	}
	station.Spec.Binding.Version, station.Spec.Binding.NodeUID = 2, "node-reinstalled"
	if err := c.Update(context.Background(), &station); err != nil {
		t.Fatal(err)
	}
	reconcileNetwork(t, r)
	stale := enrollmentStatus(t, c, "robot-01", "s1u-01-x86")
	if stale.Phase != stationv1alpha1.NetworkEnrollmentRejected || stale.Config != "" {
		t.Fatalf("stale binding must be rejected: %+v", stale)
	}
	if got := configOf(t, enrollmentStatus(t, c, "dev", "cailiang")); len(got.Peers) != 1 {
		t.Fatalf("the old key must leave every peer list: %+v", got.Peers)
	}

	// The reinstalled node enrolls under binding 2 with a new key and keeps its address.
	var enrollment stationv1alpha1.NetworkEnrollment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns-robot-01", Name: "s1u-01-x86"}, &enrollment); err != nil {
		t.Fatal(err)
	}
	enrollment.Spec.BindingVersion, enrollment.Spec.NodeUID, enrollment.Spec.PublicKey = 2, "node-reinstalled", "key-robot-1-new"
	if err := c.Update(context.Background(), &enrollment); err != nil {
		t.Fatal(err)
	}
	reconcileNetwork(t, r)
	if again := enrollmentStatus(t, c, "robot-01", "s1u-01-x86"); again.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || again.Address != "10.254.240.2" || again.PublicKey != "key-robot-1-new" {
		t.Fatalf("rebound node: %+v", again)
	}

	// Retirement removes the node and frees the address for the next one.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns-robot-01", Name: "stn-s1u-01-x86"}, &station); err != nil {
		t.Fatal(err)
	}
	station.Spec.Retired = true
	if err := c.Update(context.Background(), &station); err != nil {
		t.Fatal(err)
	}
	reconcileNetwork(t, r)
	var siteNetwork stationv1alpha1.SiteNetwork
	if err := c.Get(context.Background(), testNetworkKey, &siteNetwork); err != nil {
		t.Fatal(err)
	}
	if len(siteNetwork.Status.Allocations) != 1 || siteNetwork.Status.Allocations[0].NodeName != "s1u-02-x86" {
		t.Fatalf("allocations after retirement: %+v", siteNetwork.Status.Allocations)
	}
}

func TestNetworkWithoutSiteOrKey(t *testing.T) {
	objects := []client.Object{testSiteNetwork()}
	objects = append(objects, testMember("elsewhere", "stranger", "key-x")...)
	objects = append(objects, testMember("robot-01", "s1u-01-x86", "")...)
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)
	if got := enrollmentStatus(t, c, "elsewhere", "stranger"); got.Phase != stationv1alpha1.NetworkEnrollmentRejected || !strings.Contains(got.Reason, "not in any site") {
		t.Fatalf("unknown cluster: %+v", got)
	}
	if got := enrollmentStatus(t, c, "robot-01", "s1u-01-x86"); got.Phase != stationv1alpha1.NetworkEnrollmentPending {
		t.Fatalf("no key yet: %+v", got)
	}
}

// With a routed private line the registry stays and the tunnel goes.
func TestNetworkRoutedModeKeepsRegistryWithoutTunnel(t *testing.T) {
	siteNetwork := testSiteNetwork()
	siteNetwork.Spec.Mode = stationv1alpha1.SiteNetworkModeRouted
	objects := []client.Object{siteNetwork}
	objects = append(objects, testMember("robot-01", "s1u-01-x86", "", "10.20.0.31")...)
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)
	got := enrollmentStatus(t, c, "robot-01", "s1u-01-x86")
	if got.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || got.Address != "10.20.0.31" || got.Config != "" || got.TunnelAddress != "" {
		t.Fatalf("routed member: %+v", got)
	}
}

// The declaration shipped for the live site must enroll all six nodes of
// 2026-11-12 with the addresses and peers they have today.
func TestLiveSiteDeclarationEnrollsTheSixNodes(t *testing.T) {
	data, err := os.ReadFile("../deploy/site-network.live-20261112.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var siteNetwork stationv1alpha1.SiteNetwork
	if err := yaml.UnmarshalStrict(data, &siteNetwork); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{&siteNetwork}
	live := []struct {
		cluster, node, key, reported, address string
		peers                                 int
	}{
		{"rlark-local-dev-gpu", "cailiang", "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM=", "10.11.14.128", "10.254.240.1", 5},
		{"rlark-ksyun-gpu", "10-0-2-57", "ixGRrgUmo9UMpnCg00v3DPbRT2NEqvmm6eI/lfzpEEc=", "10.0.2.57", "10.0.2.57", 4},
		{"rlark-robot-s1u-01", "s1u-01-x86", "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs=", "192.168.31.250", "10.254.240.3", 5},
		{"rlark-ksyun-gpu", "10-0-2-196", "5Go4sNJOoyAwxL5PXdMYnVHRJJQWkNzdBRUKDWzDzS0=", "10.0.2.196", "10.0.2.196", 4},
		{"rlark-local-dev-gpu", "alexfu", "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "10.11.13.192", "10.254.240.5", 5},
		{"rlark-local-dev-gpu", "wendychen", "uaC5QIh01DyQvTqCObM2dl4hcMIedM5lPYIhkrcQjQQ=", "10.11.14.135", "10.254.240.6", 5},
	}
	for _, node := range live {
		objects = append(objects, testMember(node.cluster, node.node, node.key, node.reported)...)
	}
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)
	for _, node := range live {
		status := enrollmentStatus(t, c, node.cluster, node.node)
		if status.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || status.Address != node.address || len(status.Problems) != 0 {
			t.Errorf("%s: %+v", node.node, status)
			continue
		}
		if config := configOf(t, status); len(config.Peers) != node.peers {
			t.Errorf("%s has %d peers, want %d", node.node, len(config.Peers), node.peers)
		}
	}
	if err := c.Get(context.Background(), testNetworkKey, &siteNetwork); err != nil {
		t.Fatal(err)
	}
	if len(siteNetwork.Status.Unreachable) != 0 || len(siteNetwork.Status.Allocations) != 0 || siteNetwork.Status.Members != 6 {
		t.Fatalf("status %+v", siteNetwork.Status)
	}
}

// A cloud node cannot reach Station API before its tunnel is up: declaring its
// key puts it into its peers' configurations first, and its own enrollment
// takes over once it arrives.
func TestNetworkAdmitsDeclaredKeyBeforeEnrollment(t *testing.T) {
	siteNetwork := testSiteNetwork()
	siteNetwork.Spec.Sites = append(siteNetwork.Spec.Sites, stationv1alpha1.SiteNetworkSite{Name: "ksyun", Clusters: []string{"cloud"}})
	siteNetwork.Spec.Nodes = append(siteNetwork.Spec.Nodes, stationv1alpha1.SiteNetworkNode{
		ClusterID: "cloud", NodeName: "h100-3", Role: "Hub", Address: "10.254.240.9", NodeAddress: "10.0.2.9", PublicKey: "key-cloud",
		Endpoints: []stationv1alpha1.SiteNetworkEndpoint{{Address: "36.103.180.169:51822", Scope: "Public"}},
	})
	objects := append([]client.Object{siteNetwork}, testMember("dev", "cailiang", "key-dev", "10.11.14.128")...)
	r, c := newNetworkTest(t, objects...)
	reconcileNetwork(t, r)
	entry := configOf(t, enrollmentStatus(t, c, "dev", "cailiang"))
	if len(entry.Peers) != 1 || entry.Peers[0].PublicKey != "key-cloud" || entry.Peers[0].Endpoint != "36.103.180.169:51822" || entry.Peers[0].AllowedIPs[1] != "10.0.2.9/32" {
		t.Fatalf("the site entry must dial the declared node: %+v", entry.Peers)
	}

	// The node enrolls through the tunnel with a rotated key: its enrollment wins.
	for _, object := range testMember("cloud", "h100-3", "key-cloud-rotated", "10.0.2.9") {
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	reconcileNetwork(t, r)
	cloud := enrollmentStatus(t, c, "cloud", "h100-3")
	if cloud.Phase != stationv1alpha1.NetworkEnrollmentEnrolled || cloud.TunnelAddress != "10.254.240.9" || cloud.Address != "10.0.2.9" {
		t.Fatalf("enrolled cloud node: %+v", cloud)
	}
	if entry = configOf(t, enrollmentStatus(t, c, "dev", "cailiang")); len(entry.Peers) != 1 || entry.Peers[0].PublicKey != "key-cloud-rotated" {
		t.Fatalf("the enrolled key replaces the declared one: %+v", entry.Peers)
	}
	var updated stationv1alpha1.SiteNetwork
	if err := c.Get(context.Background(), testNetworkKey, &updated); err != nil || updated.Status.Members != 2 {
		t.Fatalf("members %d, %v", updated.Status.Members, err)
	}
}
