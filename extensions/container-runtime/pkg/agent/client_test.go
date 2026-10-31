package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeartbeatClientMTLSAndValidation(t *testing.T) {
	r, id, _ := observerFixture(t)
	r.Now = nil
	pool, certificate := agentCertificate(t, id.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: r})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := NewClient(server.URL, certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTPClient.CloseIdleConnections()
	observation := Observation{Version: "v1", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}
	if err := client.Heartbeat(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Heartbeat(ctx, observation); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, endpoint := range []string{"http://localhost", "https://user:secret@localhost", "https://localhost/path", "https://localhost?token=bad", "https://localhost#bad"} {
		if _, err := NewClient(endpoint, certificate, roots); err == nil {
			t.Fatalf("invalid endpoint accepted: %s", endpoint)
		}
	}
	_, bad := agentCertificate(t, id.Metadata())
	badClient, err := NewClient(server.URL, bad, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer badClient.HTTPClient.CloseIdleConnections()
	if err := badClient.Heartbeat(context.Background(), observation); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	untrusted, err := NewClient(server.URL, certificate, x509.NewCertPool())
	if err != nil {
		t.Fatal(err)
	}
	if err := untrusted.Heartbeat(context.Background(), observation); err == nil {
		t.Fatal("untrusted API accepted")
	}
}
func TestHeartbeatClientRejectsRedirectAndDiscardsBody(t *testing.T) {
	redirected := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected = true; w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("sensitive upstream detail"))
	}))
	defer server.Close()
	_, certificate := agentCertificate(t, map[string]string{})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(target.Certificate())
	client, err := NewClient(server.URL, certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTPClient.CloseIdleConnections()
	err = client.Heartbeat(context.Background(), Observation{})
	if err == nil || strings.Contains(err.Error(), "sensitive") || redirected {
		t.Fatalf("unsafe redirect/error: %v, redirected=%v", err, redirected)
	}
}
