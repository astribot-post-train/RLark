package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rawruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/rawallocation"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Only the process is substituted: every Host claim, admission read and status
// patch crosses the production HTTP authorization boundary through a real client.
func TestRawHostThroughScopedHTTPAPI(t *testing.T) {
	ctx := context.Background()
	f := newRawFixture(t)
	node, err := f.server.rlarkClient.RlinfV1alpha1().Nodes("agents").Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	node.Status.Reservation = nil
	node.Status.Phase = api.NodeOnline
	heartbeat := metav1.Now()
	node.Status.LastHeartbeatTime = &heartbeat
	node.Status.Raw.TrainingIP = "10.0.0.1"
	node.Status.Raw.NetworkScope = "private"
	node.Status.Raw.Environment = "prepared"
	node.Status.Raw.AuthorizedGPUUUIDs = []string{"GPU-authorized"}
	cluster, err := f.server.rlarkClient.RlinfV1alpha1().Clusters().Get(ctx, "cloud", metav1.GetOptions{})
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	scheduler := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(node, cluster).Build()
	manager := rawallocation.Manager{Client: scheduler}
	bindings, err := manager.Reserve(ctx, rawallocation.Request{Cluster: client.ObjectKeyFromObject(cluster), JobUID: "job-uid", TenantID: f.identity.TenantID, ExecutionID: "run", Environment: "prepared", Nodes: []client.ObjectKey{client.ObjectKeyFromObject(node)}})
	require.NoError(t, err)
	binding, err := manager.AssignTask(ctx, client.ObjectKeyFromObject(node), bindings[0], f.identity.TenantID, "task-a")
	require.NoError(t, err)
	require.NoError(t, scheduler.Get(ctx, client.ObjectKeyFromObject(node), node))
	_, err = f.server.rlarkClient.RlinfV1alpha1().Nodes("agents").Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	task, err := f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(ctx, "worker-a", metav1.GetOptions{})
	require.NoError(t, err)
	task.Spec.Placement.Binding = &binding
	parameters, err := json.Marshal(rawruntime.ProcessSpec{Argv: []string{"/usr/bin/python3", "-c", "print(45)"}, Directory: "/work"})
	require.NoError(t, err)
	task.Spec.Workload.Parameters.Raw = parameters
	_, err = f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Update(ctx, task, metav1.UpdateOptions{})
	require.NoError(t, err)
	certificate := &x509.Certificate{}
	cert.SetX509CertMeta(certificate, f.identity.Metadata())
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		recorder := httptest.NewRecorder()
		f.router.ServeHTTP(recorder, r)
		if recorder.Code >= 400 {
			t.Logf("%s %s: %d %s", r.Method, r.URL, recorder.Code, recorder.Body.String())
		}
		for k, v := range recorder.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer httpServer.Close()
	management, err := client.New(&rest.Config{Host: httpServer.URL + "/api/raw/kubernetes", Timeout: 5 * time.Second}, client.Options{Scheme: scheme, Mapper: rawnode.RESTMapper()})
	require.NoError(t, err)
	supervisor := &rawHostSupervisor{}
	admission := rawruntime.NodeAdmission{Client: management, Node: client.ObjectKeyFromObject(node), NodeUID: f.identity.NodeUID, Incarnation: f.identity.Incarnation, TenantID: f.identity.TenantID}
	assemble := func() *host.Host {
		h, err := rawruntime.NewHost(ctx, management, admission, &rawruntime.Provider{ID: "raw-node", Supervisor: supervisor}, []workload.Adapter{rawruntime.ProcessAdapter{}})
		require.NoError(t, err)
		return h
	}
	for range 2 {
		result, err := assemble().Reconcile(ctx, client.ObjectKeyFromObject(task))
		require.NoError(t, err)
		require.Equal(t, host.OutcomeSucceeded, result.Outcome)
	}
	require.Equal(t, 1, supervisor.starts)
	current, err := f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(ctx, "worker-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, api.TaskPhaseRunning, current.Status.Phase)
	current.Spec.Suspended = true
	_, err = f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
	result, err := assemble().Reconcile(ctx, client.ObjectKeyFromObject(task))
	require.NoError(t, err)
	require.Equal(t, "TaskSuspended", result.Reason)
	current, err = f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(ctx, "worker-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, api.TaskPhaseStopped, current.Status.Phase)
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	current.DeletionTimestamp = &now
	_, err = f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = assemble().Reconcile(ctx, client.ObjectKeyFromObject(task))
	require.NoError(t, err)
	current, err = f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(ctx, "worker-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, current.Finalizers, host.TaskFinalizer)
	require.Empty(t, current.Annotations[host.TaskClaimantAnnotation])
}

type rawHostSupervisor struct {
	record *rawruntime.Record
	starts int
}

func (s *rawHostSupervisor) Observe(_ context.Context, _ rt.TaskIdentity) (rawruntime.Record, error) {
	if s.record == nil {
		return rawruntime.Record{}, fault.New(fault.NotFound, "absent")
	}
	return *s.record, nil
}
func (s *rawHostSupervisor) Ensure(_ context.Context, task rt.TaskIdentity, spec rawruntime.ProcessSpec) (rawruntime.Record, error) {
	if s.record == nil {
		s.starts++
		s.record = &rawruntime.Record{Task: task, Spec: spec, Phase: "Running"}
	}
	return *s.record, nil
}
func (s *rawHostSupervisor) Stop(_ context.Context, _ rt.TaskIdentity) (rawruntime.Record, error) {
	s.record.Cleaned = true
	s.record.Deleted = true
	return *s.record, nil
}
