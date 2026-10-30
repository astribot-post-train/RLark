package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	clientset "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type rawFixture struct {
	server   *Server
	identity rawnode.Identity
	router   *gin.Engine
}

func newRawFixture(t *testing.T) *rawFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	i := rawnode.Identity{Cluster: "cloud", ClusterUID: "cluster-uid", Namespace: "agents", Node: "node-a", NodeUID: "node-uid", Incarnation: "container-a", TenantID: tenant.DefaultID, CredentialID: "credential-a"}
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: i.Node, Namespace: i.Namespace, UID: types.UID(i.NodeUID), ResourceVersion: "1", Annotations: map[string]string{rawnode.CredentialAnnotation: i.CredentialID}}, Spec: api.NodeSpec{AgentType: api.AgentTypeRaw}, Status: api.NodeStatus{Raw: &api.RawNodeStatus{Incarnation: i.Incarnation}, Reservation: &api.RawNodeReservation{JobUID: "job-uid", ExecutionID: "run", GroupDigest: "group"}}}
	controller := true
	task := &api.Task{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Task"}, ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Namespace: i.Namespace, UID: "task-a", ResourceVersion: "1", Generation: 2, Annotations: map[string]string{utils.ParentUIDAnnotation: "job-uid"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Job", Name: "training", UID: "job-uid", Controller: &controller}}}}
	task.Spec.Runtime.Kind = "Raw"
	task.Spec.Workload.Kind = "raw.rlark.io/process"
	task.Spec.Workload.Version = "v1"
	task.Spec.Workload.Carrier = api.TaskWorkloadCarrier{Type: "raw.rlark.io/process", Version: "v1"}
	task.Spec.Placement.Binding = &api.RawTaskBinding{TaskUID: "task-a", ClusterUID: i.ClusterUID, JobUID: "job-uid", NodeName: i.Node, NodeUID: i.NodeUID, Incarnation: i.Incarnation, ExecutionID: "run", AllocationID: "allocation-a", GroupDigest: "group"}
	peer := task.DeepCopy()
	peer.Name = "worker-b"
	peer.UID = "task-b"
	peer.Spec.Placement.Binding.TaskUID = "task-b"
	peer.Spec.Placement.Binding.NodeName = "node-b"
	peer.Spec.Placement.Binding.NodeUID = "peer-uid"
	foreign := task.DeepCopy()
	foreign.Name = "foreign"
	foreign.UID = "foreign"
	foreign.Labels = map[string]string{tenant.IDLabel: "other"}
	s := NewServer(Config{})
	s.rlarkClient = clientset.NewSimpleClientset(node, task, peer, foreign,
		&api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: i.Cluster, UID: types.UID(i.ClusterUID)}, Spec: api.ClusterSpec{AgentNamespace: i.Namespace}},
		&api.Job{ObjectMeta: metav1.ObjectMeta{Name: "training", UID: "job-uid"}},
	)
	s.kubeClient = kubefake.NewSimpleClientset()
	router := gin.New()
	s.registerHTTPSHandlers(router)
	return &rawFixture{server: s, identity: i, router: router}
}

func (f *rawFixture) request(t *testing.T, method, path, body, contentType string, metadata map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	certificate := &x509.Certificate{}
	cert.SetX509CertMeta(certificate, metadata)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, req)
	return recorder
}
func (f *rawFixture) ownRequest(t *testing.T, method, path, body, contentType string) *httptest.ResponseRecorder {
	return f.request(t, method, path, body, contentType, f.identity.Metadata())
}
func rawAPIPath(path string) string {
	return "/api/raw/kubernetes/apis/" + api.GroupVersion.String() + path
}

