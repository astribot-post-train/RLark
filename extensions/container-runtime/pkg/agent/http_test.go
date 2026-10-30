package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestReleaseMTLSManifestAndDownload(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("Linux ELF artifact")
	}
	f := newReleaseFixture(t)
	store, err := artifact.NewDirectoryStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	pool, certificate := agentCertificate(t, f.identity.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: Reporter{Client: f.reporter.Client, ReleaseStore: store}})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport)
	transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/release", bytes.NewBufferString(`{"action":"manifest","os":"linux","architecture":"`+runtime.GOARCH+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ReleaseManifest
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&manifest) != nil {
		_ = response.Body.Close()
		t.Fatalf("manifest response status=%d", response.StatusCode)
	}
	_ = response.Body.Close()
	if manifest.UID != string(f.release.UID) || manifest.SHA256 == "" || manifest.Bytes != int64(len(f.data)) {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}

	payload, _ := json.Marshal(ReleaseRequest{Action: "download", OS: "linux", Architecture: runtime.GOARCH, ReleaseUID: manifest.UID, SHA256: manifest.SHA256})
	response, err = server.Client().Post(server.URL+"/v1/release", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || !bytes.Equal(data, f.data) {
		t.Fatalf("download status=%d bytes=%d err=%v", response.StatusCode, len(data), readErr)
	}
}

func TestReleaseHTTPRejectsStaleAndMalformedRequests(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("Linux ELF artifact")
	}
	f := newReleaseFixture(t)
	store, err := artifact.NewDirectoryStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	pool, certificate := agentCertificate(t, f.identity.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: Reporter{Client: f.reporter.Client, ReleaseStore: store}})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport)
	transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
	tests := []struct {
		name, body string
		want       int
	}{
		{"unknown-field", `{"action":"manifest","os":"linux","architecture":"` + runtime.GOARCH + `","tenant":"other"}`, http.StatusBadRequest},
		{"stale-digest", `{"action":"download","os":"linux","architecture":"` + runtime.GOARCH + `","releaseUID":"release-uid","sha256":"` + strings.Repeat("0", 64) + `"}`, http.StatusForbidden},
		{"invalid-action", `{"action":"install","os":"linux","architecture":"` + runtime.GOARCH + `"}`, http.StatusForbidden},
		{"trailing-value", `{"action":"manifest","os":"linux","architecture":"` + runtime.GOARCH + `"} {}`, http.StatusBadRequest},
		{"oversized", `{"action":"` + strings.Repeat("x", 17<<10) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response, err := server.Client().Post(server.URL+"/v1/release", "application/json", bytes.NewBufferString(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", response.StatusCode, tc.want)
			}
		})
	}
	// No current target is a successful empty response, not a filesystem error.
	var cluster ext.ContainerCluster
	if err := f.reporter.Client.Get(context.Background(), client.ObjectKey{Namespace: f.identity.Namespace, Name: "cloud"}, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.TargetAgentRelease = ""
	if err := f.reporter.Client.Update(context.Background(), &cluster); err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Post(server.URL+"/v1/release", "application/json", bytes.NewBufferString(`{"action":"manifest","os":"linux","architecture":"`+runtime.GOARCH+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("no target status=%d", response.StatusCode)
	}
}

func TestReleaseMTLSRejectsForeignNodeIdentity(t *testing.T) {
	f := newReleaseFixture(t)
	store, err := artifact.NewDirectoryStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"container-tenant", "container-node-uid", "container-cluster-uid", "container-instance", "container-credential", "cert-role"} {
		t.Run(field, func(t *testing.T) {
			metadata := f.identity.Metadata()
			metadata[field] = "foreign"
			pool, certificate := agentCertificate(t, metadata)
			server := httptest.NewUnstartedServer(Handler{Reporter: Reporter{Client: f.reporter.Client, ReleaseStore: store}})
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
			server.StartTLS()
			defer server.Close()
			server.Client().Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{certificate}
			response, err := server.Client().Post(server.URL+"/v1/release", "application/json", bytes.NewBufferString(`{"action":"manifest","os":"linux","architecture":"`+runtime.GOARCH+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("foreign %s accepted: %d", field, response.StatusCode)
			}
		})
	}
}

func TestReleaseRouteIsAbsentWithoutArtifactStore(t *testing.T) {
	r, _, _ := observerFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/release", bytes.NewBufferString(`{}`))
	response := httptest.NewRecorder()
	Handler{Reporter: r}.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unconfigured release endpoint status=%d", response.Code)
	}
}
