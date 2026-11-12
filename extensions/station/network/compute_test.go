package network

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

const (
	keyDev   = "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM="
	keyCloud = "ixGRrgUmo9UMpnCg00v3DPbRT2NEqvmm6eI/lfzpEEc="
	keyRobot = "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs="
	keyTrain = "5Go4sNJOoyAwxL5PXdMYnVHRJJQWkNzdBRUKDWzDzS0="
	key3090  = "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4="
)

// liveTopology is the site of 2026-11-12 written as a declaration.
func liveTopology() (Topology, []Member) {
	topology := Topology{
		Pool: "10.254.240.0/24", MTU: 1380, Interface: "st-wg0", ListenPort: 51820, KeepaliveSeconds: 25,
		Sites: []Site{{Name: "office", Mesh: true}, {Name: "lab", Reaches: []string{"office"}}, {Name: "ksyun"}},
	}
	members := []Member{
		{ID: "rlark-local-dev-gpu/cailiang", Site: "office", Role: RoleHub, PublicKey: keyDev, Address: "10.254.240.1",
			Endpoints: []Endpoint{{Address: "10.11.14.128:51820", Scope: ScopeSite}},
			Advertise: []Advertisement{{CIDR: "10.11.5.12/32", Sites: []string{"ksyun"}}}},
		{ID: "rlark-ksyun-gpu/10-0-2-57", Site: "ksyun", Role: RoleHub, PublicKey: keyCloud, Address: "10.254.240.2", NodeAddress: "10.0.2.57",
			Endpoints: []Endpoint{{Address: "36.103.180.169:51820", Scope: ScopePublic}},
			Advertise: []Advertisement{{CIDR: "172.27.0.0/24", Sites: []string{"office"}}}},
		{ID: "rlark-robot-s1u-01/s1u-01-x86", Site: "lab", Role: RoleLeaf, PublicKey: keyRobot, Address: "10.254.240.3",
			Uplink: &Uplink{Gateway: "192.168.0.11", Source: "192.168.31.250", Interface: "enp2s0"}},
		{ID: "rlark-ksyun-gpu/10-0-2-196", Site: "ksyun", Role: RoleHub, PublicKey: keyTrain, Address: "10.254.240.4", NodeAddress: "10.0.2.196",
			Interface: "st-hub0", ListenPort: 51821,
			Endpoints: []Endpoint{{Address: "36.103.180.169:51821", Scope: ScopePublic}},
			Advertise: []Advertisement{{CIDR: "172.27.1.0/24", Sites: []string{"office"}}}},
		{ID: "rlark-local-dev-gpu/alexfu", Site: "office", Role: RoleHub, PublicKey: key3090, Address: "10.254.240.5",
			Endpoints: []Endpoint{{Address: "10.11.13.192:51820", Scope: ScopeSite}}},
	}
	return topology, members
}

