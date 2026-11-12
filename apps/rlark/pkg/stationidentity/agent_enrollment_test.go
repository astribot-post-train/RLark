package stationidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentEnrollsPublicKeyAndStoresComputedConfiguration(t *testing.T) {
	config := `{"interface":"st-wg0","address":"10.254.240.6/32","listenPort":51820,"mtu":1380,"peers":[],"routes":[]}`
	digest := sha256.Sum256([]byte(config))
	answer := NetworkEnrollmentAnswer{State: "pending"}
	status := http.StatusOK
	var requests []enrollmentRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/network/enrollments" || r.Header.Get("Authorization") != "Bearer pod-token" {
			t.Errorf("unexpected request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var request enrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode enrollment: %v", err)
		}
		requests = append(requests, request)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	defer server.Close()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	target := filepath.Join(t.TempDir(), "config.json")
	agent, err := newAgent(AgentConfig{
		APIURL: server.URL, ClusterID: "cluster-a", TokenFile: "token", CAFile: "ca", Interval: 30 * time.Second, RequestLimit: 2 * time.Second,
		NetworkIdentityFile: "identity", NetworkConfigFile: target,
	}, func(path string) ([]byte, error) {
		switch path {
		case "token":
			return []byte("pod-token\n"), nil
		case "ca":
			return certPEM, nil
		case "identity":
			return []byte(`{"publicKey":"` + testPublicKey + `"}`), nil
		}
		return nil, errors.New("unexpected file")
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	if state, err := agent.Enroll(context.Background()); err != nil || state != "pending" {
		t.Fatalf("pending enrollment: %q, %v", state, err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a pending enrollment must not write a configuration")
	}
	if len(requests) != 1 || requests[0].ClusterID != "cluster-a" || requests[0].PublicKey != testPublicKey || len(requests[0].Addresses) != 1 || requests[0].Addresses[0] != "127.0.0.1" {
		t.Fatalf("request %+v", requests)
	}

	answer = NetworkEnrollmentAnswer{State: "enrolled", Config: json.RawMessage(config), ConfigHash: hex.EncodeToString(digest[:])}
	if state, err := agent.Enroll(context.Background()); err != nil || state != "enrolled" {
		t.Fatalf("enrolled: %q, %v", state, err)
	}
	if stored, err := os.ReadFile(target); err != nil || string(stored) != config {
		t.Fatalf("stored %q, %v", stored, err)
	}

	// Anything but a verified enrolled answer keeps the last configuration.
	answer.ConfigHash = "0000"
	if _, err := agent.Enroll(context.Background()); err == nil {
		t.Fatal("accepted a configuration that does not match its hash")
	}
	answer = NetworkEnrollmentAnswer{State: "rejected", Reason: "retired"}
	if state, err := agent.Enroll(context.Background()); err != nil || state != "rejected" {
		t.Fatalf("rejected: %q, %v", state, err)
	}
	status = http.StatusServiceUnavailable
	if _, err := agent.Enroll(context.Background()); err == nil {
		t.Fatal("an unavailable API is an error")
	}
	status = http.StatusConflict
	if state, err := agent.Enroll(context.Background()); err != nil || state != "pending" {
		t.Fatalf("unbound Station: %q, %v", state, err)
	}
	if stored, err := os.ReadFile(target); err != nil || string(stored) != config {
		t.Fatalf("the last good configuration was lost: %q, %v", stored, err)
	}
}

func TestAgentConfigIdentityFileNeedsConfigFile(t *testing.T) {
	config := AgentConfig{APIURL: "https://station.example", ClusterID: "c", TokenFile: "token", Interval: 30 * time.Second, RequestLimit: time.Second, NetworkIdentityFile: "/x"}
	if err := config.Validate(); err == nil {
		t.Fatal("accepted an identity file without a configuration file")
	}
	// Without a tunnel the node enrolls its address only.
	config.NetworkIdentityFile, config.NetworkConfigFile = "", "/x"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}
