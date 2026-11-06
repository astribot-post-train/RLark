package rayrelay

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestCredentials(t *testing.T, id testIdentity) (string, string) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	key, err := x509.MarshalPKCS8PrivateKey(id.certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.leaf.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func writeTestConfig(t *testing.T, cfg ServiceConfig) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceLoadsDedicatedCredentialsAndRejectsInvalidScope(t *testing.T) {
	ca, _, ids := testPKI(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := writeTestCredentials(t, ids["node-a"])
	cfg := ServiceConfig{Mode: "peer", Listen: "127.0.0.1:19001", Certificate: certPath, PrivateKey: keyPath, CA: caPath,
		GatewayAddress: "127.0.0.1:30001", ServerName: "localhost", GatewayPin: pin(ids["gateway"].leaf), NodeID: "node-a", MaxStreams: 8, IdleSeconds: 30,
		Peers: map[string]PeerAccess{
			"node-a": {Addresses: []string{"192.0.2.1"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
			"node-b": {Addresses: []string{"192.0.2.2"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
		}}
	if _, err := LoadPeer(writeTestConfig(t, cfg)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ServiceConfig){
		"wrong mode":             func(c *ServiceConfig) { c.Mode = "gateway" },
		"identity mismatch":      func(c *ServiceConfig) { c.NodeID = "node-b" },
		"public connector":       func(c *ServiceConfig) { c.Listen = "0.0.0.0:19001" },
		"invalid connector port": func(c *ServiceConfig) { c.Listen = "127.0.0.1:0" },
		"invalid pin":            func(c *ServiceConfig) { c.GatewayPin = "wrong" },
		"source hostname":        func(c *ServiceConfig) { c.LocalAddress = "localhost" },
		"source with port":       func(c *ServiceConfig) { c.LocalAddress = "127.0.0.1:1234" },
		"unspecified source":     func(c *ServiceConfig) { c.LocalAddress = "0.0.0.0" },
		"multicast source":       func(c *ServiceConfig) { c.LocalAddress = "224.0.0.1" },
		"gateway-only field":     func(c *ServiceConfig) { c.MaxNodes = 4 },
		"invalid timeout":        func(c *ServiceConfig) { c.IdleSeconds = 1801 },
		"missing ca":             func(c *ServiceConfig) { c.CA = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			mutate(&bad)
			if _, err := LoadPeer(writeTestConfig(t, bad)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPeer(writeTestConfig(t, cfg)); err == nil {
		t.Fatal("public private key accepted")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		t.Fatal(err)
	}

	serverCert, serverKey := writeTestCredentials(t, ids["gateway"])
	gateway := ServiceConfig{Mode: "gateway", Listen: "127.0.0.1:30001", Certificate: serverCert, PrivateKey: serverKey, CA: caPath, MaxNodes: 4, MaxStreams: 8, IdleSeconds: 30,
		Peers: map[string]PeerAccess{
			"node-a": {Pin: pin(ids["node-a"].leaf), Addresses: []string{"192.0.2.1"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
			"node-b": {Pin: pin(ids["node-b"].leaf), Addresses: []string{"192.0.2.2"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
		}}
	if _, err := LoadGateway(writeTestConfig(t, gateway)); err != nil {
		t.Fatal(err)
	}
	gateway.NodeID = "node-a"
	if _, err := LoadGateway(writeTestConfig(t, gateway)); err == nil {
		t.Fatal("gateway accepted peer-only field")
	}
}

func TestServiceRejectsUnknownAndMultipleJSON(t *testing.T) {
	for _, value := range []string{`{"mode":"peer","unknown":"private-value"}`, `{} {}`, strings.Repeat(" ", (1<<20)+1)} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadServiceConfig(path, "peer"); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid JSON or configuration contents leaked: %v", err)
		}
	}
}

func TestPeerServiceListenerFailureStopsSession(t *testing.T) {
	_, roots, ids := testPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client := &Client{Address: "127.0.0.1:1", ServerName: "localhost", Roots: roots, Certificate: ids["node-a"].certificate, Pin: pin(ids["gateway"].leaf), NodeID: "node-a", MaxStreams: 8, IdleTimeout: time.Minute,
		Peers: map[string]PeerAccess{
			"node-a": {Addresses: []string{"192.0.2.1"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
			"node-b": {Addresses: []string{"192.0.2.2"}, Ports: []PortRange{{First: 20000, Last: 20010}}},
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := (&PeerService{Client: client, Listen: listener.Addr().String()}).Run(ctx); err == nil {
		t.Fatal("listener collision not reported")
	}
	if ctx.Err() != nil {
		t.Fatal("peer loop did not terminate after listener failure")
	}
}