// The hand-written ConfigMaps of 2026-11-12, one per node.
var liveConfigs = map[string]string{
	"rlark-local-dev-gpu/cailiang":  `{"interface": "st-wg0", "address": "10.254.240.1/32", "listenPort": 51820, "mtu": 1380, "peers": [{"publicKey": "ixGRrgUmo9UMpnCg00v3DPbRT2NEqvmm6eI/lfzpEEc=", "allowedIPs": ["10.254.240.2/32", "172.27.0.0/24"], "endpoint": "36.103.180.169:51820", "keepaliveSeconds": 25}, {"publicKey": "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs=", "allowedIPs": ["10.254.240.3/32"]}, {"publicKey": "5Go4sNJOoyAwxL5PXdMYnVHRJJQWkNzdBRUKDWzDzS0=", "endpoint": "36.103.180.169:51821", "keepaliveSeconds": 25, "allowedIPs": ["10.254.240.4/32", "172.27.1.0/24"]}, {"publicKey": "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "allowedIPs": ["10.254.240.5/32"]}], "routes": ["10.254.240.2/32", "10.254.240.3/32", "10.254.240.4/32", "172.27.0.0/24", "172.27.1.0/24", "10.254.240.5/32"]}`,
	"rlark-ksyun-gpu/10-0-2-57":     `{"interface": "st-wg0", "address": "10.254.240.2/32", "listenPort": 51820, "mtu": 1380, "peers": [{"publicKey": "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM=", "allowedIPs": ["10.254.240.1/32"]}, {"publicKey": "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs=", "allowedIPs": ["10.254.240.3/32"]}, {"publicKey": "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "allowedIPs": ["10.254.240.5/32"]}], "routes": ["10.254.240.1/32", "10.254.240.3/32", "10.254.240.5/32"]}`,
	"rlark-robot-s1u-01/s1u-01-x86": `{"interface": "st-wg0", "address": "10.254.240.3/32", "listenPort": 51820, "mtu": 1380, "peers": [{"publicKey": "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM=", "allowedIPs": ["10.254.240.1/32"], "endpoint": "10.11.14.128:51820", "keepaliveSeconds": 25}, {"publicKey": "ixGRrgUmo9UMpnCg00v3DPbRT2NEqvmm6eI/lfzpEEc=", "allowedIPs": ["10.254.240.2/32", "10.0.2.57/32"], "endpoint": "36.103.180.169:51820", "keepaliveSeconds": 25}, {"publicKey": "5Go4sNJOoyAwxL5PXdMYnVHRJJQWkNzdBRUKDWzDzS0=", "allowedIPs": ["10.254.240.4/32", "10.0.2.196/32"], "endpoint": "36.103.180.169:51821", "keepaliveSeconds": 25}, {"publicKey": "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "allowedIPs": ["10.254.240.5/32"], "endpoint": "10.11.13.192:51820", "keepaliveSeconds": 25}], "routes": ["10.254.240.1/32", "10.254.240.2/32", "10.254.240.4/32", "10.0.2.57/32", "10.0.2.196/32", "10.254.240.5/32"], "uplinkRoutes": [{"destination": "10.11.14.128/32", "gateway": "192.168.0.11", "source": "192.168.31.250", "interface": "enp2s0"}, {"destination": "36.103.180.169/32", "gateway": "192.168.0.11", "source": "192.168.31.250", "interface": "enp2s0"}, {"destination": "10.11.13.192/32", "gateway": "192.168.0.11", "source": "192.168.31.250", "interface": "enp2s0"}]}`,
	"rlark-ksyun-gpu/10-0-2-196":    `{"interface": "st-hub0", "address": "10.254.240.4/32", "listenPort": 51821, "mtu": 1380, "peers": [{"publicKey": "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM=", "allowedIPs": ["10.254.240.1/32", "10.11.5.12/32"]}, {"publicKey": "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs=", "allowedIPs": ["10.254.240.3/32"]}, {"publicKey": "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "allowedIPs": ["10.254.240.5/32"]}], "routes": ["10.254.240.1/32", "10.11.5.12/32", "10.254.240.3/32", "10.254.240.5/32"]}`,
	"rlark-local-dev-gpu/alexfu":    `{"interface": "st-wg0", "address": "10.254.240.5/32", "listenPort": 51820, "mtu": 1380, "peers": [{"publicKey": "DwjZS7W8Yab4SZN62rtBuvKzrzWwoa6nHwObl1Ck2XM=", "allowedIPs": ["10.254.240.1/32"], "endpoint": "10.11.14.128:51820", "keepaliveSeconds": 25}, {"publicKey": "YxGnfRDiE+94DLi/fCMeVe3zl73UTudA9apw8/BWjXs=", "allowedIPs": ["10.254.240.3/32"]}, {"publicKey": "ixGRrgUmo9UMpnCg00v3DPbRT2NEqvmm6eI/lfzpEEc=", "allowedIPs": ["10.254.240.2/32", "10.0.2.57/32"], "endpoint": "36.103.180.169:51820", "keepaliveSeconds": 25}, {"publicKey": "5Go4sNJOoyAwxL5PXdMYnVHRJJQWkNzdBRUKDWzDzS0=", "allowedIPs": ["10.254.240.4/32", "10.0.2.196/32"], "endpoint": "36.103.180.169:51821", "keepaliveSeconds": 25}], "routes": ["10.254.240.1/32", "10.254.240.3/32", "10.254.240.2/32", "10.0.2.57/32", "10.254.240.4/32", "10.0.2.196/32"]}`,
}