func TestRawIdentityRotationAndLiveAuthority(t *testing.T) {
	for _, mode := range []string{"credential", "incarnation", "node-uid", "cluster-uid", "tenant", "ambiguous-cluster", "deleting-node", "non-raw", "api-outage"} {
		t.Run(mode, func(t *testing.T) {
			f := newRawFixture(t)
			ctx := context.Background()
			node, err := f.server.rlarkClient.RlinfV1alpha1().Nodes("agents").Get(ctx, "node-a", metav1.GetOptions{})
			require.NoError(t, err)
			switch mode {
			case "credential":
				node.Annotations[rawnode.CredentialAnnotation] = "rotated"
			case "incarnation":
				node.Status.Raw.Incarnation = "replacement"
			case "node-uid":
				node.UID = "replacement"
			case "cluster-uid":
				f.identity.ClusterUID = "old-cluster"
			case "tenant":
				f.identity.TenantID = "other"
			case "ambiguous-cluster":
				_, err = f.server.rlarkClient.RlinfV1alpha1().Clusters().Create(ctx, &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "ambiguous"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}, metav1.CreateOptions{})
				require.NoError(t, err)
			case "deleting-node":
				now := metav1.Now()
				node.DeletionTimestamp = &now
			case "non-raw":
				node.Spec.AgentType = api.AgentTypeKubernetes
			case "api-outage":
				f.server.rlarkClient.(*clientset.Clientset).PrependReactor("list", "clusters", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("private dependency detail")
				})
			}
			_, err = f.server.rlarkClient.RlinfV1alpha1().Nodes("agents").Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
			response := f.ownRequest(t, http.MethodPost, "/api/raw/heartbeat", "", "application/json")
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.NotContains(t, response.Body.String(), "private dependency detail")
			leases, err := f.server.kubeClient.CoordinationV1().Leases("agents").List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, leases.Items)
		})
	}
}

func TestRawCredentialSigningRotationAndAdminBoundary(t *testing.T) {
	f := newRawFixture(t)
	ca, err := cert.GenerateCA(cert.GenerateTemplateCA())
	require.NoError(t, err)
	f.server.ca = []cert.Data{*ca}
	i := f.identity
	request := RawNodeSignRequest{Cluster: i.Cluster, ClusterUID: i.ClusterUID, Namespace: i.Namespace, Node: i.Node, NodeUID: i.NodeUID, Incarnation: i.Incarnation, TenantID: i.TenantID, EnrollmentID: "c05d3fb3-9583-426a-88b6-2d7a897fd998", PreviousCredentialID: i.CredentialID}
	body, err := json.Marshal(SignRequest{Role: rawnode.Role, RawNode: &request})
	require.NoError(t, err)
	denied := f.ownRequest(t, http.MethodPost, "/api/sign", string(body), "application/json")
	require.Equal(t, http.StatusForbidden, denied.Code)
	admin := map[string]string{apis.MetaCertRole: "admin", apis.MetaPermissionAdmin: "true"}
	first := f.request(t, http.MethodPost, "/api/sign", string(body), "application/json", admin)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var signed SignResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &signed))
	parsed, err := cert.LoadData([]byte(signed.CertPEM), []byte(signed.KeyPEM))
	require.NoError(t, err)
	require.NoError(t, parsed.Cert.CheckSignatureFrom(ca.Cert))
	metadata, ok := cert.GetX509CertMeta(parsed.Cert)
	require.True(t, ok)
	require.Empty(t, metadata[apis.MetaKubernetesImpersonation])
	require.Empty(t, metadata[apis.MetaPermissionAdmin])
	require.Equal(t, http.StatusNoContent, f.request(t, http.MethodPost, "/api/raw/heartbeat", "", "", metadata).Code)
	request.PreviousCredentialID = request.EnrollmentID
	request.EnrollmentID = "b0aa1fba-5fdb-48a5-b1bb-8023cc63168b"
	body, err = json.Marshal(SignRequest{Role: rawnode.Role, RawNode: &request})
	require.NoError(t, err)
	second := f.request(t, http.MethodPost, "/api/sign", string(body), "application/json", admin)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.Equal(t, http.StatusForbidden, f.request(t, http.MethodPost, "/api/raw/heartbeat", "", "", metadata).Code)
}

