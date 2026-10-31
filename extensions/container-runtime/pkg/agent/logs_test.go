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
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type readOutcome struct {
	snapshot raw.LogSnapshot
	err      error
}

func pendingLog(t *testing.T, s LogService, id Identity, target LogTarget) (LogWork, <-chan readOutcome, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	result := make(chan readOutcome, 1)
	go func() { v, err := s.Read(ctx, target); result <- readOutcome{v, err} }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if work := s.Broker.poll(id); work != nil {
			return *work, result, cancel
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("read did not reach bounded broker")
	return LogWork{}, result, cancel
}

func logFixture(t *testing.T) (LogService, Identity, *Assignment, LogTarget) {
	t.Helper()
	tasks, id, assignment := taskFixture(t)
	target := LogTarget{Allocation: objectRef(&assignment.Allocation), TaskUID: string(assignment.Task.UID)}
	return LogService{Tasks: tasks, Broker: &LogBroker{}}, id, assignment, target
}

type fixtureLogs struct {
	task rt.TaskIdentity
	err  error
}

func (f fixtureLogs) Logs(_ context.Context, task rt.TaskIdentity, limit int64) (raw.LogSnapshot, error) {
	if task != f.task || limit != raw.MaxLogBytes {
		return raw.LogSnapshot{}, ErrIdentity
	}
	return raw.LogSnapshot{Data: []byte("only-task-output\x00\xff"), Truncated: true, RetentionLimitReached: true}, f.err
}

func TestLogsRoundTripOverScopedMTLSAndReadFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		s, id, _, target := logFixture(t)
		pool, certificate := agentCertificate(t, id.Metadata())
		server := httptest.NewUnstartedServer(Handler{Reporter: s.Tasks.Reporter, EnableTasks: true, Logs: s.Broker})
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
		server.StartTLS()
		t.Cleanup(server.Close)
		server.Client().Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{certificate}
		c := &Client{Endpoint: server.URL, HTTPClient: server.Client()}
		work, result, _ := pendingLog(t, s, id, target)
		reader := fixtureLogs{task: work.Task}
		if failed {
			reader.err = errors.New("private local path and error must not cross transport")
		}
		require.NoError(t, RespondLog(context.Background(), c, reader))
		got := <-result
		if failed {
			require.ErrorIs(t, got.err, ErrUnavailable)
			require.Empty(t, got.snapshot.Data)
		} else {
			require.NoError(t, got.err)
			require.Equal(t, []byte("only-task-output\x00\xff"), got.snapshot.Data)
			require.True(t, got.snapshot.Truncated)
			require.True(t, got.snapshot.RetentionLimitReached)
		}
		next, err := c.Log(context.Background(), LogRequest{Action: "poll"})
		require.NoError(t, err)
		require.Nil(t, next)
	}
}

func TestLogsRevalidateRevocationReplacementAndRelease(t *testing.T) {
	for _, mode := range []string{"credential", "tenant", "native node", "task UID", "allocation released", "job replaced", "binding replaced", "ambiguous binding", "cluster tenant"} {
		t.Run(mode, func(t *testing.T) {
			s, id, a, target := logFixture(t)
			work, result, cancel := pendingLog(t, s, id, target)
			defer cancel()
			ctx := context.Background()
			c := s.Tasks.Reporter.Client
			switch mode {
			case "credential", "tenant":
				var node ext.ContainerNode
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &node))
				if mode == "credential" {
					node.Annotations[CredentialAnnotation] = "new"
				} else {
					node.Spec.InstanceID = "new"
				}
				require.NoError(t, c.Update(ctx, &node))
			case "native node":
				var node native.Node
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: a.Allocation.Spec.Node.Namespace, Name: a.Allocation.Spec.Node.Name}, &node))
				node.UID = "replaced"
				require.NoError(t, c.Update(ctx, &node))
			case "task UID":
				a.Task.UID = "replaced"
				require.NoError(t, c.Update(ctx, &a.Task))
			case "allocation released":
				a.Allocation.Finalizers = nil
				require.NoError(t, c.Update(ctx, &a.Allocation))
				require.NoError(t, c.Delete(ctx, &a.Allocation))
			case "job replaced":
				var job native.Job
				require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "job"}, &job))
				job.UID = "replaced"
				require.NoError(t, c.Update(ctx, &job))
			case "binding replaced", "ambiguous binding":
				var b ext.ContainerJob
				require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "job", Namespace: "agents"}, &b))
				b.UID = "replaced"
				if mode == "binding replaced" {
					require.NoError(t, c.Update(ctx, &b))
				} else {
					b.Name = "other"
					b.ResourceVersion = ""
					require.NoError(t, c.Create(ctx, &b))
				}
			case "cluster tenant":
				var cc ext.ContainerCluster
				require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "cloud", Namespace: "agents"}, &cc))
				cc.Spec.Runtime = "Docker"
				require.NoError(t, c.Update(ctx, &cc))
			}
			_, err := s.Handle(ctx, id, LogRequest{Action: "complete", Work: &work, Snapshot: &raw.LogSnapshot{Data: []byte("secret")}})
			require.Error(t, err)
			// Even bytes already buffered before revocation must be fenced at return.
			require.NoError(t, s.Broker.complete(id, work, logResult{snapshot: raw.LogSnapshot{Data: []byte("secret")}}))
			got := <-result
			require.Error(t, got.err)
			require.Empty(t, got.snapshot.Data)
		})
	}
}