// TestComputeCoversLiveSite is the migration guarantee: switching a node from
// its hand-written ConfigMap to the computed document removes no peer, no
// allowed prefix and no route, so the gateway keeps every session.
func TestComputeCoversLiveSite(t *testing.T) {
	topology, members := liveTopology()
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Problems) != 0 || len(result.Unreachable) != 0 {
		t.Fatalf("problems %v, unreachable %v", result.Problems, result.Unreachable)
	}
	// The only differences from the hand-written files, all additions.
	added := map[string][]string{
		"rlark-local-dev-gpu/cailiang": {"10.0.2.57/32", "10.0.2.196/32", "endpoint " + key3090, "keepalive " + key3090},
		"rlark-ksyun-gpu/10-0-2-57":    {"10.11.5.12/32"},
		"rlark-local-dev-gpu/alexfu":   {"172.27.0.0/24", "172.27.1.0/24"},
	}
	for id, raw := range liveConfigs {
		var live NodeConfig
		if err := json.Unmarshal([]byte(raw), &live); err != nil {
			t.Fatal(err)
		}
		got := result.Configs[id]
		if got.Interface != live.Interface || got.Address != live.Address || got.ListenPort != live.ListenPort || got.MTU != live.MTU {
			t.Errorf("%s: device %+v, live %+v", id, got, live)
		}
		if !reflect.DeepEqual(got.UplinkRoutes, live.UplinkRoutes) {
			t.Errorf("%s: uplink routes %v, live %v", id, got.UplinkRoutes, live.UplinkRoutes)
		}
		var extra []string
		computed := map[string]Peer{}
		for _, peer := range got.Peers {
			computed[peer.PublicKey] = peer
		}
		if len(got.Peers) != len(live.Peers) {
			t.Errorf("%s: %d peers, live %d", id, len(got.Peers), len(live.Peers))
		}
		for _, want := range live.Peers {
			peer, ok := computed[want.PublicKey]
			if !ok {
				t.Errorf("%s: peer %s is missing", id, want.PublicKey)
				continue
			}
			if peer.Endpoint != want.Endpoint {
				if want.Endpoint != "" {
					t.Errorf("%s: peer %s endpoint %q, live %q", id, want.PublicKey, peer.Endpoint, want.Endpoint)
				}
				extra = append(extra, "endpoint "+want.PublicKey)
			}
			if peer.KeepaliveSeconds != want.KeepaliveSeconds {
				if want.KeepaliveSeconds != 0 {
					t.Errorf("%s: peer %s keepalive %d, live %d", id, want.PublicKey, peer.KeepaliveSeconds, want.KeepaliveSeconds)
				}
				extra = append(extra, "keepalive "+want.PublicKey)
			}
			for _, prefix := range want.AllowedIPs {
				if !contains(peer.AllowedIPs, prefix) {
					t.Errorf("%s: peer %s lost allowed prefix %s", id, want.PublicKey, prefix)
				}
			}
			for _, prefix := range peer.AllowedIPs {
				if !contains(want.AllowedIPs, prefix) {
					extra = append(extra, prefix)
				}
			}
		}
		for _, route := range live.Routes {
			if !contains(got.Routes, route) {
				t.Errorf("%s: lost route %s", id, route)
			}
		}
		sort.Strings(extra)
		want := append([]string(nil), added[id]...)
		sort.Strings(want)
		if !reflect.DeepEqual(extra, want) {
			t.Errorf("%s: additions %v, expected %v", id, extra, want)
		}
	}
}

// collectionSite is the full site over the public internet: 96 robots behind
// their own NAT, one workstation that relays between them, three cloud nodes.
func collectionSite(robots int) (Topology, []Member) {
	topology := Topology{
		Pool: "10.254.240.0/22", MTU: 1380, Interface: "st-wg0", ListenPort: 51820, KeepaliveSeconds: 25,
		Sites: []Site{{Name: "collection"}, {Name: "ksyun"}},
	}
	members := []Member{{
		ID: "site/workstation", Site: "collection", Role: RoleHub, Relay: true, PublicKey: "workstation", Address: "10.254.240.1",
		Endpoints: []Endpoint{{Address: "10.20.0.1:51820", Scope: ScopeSite}},
	}}
	for i := 0; i < 3; i++ {
		members = append(members, Member{
			ID: fmt.Sprintf("ksyun/h100-%d", i), Site: "ksyun", Role: RoleHub, PublicKey: fmt.Sprintf("cloud-%d", i),
			Address: fmt.Sprintf("10.254.240.%d", 2+i), NodeAddress: fmt.Sprintf("10.0.2.%d", 10+i),
			Endpoints: []Endpoint{{Address: fmt.Sprintf("36.103.180.169:%d", 51820+i), Scope: ScopePublic}},
		})
	}
	for i := 0; i < robots; i++ {
		members = append(members, Member{
			ID: fmt.Sprintf("robots/s1u-%02d-x86", i+1), Site: "collection", Role: RoleLeaf, PublicKey: fmt.Sprintf("robot-%02d", i+1),
			Address: fmt.Sprintf("10.254.241.%d", i+1),
			Uplink:  &Uplink{Gateway: "192.168.0.11", Source: "192.168.0.10", Interface: "enp2s0"},
		})
	}
	return topology, members
}

