//go:build linux

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func diagnosticFixture(t *testing.T) (DockerDiagnosticsService, Identity, *Assignment, LogTarget) {
	t.Helper()
	tasks, id, a := dockerTaskFixture(t)
	var n ext.ContainerNode
	require.NoError(t, tasks.Reporter.Client.Get(context.Background(), client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &n))
	n.Status.Capabilities = append(n.Status.Capabilities, DockerDiagnosticsCapability)
	require.NoError(t, tasks.Reporter.Client.Status().Update(context.Background(), &n))
	return DockerDiagnosticsService{Tasks: tasks, Broker: &DockerDiagnosticBroker{}}, id, a, LogTarget{Allocation: objectRef(&a.Allocation), TaskUID: string(a.Task.UID)}
}

type diagnosticOutcome struct {
	result dockerDiagnosticResult
	err    error
}

func pendingDiagnostic(t *testing.T, s DockerDiagnosticsService, id Identity, target LogTarget, kind string) (DockerDiagnosticWork, <-chan diagnosticOutcome, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	result := make(chan diagnosticOutcome, 1)
	go func() { r, err := s.read(ctx, target, kind); result <- diagnosticOutcome{r, err} }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if w := s.Broker.poll(id); w != nil {
			return *w, result, cancel
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("diagnostic did not reach broker")
	return DockerDiagnosticWork{}, result, cancel
}
func TestDockerDiagnosticsMTLSRoundTripAndUnavailable(t *testing.T) {
	for _, kind := range []string{"logs", "stats"} {
		for _, failed := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "ok", true: "unavailable"}[failed], func(t *testing.T) {
				s, id, _, target := diagnosticFixture(t)
				pool, cert := agentCertificate(t, id.Metadata())
				server := httptest.NewUnstartedServer(Handler{Reporter: s.Tasks.Reporter, EnableTasks: true, DockerDiagnostics: s.Broker})
				server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
				server.StartTLS()
				defer server.Close()
				server.Client().Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{cert}
				c := &Client{Endpoint: server.URL, HTTPClient: server.Client()}
				work, result, _ := pendingDiagnostic(t, s, id, target, kind)
				require.NoError(t, RespondDockerDiagnostics(context.Background(), c, func(_ context.Context, k string, task rt.TaskIdentity) (raw.LogSnapshot, dockerruntime.StatsSnapshot, error) {
					require.Equal(t, kind, k)
					require.Equal(t, work.Task, task)
					if failed {
						return raw.LogSnapshot{}, dockerruntime.StatsSnapshot{}, errors.New("private local path")
					}
					return raw.LogSnapshot{Data: []byte("stdout\nstderr\n"), Truncated: true}, dockerruntime.StatsSnapshot{SampledAt: time.Now().UTC().Format(time.RFC3339Nano), CPUUsageNS: "18446744073709551615", PIDs: "0"}, nil
				}))
				got := <-result
				if failed {
					require.ErrorIs(t, got.err, ErrUnavailable)
				} else {
					require.NoError(t, got.err)
					if kind == "logs" {
						require.Equal(t, []byte("stdout\nstderr\n"), got.result.logs.Data)
					} else {
						require.Equal(t, "18446744073709551615", got.result.stats.CPUUsageNS)
					}
				}
				w, err := c.DockerDiagnostic(context.Background(), DockerDiagnosticRequest{Action: "poll"})
				require.NoError(t, err)
				require.Nil(t, w)
				_, err = c.DockerDiagnostic(context.Background(), DockerDiagnosticRequest{Action: "complete", Work: &work, Unavailable: true})
				require.Error(t, err)
			})
		}
	}
}
func TestDockerDiagnosticsRevalidateRevocationAndBufferedResult(t *testing.T) {
	for _, mode := range []string{"credential", "capability", "execution capability", "generation", "cleanup", "task UID", "binding", "tenant", "cluster"} {
		t.Run(mode, func(t *testing.T) {
			s, id, a, target := diagnosticFixture(t)
			work, result, _ := pendingDiagnostic(t, s, id, target, "logs")
			ctx := context.Background()
			c := s.Tasks.Reporter.Client
			switch mode {
			case "credential", "capability", "execution capability", "tenant":
				var n ext.ContainerNode
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &n))
				switch mode {
				case "credential":
					n.Annotations[CredentialAnnotation] = "revoked"
				case "capability":
					n.Status.Capabilities = []string{DockerExecutionCapability}
				case "execution capability":
					n.Status.Capabilities = []string{DockerDiagnosticsCapability}
				case "tenant":
					n.Spec.InstanceID = "foreign"
				}
				if strings.Contains(mode, "capability") {
					require.NoError(t, c.Status().Update(ctx, &n))
				} else {
					require.NoError(t, c.Update(ctx, &n))
				}
			case "generation":
				a.Task.Generation++
				require.NoError(t, c.Update(ctx, &a.Task))
			case "task UID":
				a.Task.UID = "foreign"
				require.NoError(t, c.Update(ctx, &a.Task))
			case "cleanup":
				a.Allocation.Status.CleanupConfirmed = true
				require.NoError(t, c.Status().Update(ctx, &a.Allocation))
			case "binding":
				var b ext.ContainerJob
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b))
				b.UID = "foreign"
				require.NoError(t, c.Update(ctx, &b))
			case "cluster":
				var cc ext.ContainerCluster
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
				cc.Spec.Runtime = "Raw"
				require.NoError(t, c.Update(ctx, &cc))
			}
			_, err := s.Handle(ctx, id, DockerDiagnosticRequest{Action: "poll"})
			require.Error(t, err)
			_, err = s.Handle(ctx, id, DockerDiagnosticRequest{Action: "complete", Work: &work, Logs: &raw.LogSnapshot{Data: []byte("secret")}})
			require.Error(t, err)
			require.NoError(t, s.Broker.complete(id, work, dockerDiagnosticResult{logs: raw.LogSnapshot{Data: []byte("secret")}}))
			got := <-result
			require.Error(t, got.err)
			require.Empty(t, got.result.logs.Data)
		})
	}
}
func TestDockerDiagnosticsBoundsExactIdentityRetryAndRestart(t *testing.T) {
	s, id, _, target := diagnosticFixture(t)
	work, result, cancel := pendingDiagnostic(t, s, id, target, "logs")
	_, err := s.ReadStats(context.Background(), target)
	require.ErrorIs(t, err, ErrLogBusy)
	for _, mode := range []string{"work ID", "task UID", "generation", "allocation UID", "credential", "kind", "oversized", "missing", "ambiguous"} {
		req := DockerDiagnosticRequest{Action: "complete", Logs: &raw.LogSnapshot{Data: []byte("ok")}}
		w := work
		req.Work = &w
		who := id
		switch mode {
		case "work ID":
			w.ID = "foreign"
		case "task UID":
			w.Task.ObjectUID = "foreign"
		case "generation":
			w.Task.Generation++
		case "allocation UID":
			w.Target.Allocation.UID = "foreign"
		case "credential":
			who.CredentialID = "foreign"
		case "kind":
			w.Kind = "stats"
		case "oversized":
			req.Logs.Data = make([]byte, raw.MaxLogBytes+1)
		case "missing":
			req.Logs = nil
		case "ambiguous":
			req.Unavailable = true
		}
		_, err := s.Handle(context.Background(), who, req)
		require.Error(t, err, mode)
	}
	// Lost completion is replayable while pending and bounded broker state is isolated.
	broker := &DockerDiagnosticBroker{}
	pending, err := broker.begin(id, work)
	require.NoError(t, err)
	payload := dockerDiagnosticResult{logs: raw.LogSnapshot{Data: []byte("once")}}
	require.NoError(t, broker.complete(id, work, payload))
	require.NoError(t, broker.complete(id, work, payload))
	require.Len(t, pending.result, 1)
	broker.forget(pending)
	require.NoError(t, s.Broker.complete(id, work, payload))
	require.NoError(t, (<-result).err)
	cancel()
	work, result, cancel = pendingDiagnostic(t, s, id, target, "stats")
	_, err = (DockerDiagnosticsService{Tasks: s.Tasks, Broker: &DockerDiagnosticBroker{}}).Handle(context.Background(), id, DockerDiagnosticRequest{Action: "complete", Work: &work, Unavailable: true})
	require.Error(t, err)
	cancel()
	require.ErrorIs(t, (<-result).err, context.Canceled)
	work, result, _ = pendingDiagnostic(t, s, id, target, "stats")
	_, err = s.Handle(context.Background(), id, DockerDiagnosticRequest{Action: "complete", Work: &work, Stats: &dockerruntime.StatsSnapshot{SampledAt: time.Now().UTC().Format(time.RFC3339Nano), PIDs: "01"}})
	require.ErrorIs(t, err, ErrCapability)
	_, err = s.Handle(context.Background(), id, DockerDiagnosticRequest{Action: "complete", Work: &work, Unavailable: true})
	require.NoError(t, err)
	require.ErrorIs(t, (<-result).err, ErrUnavailable)
}
func TestDockerDiagnosticHTTPRejectsUnauthenticated(t *testing.T) {
	s, _, _, _ := diagnosticFixture(t)
	h := Handler{Reporter: s.Tasks.Reporter, EnableTasks: true, DockerDiagnostics: s.Broker}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/v1/docker-diagnostics", strings.NewReader(`{"action":"poll"}`)))
		require.Contains(t, []int{403, 405}, w.Code)
	}
}
