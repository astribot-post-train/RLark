package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpgradeMTLSClientAndStrictRequestBoundary(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	// Certificate validation uses real time; keep the reservation heartbeat fresh.
	now := time.Now().Truncate(time.Second)
	f.reporter.Now = func() time.Time { return now }
	pool, certificate := agentCertificate(t, f.identity.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: f.reporter, EnableUpgrades: true})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, err := NewClient(server.URL, certificate, roots)
	mustAgent(t, err)
	defer c.HTTPClient.CloseIdleConnections()
	assigned, err := c.Upgrade(context.Background(), UpgradeRequest{Action: "assignment"})
	mustAgent(t, err)
	if assigned == nil || assigned.Granted || assigned.Operation.UID != string(f.op.UID) {
		t.Fatal("discovery granted application or lost identity")
	}
	req := UpgradeRequest{Action: "get", Operation: assigned.Operation, Allocation: *assigned.Allocation}
	got, err := c.Upgrade(context.Background(), req)
	mustAgent(t, err)
	if got.Granted {
		t.Fatal("get granted installation")
	}
	mustAgent(t, f.reporter.Report(context.Background(), f.identity, Observation{Version: "old", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}))
	claim := upgradeClaim(f)
	claim.ResourceVersion = got.ResourceVersion
	granted, err := c.Upgrade(context.Background(), claim)
	mustAgent(t, err)
	if !granted.Granted {
		t.Fatal("authenticated exact claim was denied")
	}
	now = now.Add(time.Second)
	mustAgent(t, f.reporter.Report(context.Background(), f.identity, Observation{Version: "next", PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}))
	idle := true
	completed, err := c.Upgrade(context.Background(), UpgradeRequest{Action: "complete", Operation: claim.Operation, Allocation: claim.Allocation, AttemptID: claim.AttemptID, Result: "Succeeded", Version: "next", Idle: &idle})
	mustAgent(t, err)
	if completed.Phase != "Applying" || completed.Completion == nil || completed.Granted {
		t.Fatal("authenticated completion bypassed controller cleanup")
	}
	for _, body := range []string{`{"action":"assignment","tenant":"foreign"}`, `{} {}`, strings.Repeat(" ", 17<<10)} {
		response, err := c.HTTPClient.Post(server.URL+"/v1/upgrade", "application/json", strings.NewReader(body))
		mustAgent(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid body status %d", response.StatusCode)
		}
	}
	// A different certificate remains a valid CA client but cannot discover this node.
	foreign := f.identity
	foreign.TenantID = "foreign"
	_, foreignCertificate := agentCertificate(t, foreign.Metadata())
	// agentCertificate creates a new CA; verify the server also rejects untrusted issuers.
	c.HTTPClient.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{foreignCertificate}
	c.HTTPClient.CloseIdleConnections()
	if _, err := c.Upgrade(context.Background(), UpgradeRequest{Action: "assignment"}); err == nil {
		t.Fatal("foreign issuer accepted")
	}
}

func TestUpgradeEndpointDisabledAndUnauthenticated(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		status  int
	}{{false, http.StatusNotFound}, {true, http.StatusForbidden}} {
		recorder := httptest.NewRecorder()
		Handler{EnableUpgrades: tc.enabled}.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/upgrade", strings.NewReader(`{"action":"assignment"}`)))
		if recorder.Code != tc.status {
			t.Fatalf("enabled=%v status=%d", tc.enabled, recorder.Code)
		}
	}
}