func TestComputeCollectionSite(t *testing.T) {
	topology, members := collectionSite(96)
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Problems) != 0 || len(result.Unreachable) != 0 {
		t.Fatalf("problems %v, unreachable %v", result.Problems, result.Unreachable[:min(5, len(result.Unreachable))])
	}
	links, largest := 0, 0
	for id, config := range result.Configs {
		links += len(config.Peers)
		data, _ := json.Marshal(config)
		largest = max(largest, len(data))
		want := map[string]int{"site/workstation": 99, "ksyun/h100-0": 97}[id]
		if want == 0 {
			want = 4
			if config.Interface == "" {
				t.Fatal("empty interface")
			}
		}
		if id == "ksyun/h100-1" || id == "ksyun/h100-2" {
			want = 97
		}
		if len(config.Peers) != want {
			t.Errorf("%s has %d peers, want %d", id, len(config.Peers), want)
		}
	}
	// 96 robots x (workstation + 3 cloud) + workstation x 3 cloud; a full mesh
	// of the same 100 nodes would be 4950.
	if links/2 != 387 {
		t.Errorf("%d tunnels, want 387", links/2)
	}
	if largest > 16*1024 {
		t.Errorf("largest configuration is %d bytes", largest)
	}
	robot := result.Configs["robots/s1u-01-x86"]
	if robot.Peers[0].AllowedIPs[1] != "10.254.240.0/22" || len(robot.UplinkRoutes) != 2 {
		t.Errorf("robot does not relay through the workstation: %+v", robot)
	}
}

func TestComputeReportsUnreachablePairs(t *testing.T) {
	topology, members := collectionSite(2)
	members[0].Relay = false
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"robots/s1u-01-x86 -> robots/s1u-02-x86", "robots/s1u-02-x86 -> robots/s1u-01-x86"}
	if !reflect.DeepEqual(result.Unreachable, want) {
		t.Fatalf("unreachable %v, want %v", result.Unreachable, want)
	}
}

func TestComputeSkipsInvalidMembers(t *testing.T) {
	topology, members := liveTopology()
	members[4].PublicKey = keyDev
	members[2].Address = "10.254.241.3"
	members = append(members, Member{ID: "x/leaf-with-endpoint", Site: "lab", Role: RoleLeaf, PublicKey: "k", Address: "10.254.240.9",
		Endpoints: []Endpoint{{Address: "1.2.3.4:5", Scope: ScopePublic}}})
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rlark-local-dev-gpu/alexfu", "rlark-robot-s1u-01/s1u-01-x86", "x/leaf-with-endpoint"} {
		if _, ok := result.Configs[id]; ok || len(result.Problems[id]) == 0 {
			t.Errorf("%s was not skipped with a problem: %v", id, result.Problems[id])
		}
	}
	if len(result.Configs["rlark-local-dev-gpu/cailiang"].Peers) != 2 {
		t.Errorf("valid members must still be configured: %+v", result.Configs["rlark-local-dev-gpu/cailiang"])
	}
}

func TestComputeDropsPrefixThatHidesAnEndpoint(t *testing.T) {
	topology, members := liveTopology()
	members[0].Advertise = append(members[0].Advertise, Advertisement{CIDR: "10.11.12.0/22"})
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	robot := result.Configs["rlark-robot-s1u-01/s1u-01-x86"]
	if contains(robot.Routes, "10.11.12.0/22") || len(result.Problems["rlark-robot-s1u-01/s1u-01-x86"]) != 1 {
		t.Fatalf("the robot dials 10.11.14.128 and must not route it into the tunnel: %v %v", robot.Routes, result.Problems)
	}
	if !contains(result.Configs["rlark-ksyun-gpu/10-0-2-57"].Routes, "10.11.12.0/22") {
		t.Fatal("a node that does not dial into the prefix keeps it")
	}
}

func TestAllocate(t *testing.T) {
	for _, test := range []struct {
		pool  string
		taken []string
		want  string
	}{
		{"10.254.240.0/24", nil, "10.254.240.1"},
		{"10.254.240.0/24", []string{"10.254.240.1", "10.254.240.2", "10.254.240.3", "10.254.240.5"}, "10.254.240.4"},
		{"10.254.240.0/30", []string{"10.254.240.1"}, "10.254.240.2"},
	} {
		got, err := Allocate(test.pool, test.taken)
		if err != nil || got != test.want {
			t.Errorf("Allocate(%s, %v) = %s, %v; want %s", test.pool, test.taken, got, err, test.want)
		}
	}
	if _, err := Allocate("10.254.240.0/30", []string{"10.254.240.1", "10.254.240.2"}); err == nil {
		t.Error("an exhausted pool must fail")
	}
}

// The document Compute emits is the one station-gateway parses. The same file
// is read by extensions/station-gateway's TestParsesComputedDocument, so the
// two modules cannot drift apart. Regenerate with UPDATE_GOLDEN=1.
func TestComputedDocumentIsTheGatewayContract(t *testing.T) {
	topology, members := liveTopology()
	result, err := Compute(topology, members)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(result.Configs["rlark-robot-s1u-01/s1u-01-x86"])
	if err != nil {
		t.Fatal(err)
	}
	const golden = "../../station-gateway/testdata/computed-s1u-01-x86.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil || string(want) != string(got) {
		t.Fatalf("computed document differs from %s (%v):\n%s", golden, err, got)
	}
}
