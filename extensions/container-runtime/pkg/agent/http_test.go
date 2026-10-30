package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
)

func agentCertificate(t *testing.T, metadata map[string]string) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Agent test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	cert.SetX509CertMeta(leaf, metadata)
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pool, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

func TestHeartbeatMTLSIdentityAndStrictBody(t *testing.T) {
	for _, mode := range []string{"valid", "wrong tenant", "old UID", "admin role", "forged JSON identity", "missing busy"} {
		t.Run(mode, func(t *testing.T) {
			r, id, _ := observerFixture(t)
			r.Now = nil
			metadata := id.Metadata()
			switch mode {
			case "wrong tenant":
				metadata["container-tenant"] = "other"
			case "old UID":
				metadata["container-node-uid"] = "old"
			case "admin role":
				metadata["cert-role"] = "admin"
			}
			pool, certificate := agentCertificate(t, metadata)
			server := httptest.NewUnstartedServer(Handler{Reporter: r})
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
			server.StartTLS()
			defer server.Close()
			transport := server.Client().Transport.(*http.Transport)
			transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
			observation := map[string]any{"version": "v1", "privateAddress": "10.0.3.85", "visibleGPUUUIDs": []string{"GPU-a"}, "busy": false}
			if mode == "missing busy" {
				delete(observation, "busy")
			}
			if mode == "forged JSON identity" {
				observation["identity"] = id
			}
			body, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Post(server.URL+"/v1/heartbeat", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			want := http.StatusForbidden
			if mode == "valid" {
				want = http.StatusNoContent
			}
			if mode == "forged JSON identity" || mode == "missing busy" {
				want = http.StatusBadRequest
			}
			if response.StatusCode != want {
				t.Fatalf("got %d, want %d", response.StatusCode, want)
			}
		})
	}
}

func TestHeartbeatRejectsUnverifiedPeer(t *testing.T) {
	r, id, _ := observerFixture(t)
	_, certificate := agentCertificate(t, id.Metadata())
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/heartbeat", bytes.NewBufferString(`{}`))
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	response := httptest.NewRecorder()
	Handler{Reporter: r}.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unverified certificate was accepted: %d", response.Code)
	}
}
