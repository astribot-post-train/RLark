package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestConfigValidation(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	valid := fmt.Sprintf(`{"interface":"wg-site","address":"10.253.0.1/32","listenPort":51820,"mtu":1420,"peers":[{"publicKey":%q,"allowedIPs":["10.253.0.2/32"]}],"routes":["10.253.0.2/32"]}`, key.PublicKey().String())
	if _, err := parse([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	tests := []string{`{}`, `{"interface":"wg-site","address":"bad","listenPort":51820,"mtu":1420}`, `{"interface":"wg-site","address":"10.253.0.1/32","listenPort":51820,"mtu":1420,"routes":["0.0.0.0/0"]}`, `{"interface":"wg-site","address":"10.253.0.1/32","listenPort":51820,"mtu":1420,"unknown":1}`}
	for _, input := range tests {
		if _, err := parse([]byte(input)); err == nil {
			t.Errorf("accepted invalid config %s", input)
		}
	}
}

func TestUnchangedPeerKeepsLearnedEndpoint(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	peerKey, _ := wgtypes.GeneratePrivateKey()
	_, prefix, _ := net.ParseCIDR("10.254.240.3/32")
	keep := time.Duration(0)
	d := &desired{config: config{ListenPort: 51820}, peers: []wgtypes.PeerConfig{{PublicKey: peerKey.PublicKey(), AllowedIPs: []net.IPNet{*prefix}, PersistentKeepaliveInterval: &keep}}}
	endpoint := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234}
	current := &wgtypes.Device{PrivateKey: key, ListenPort: 51820, Peers: []wgtypes.Peer{{PublicKey: peerKey.PublicKey(), AllowedIPs: []net.IPNet{*prefix}, Endpoint: endpoint}}}
	cfg, changed := deviceConfig(d, key, current)
	if changed || cfg.Peers[0].Endpoint.String() != endpoint.String() {
		t.Fatal("unchanged peer lost learned endpoint")
	}
	d.ListenPort = 51821
	cfg, changed = deviceConfig(d, key, current)
	if !changed || cfg.Peers[0].Endpoint.String() != endpoint.String() {
		t.Fatal("changed device did not retain learned endpoint")
	}
}
func TestKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity", "private.key")
	a, err := keyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := keyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("identity changed on restart")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("unexpected key mode %s", fi.Mode())
	}
}

func TestInvalidKeyIsNotReplaced(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := keyFile(p); err == nil {
		t.Fatal("accepted corrupt identity")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "corrupt\n" {
		t.Fatal("replaced existing identity")
	}
}
func TestDuplicatePeerRejected(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprintf(`{"publicKey":%q,"allowedIPs":["10.253.0.2/32"]}`, key.PublicKey().String())
	data := fmt.Sprintf(`{"interface":"wg-site","address":"10.253.0.1/32","listenPort":51820,"mtu":1420,"peers":[%s,%s]}`, p, p)
	if _, err = parse([]byte(data)); err == nil {
		t.Fatal("accepted duplicate peer")
	}
}

func TestTrailingJSONRejected(t *testing.T) {
	data := []byte(`{"interface":"wg-site","address":"10.253.0.1/32","listenPort":51820,"mtu":1420} {}`)
	if _, err := parse(data); err == nil {
		t.Fatal("accepted trailing JSON object")
	}
}

func TestUplinkRouteValidation(t *testing.T) {
	base := `{"interface":"wg-site","address":"10.254.240.1/32","listenPort":51820,"mtu":1420,"uplinkRoutes":[%s]}`
	valid := `{"destination":"10.11.14.128/32","gateway":"192.168.0.11","source":"192.168.31.250","interface":"enp2s0"}`
	if _, err := parse([]byte(fmt.Sprintf(base, valid))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"destination":"10.11.0.0/16","gateway":"192.168.0.11","source":"192.168.31.250","interface":"enp2s0"}`,
		`{"destination":"10.11.14.128/32","gateway":"bad","source":"192.168.31.250","interface":"enp2s0"}`,
		`{"destination":"10.11.14.128/32","gateway":"192.168.0.11","source":"bad","interface":"enp2s0"}`,
	} {
		if _, err := parse([]byte(fmt.Sprintf(base, bad))); err == nil {
			t.Errorf("accepted invalid uplink %s", bad)
		}
	}
}
