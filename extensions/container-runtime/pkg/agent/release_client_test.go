package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
)

func TestReleaseClientUsesAuthenticatedTransport(t *testing.T) {
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
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, err := NewClient(server.URL, certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTPClient.CloseIdleConnections()
	manifest, err := c.TargetRelease(context.Background(), "linux", runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.DownloadRelease(context.Background(), manifest)
	if err != nil || !bytes.Equal(data, f.data) {
		t.Fatalf("download bytes=%d err=%v", len(data), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.DownloadRelease(ctx, manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download: %v", err)
	}
}

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReleaseClientRejectsUnsafeResponses(t *testing.T) {
	f := newReleaseFixture(t)
	manifest := ReleaseManifest{Namespace: f.release.Namespace, Name: f.release.Name, UID: string(f.release.UID), Version: f.release.Spec.Version, OS: "linux", Architecture: runtime.GOARCH, Protocol: "v1", SHA256: f.release.Spec.SHA256, Bytes: int64(len(f.data))}
	valid, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		length     int64
		download   bool
	}{
		{"unknown-field", strings.TrimSuffix(string(valid), "}") + `,"artifactPath":"/etc/passwd"}`, -1, false},
		{"trailing-value", string(valid) + ` {}`, -1, false},
		{"oversized-manifest", strings.Repeat(" ", 17<<10), -1, false},
		{"length-mismatch", "", manifest.Bytes + 1, true},
		{"short-body", "short", -1, true},
		{"extra-byte", string(f.data) + "x", -1, true},
		{"digest-mismatch", strings.Repeat("x", len(f.data)), manifest.Bytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Endpoint: "https://agent.invalid", HTTPClient: &http.Client{Transport: releaseTransport(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: tc.length}, nil
			})}}
			var err error
			if tc.download {
				_, err = c.DownloadRelease(context.Background(), &manifest)
			} else {
				_, err = c.TargetRelease(context.Background(), "linux", runtime.GOARCH)
			}
			if err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
}

func TestReleaseClientRefusesRedirects(t *testing.T) {
	f := newReleaseFixture(t)
	redirected := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected = true; w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, certificate := agentCertificate(t, f.identity.Metadata())
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(target.Certificate())
	c, err := NewClient(server.URL, certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTPClient.CloseIdleConnections()
	if _, err := c.TargetRelease(context.Background(), "linux", runtime.GOARCH); err == nil || redirected {
		t.Fatalf("redirect accepted: %v followed=%v", err, redirected)
	}
}
