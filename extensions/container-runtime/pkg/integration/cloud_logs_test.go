package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/console"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
)

type cloudLogs struct {
	origin, token string
	client        *http.Client
}

// Optional live log assertions use a Gateway-issued token, never an issuer key.
// Ordinary tests neither load credentials nor contact a cloud endpoint.
func cloudLogAcceptance(t *testing.T) *cloudLogs {
	t.Helper()
	origin, file := os.Getenv("RLARK_TEST_RAW_LOG_ORIGIN"), os.Getenv("RLARK_TEST_RAW_LOG_TOKEN_FILE")
	if origin == "" && file == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("log acceptance requires an explicit loopback HTTP origin")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("log acceptance requires a private regular Gateway token file")
	}
	data, err := os.ReadFile(file)
	if err != nil || len(data) == 0 || len(data) > 16384 {
		t.Fatal("cannot read bounded Gateway token file")
	}
	return &cloudLogs{origin: origin, token: strings.TrimSpace(string(data)), client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (l *cloudLogs) request(t *testing.T, query, token string, want int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, l.origin+console.LogPath+query, nil)
	if err != nil {
		t.Fatal("cannot construct log acceptance request")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := l.client.Do(r)
	if err != nil {
		t.Fatal("log acceptance transport failed")
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 128<<10))
	if err != nil || len(data) >= 128<<10 {
		t.Fatal("log response read failed or exceeded bound")
	}
	if res.StatusCode != want {
		t.Fatalf("log acceptance HTTP status=%d, want=%d", res.StatusCode, want)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("log responses must not be cached")
	}
	return data
}

func (l *cloudLogs) checkRunning(t *testing.T, namespace string, a *ext.ContainerAllocation, checkpointReady bool) string {
	t.Helper()
	q := url.Values{"namespace": {namespace}, "allocation": {a.Name}, "allocationUID": {string(a.UID)}, "taskUID": {a.Spec.Task.UID}}
	query := "?" + q.Encode()
	l.request(t, query, "", http.StatusUnauthorized)
	l.request(t, query+"&path=/etc/passwd", l.token, http.StatusBadRequest)
	q.Set("taskUID", uuid.NewString())
	l.request(t, "?"+q.Encode(), l.token, http.StatusConflict)
	var snapshot raw.LogSnapshot
	// Running observes process creation, not completion of Python/CUDA startup.
	// Wait for the workload's explicit output instead of assuming either event
	// means that stdout has already been written and drained by the supervisor.
	if err := wait.PollUntilContextTimeout(context.Background(), time.Second, time.Minute, true, func(context.Context) (bool, error) {
		if err := json.Unmarshal(l.request(t, query, l.token, http.StatusOK), &snapshot); err != nil {
			return false, err
		}
		return len(snapshot.Data) == raw.MaxLogBytes && snapshot.Truncated, nil
	}); err != nil {
		t.Fatal("workload log output did not become ready within the bounded acceptance window")
	}
	if len(snapshot.Data) != raw.MaxLogBytes || !snapshot.Truncated || !bytes.Contains(snapshot.Data, []byte("RLARK_GPU_SMOKE_OK")) || (checkpointReady && !bytes.Contains(snapshot.Data, []byte("RLARK_CHECKPOINT_READY"))) || snapshot.RetentionLimitReached {
		t.Fatalf("unexpected cloud log snapshot: bytes=%d truncated=%t retentionLimitReached=%t", len(snapshot.Data), snapshot.Truncated, snapshot.RetentionLimitReached)
	}
	t.Logf("cloud log snapshot verified: %d bytes, GPU marker, truncation and access rejection", len(snapshot.Data))
	return query
}

func (l *cloudLogs) checkReleased(t *testing.T, query string) {
	t.Helper()
	l.request(t, query, l.token, http.StatusConflict)
	t.Log("released allocation denies old log identity; workload cleanup completed")
}