func TestRawCertificatesCannotUseGeneralRoutes(t *testing.T) {
	f := newRawFixture(t)
	metadata := f.identity.Metadata()
	// Even accidental broad legacy grants cannot escape the Raw role boundary.
	metadata[apis.MetaPermissionAdmin] = "true"
	metadata[apis.MetaKubernetesImpersonation] = "-"
	for _, path := range []string{"/api/connect", "/api/sign", "/api/revoke", "/api/kubernetes/apis/rlinf.io/v1alpha1/nodes", "/api/proxy/a/x", "/api/taskproxy/a/x", "/api/podproxy/a/x", "/api/terminal/a/ns/pod"} {
		method := http.MethodGet
		if path == "/api/sign" || path == "/api/revoke" {
			method = http.MethodPost
		}
		require.Equal(t, http.StatusForbidden, f.request(t, method, path, "", "", metadata).Code, path)
	}
	require.Equal(t, http.StatusForbidden, f.request(t, http.MethodPost, "/api/raw/heartbeat", "", "", map[string]string{apis.MetaCertRole: "agent"}).Code)
}

func TestRawScopedReadsAndUnsupportedOperations(t *testing.T) {
	f := newRawFixture(t)
	list := f.ownRequest(t, http.MethodGet, rawAPIPath("/namespaces/agents/tasks"), "", "")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var tasks api.TaskList
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &tasks))
	require.Len(t, tasks.Items, 2)
	for _, item := range tasks.Items {
		require.NotEqual(t, "foreign", item.Name)
	}
	for _, path := range []string{"/namespaces/agents/tasks/foreign", "/namespaces/elsewhere/tasks", "/jobs", "/namespaces/agents/tasks?watch=true", "/namespaces/agents/secrets", "/namespaces/agents/nodes/node-a/status", "/clusters/foreign"} {
		response := f.ownRequest(t, http.MethodGet, rawAPIPath(path), "", "")
		require.Equal(t, http.StatusForbidden, response.Code, path+response.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		require.Equal(t, http.StatusForbidden, f.ownRequest(t, method, rawAPIPath("/namespaces/agents/tasks/worker-a"), "{}", "application/json").Code)
	}
	require.Equal(t, http.StatusOK, f.ownRequest(t, http.MethodGet, rawAPIPath("/jobs/training"), "", "").Code)
}

func TestRawTaskStatusWritesFenceScopeVersionAndFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"valid", `{"metadata":{"resourceVersion":"1"},"status":{"phase":"Running","observedGeneration":2,"observedNodes":["node-a"]}}`, 200},
		{"missing-version", `{"status":{"phase":"Running","observedGeneration":2}}`, 409},
		{"stale-version", `{"metadata":{"resourceVersion":"old"},"status":{"phase":"Running","observedGeneration":2}}`, 409},
		{"stale-generation", `{"metadata":{"resourceVersion":"1"},"status":{"phase":"Running","observedGeneration":1}}`, 403},
		{"foreign-node", `{"metadata":{"resourceVersion":"1"},"status":{"phase":"Running","observedGeneration":2,"observedNodes":["node-b"]}}`, 403},
		{"spec", `{"metadata":{"resourceVersion":"1"},"spec":{"suspended":true},"status":{"observedGeneration":2}}`, 403},
		{"label", `{"metadata":{"resourceVersion":"1","labels":{"tenant.rlark.io/id":"other"}},"status":{"observedGeneration":2}}`, 403},
		{"condition", `{"metadata":{"resourceVersion":"1"},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True"}]}}`, 403},
		{"unknown-field", `{"metadata":{"resourceVersion":"1"},"status":{"unsupported":true,"observedGeneration":2}}`, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRawFixture(t)
			response := f.ownRequest(t, http.MethodPatch, rawAPIPath("/namespaces/agents/tasks/worker-a/status"), tc.body, string(types.MergePatchType))
			require.Equal(t, tc.status, response.Code, response.Body.String())
			if tc.status != 200 {
				task, err := f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(context.Background(), "worker-a", metav1.GetOptions{})
				require.NoError(t, err)
				require.Empty(t, task.Status.Phase)
			}
		})
	}
	f := newRawFixture(t)
	body := `{"metadata":{"resourceVersion":"1"},"status":{"phase":"Running","observedGeneration":2}}`
	require.Equal(t, http.StatusForbidden, f.ownRequest(t, http.MethodPatch, rawAPIPath("/namespaces/agents/tasks/worker-b/status"), body, string(types.MergePatchType)).Code)
	f.server.rlarkClient.(*clientset.Clientset).PrependReactor("update", "tasks", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "tasks"}, "worker-a", errors.New("concurrent write"))
	})
	require.Equal(t, http.StatusConflict, f.ownRequest(t, http.MethodPatch, rawAPIPath("/namespaces/agents/tasks/worker-a/status"), body, string(types.MergePatchType)).Code)
}