func TestLogsAllowDiagnosticsDuringCordonSuspendAndStaleHeartbeat(t *testing.T) {
	s, id, a, target := logFixture(t)
	a.Task.Spec.Suspended = true
	a.Task.Generation++
	require.NoError(t, s.Tasks.Reporter.Client.Update(context.Background(), &a.Task))
	var nn native.Node
	require.NoError(t, s.Tasks.Reporter.Client.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	nn.Spec.Unschedulable = true
	require.NoError(t, s.Tasks.Reporter.Client.Update(context.Background(), &nn))
	s.Tasks.Reporter.Now = func() time.Time { return time.Now().Add(time.Hour) }
	work, result, _ := pendingLog(t, s, id, target)
	_, err := s.Handle(context.Background(), id, LogRequest{Action: "complete", Work: &work, Snapshot: &raw.LogSnapshot{Data: []byte("diagnostic")}})
	require.NoError(t, err)
	require.NoError(t, (<-result).err)
}

func TestLogsRejectForeignRequestsAndBoundPayload(t *testing.T) {
	s, id, _, target := logFixture(t)
	work, result, cancel := pendingLog(t, s, id, target)
	ctx := context.Background()
	for _, mode := range []string{"credential", "work ID", "allocation UID", "task UID", "generation", "oversized", "missing result", "ambiguous result"} {
		req := LogRequest{Action: "complete", Work: &work, Snapshot: &raw.LogSnapshot{Data: []byte("content")}}
		copyWork := work
		req.Work = &copyWork
		caller := id
		switch mode {
		case "credential":
			caller.CredentialID = "foreign"
		case "work ID":
			req.Work.ID = "foreign"
		case "allocation UID":
			req.Work.Target.Allocation.UID = "foreign"
		case "task UID":
			req.Work.Target.TaskUID = "foreign"
		case "generation":
			req.Work.Task.Generation++
		case "oversized":
			req.Snapshot.Data = make([]byte, raw.MaxLogBytes+1)
		case "missing result":
			req.Snapshot = nil
		case "ambiguous result":
			req.Unavailable = true
		}
		_, err := s.Handle(ctx, caller, req)
		require.Error(t, err, mode)
	}
	_, err := s.Read(ctx, target)
	require.ErrorIs(t, err, ErrLogBusy)
	cancel()
	got := <-result
	require.ErrorIs(t, got.err, context.Canceled)
	_, err = s.Handle(ctx, id, LogRequest{Action: "complete", Work: &work, Snapshot: &raw.LogSnapshot{}})
	require.ErrorIs(t, err, ErrIdentity)
	_, again, cancelAgain := pendingLog(t, s, id, target)
	cancelAgain()
	require.Error(t, (<-again).err)
}

func TestLogBrokerDuplicateCompletionAndRestart(t *testing.T) {
	b := &LogBroker{}
	id := Identity{Namespace: "a", UID: "node"}
	work := LogWork{ID: "read-1"}
	p, err := b.begin(id, work)
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, b.complete(id, work, logResult{snapshot: raw.LogSnapshot{Data: []byte("first")}}))
	}
	require.Equal(t, "first", string((<-p.result).snapshot.Data))
	require.Nil(t, b.poll(id))
	require.ErrorIs(t, (&LogBroker{}).complete(id, work, logResult{}), ErrIdentity)
	b.forget(p)
	for i := range 32 {
		_, err = b.begin(Identity{UID: string(rune(i + 1))}, work)
		require.NoError(t, err)
	}
	_, err = b.begin(Identity{UID: "overflow"}, work)
	require.ErrorIs(t, err, ErrLogBusy)
}

func TestLogHTTPRejectsUnauthenticatedDisabledAndMalformedRequests(t *testing.T) {
	s, _, _, _ := logFixture(t)
	for _, mode := range []string{"no certificate", "disabled", "wrong method"} {
		h := Handler{Reporter: s.Tasks.Reporter, EnableTasks: true, Logs: s.Broker}
		want, method := 403, http.MethodPost
		if mode == "disabled" {
			h.Logs = nil
			want = 404
		}
		if mode == "wrong method" {
			method = http.MethodGet
			want = 405
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/v1/logs", strings.NewReader(`{"action":"poll"}`)))
		require.Equal(t, want, w.Code)
	}
}