func TestRawClaimsPreserveFrozenOwnershipAndOtherFinalizers(t *testing.T) {
	f := newRawFixture(t)
	old, err := f.server.rlarkClient.RlinfV1alpha1().Tasks("agents").Get(context.Background(), "worker-a", metav1.GetOptions{})
	require.NoError(t, err)
	next := old.DeepCopy()
	next.Finalizers = []string{host.TaskFinalizer}
	next.Annotations[host.TaskClaimantAnnotation] = "agents/Raw/node-uid"
	next.Annotations[host.TaskClaimedWorkloadTypeAnnotation] = "raw.rlark.io/process"
	next.Annotations[host.TaskClaimedWorkloadVersionAnnotation] = "v1"
	next.Annotations[host.TaskClaimedCarrierTypeAnnotation] = "raw.rlark.io/process"
	next.Annotations[host.TaskClaimedCarrierVersionAnnotation] = "v1"
	require.True(t, validRawClaimMutation(old, next, f.identity))
	old = next.DeepCopy()
	old.Finalizers = append(old.Finalizers, "other/finalizer")
	for _, mode := range []string{"steal", "change-frozen", "remove-other", "remove-live", "arbitrary-annotation"} {
		t.Run(mode, func(t *testing.T) {
			next := old.DeepCopy()
			switch mode {
			case "steal":
				next.Annotations[host.TaskClaimantAnnotation] = "agents/Raw/peer"
			case "change-frozen":
				next.Annotations[host.TaskClaimedWorkloadVersionAnnotation] = "v2"
			case "remove-other":
				next.Finalizers = []string{host.TaskFinalizer}
			case "remove-live":
				next.Finalizers = []string{"other/finalizer"}
			case "arbitrary-annotation":
				next.Annotations["other"] = "changed"
			}
			require.False(t, validRawClaimMutation(old, next, f.identity))
		})
	}
	now := metav1.Now()
	old.DeletionTimestamp = &now
	next = old.DeepCopy()
	next.Finalizers = []string{"other/finalizer"}
	for _, key := range []string{host.TaskClaimantAnnotation, host.TaskClaimedWorkloadTypeAnnotation, host.TaskClaimedWorkloadVersionAnnotation, host.TaskClaimedCarrierTypeAnnotation, host.TaskClaimedCarrierVersionAnnotation} {
		delete(next.Annotations, key)
	}
	require.True(t, validRawClaimMutation(old, next, f.identity))
}

func TestRawHeartbeatOwnershipAndReplay(t *testing.T) {
	f := newRawFixture(t)
	for range 2 {
		response := f.ownRequest(t, http.MethodPost, "/api/raw/heartbeat", "", "")
		require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	}
	lease, err := f.server.kubeClient.CoordinationV1().Leases("agents").Get(context.Background(), "node-heartbeat-node-a", metav1.GetOptions{})
	require.NoError(t, err)
	node, err := f.server.rlarkClient.RlinfV1alpha1().Nodes("agents").Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, rawnode.HeartbeatMatches(node, lease))
	f = newRawFixture(t)
	_, err = f.server.kubeClient.CoordinationV1().Leases("agents").Create(context.Background(), &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "node-heartbeat-node-a", Namespace: "agents"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, f.ownRequest(t, http.MethodPost, "/api/raw/heartbeat", "", "").Code)
}
