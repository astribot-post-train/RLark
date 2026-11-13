package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

func nodeOperationTestRequest() nodeOperationCreateRequest {
	return nodeOperationCreateRequest{
		ClusterID: "cluster-a", RequestID: "request-001", Action: "Join", NodeName: "worker-a",
		SSHAddress: "10.11.1.20", SSHPort: 22, SSHUser: "operator",
		HostKeyFingerprint: "SHA256:" + strings.Repeat("a", 43), SSHPassword: "never-return-this",
	}
}

func nodeOperationCall(g *Gateway, request nodeOperationCreateRequest) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	body, _ := json.Marshal(request)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/station/node-operations", strings.NewReader(string(body)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(authSubjectKey, "admin")
	g.handleCreateNodeOperation(ctx)
	return recorder
}

func TestCreateNodeOperationStrictJSONAndValidation(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	for name, body := range map[string]string{
		"unknown field":   `{"cluster_id":"cluster-a","request_id":"r","action":"Join","node_name":"worker","namespace":"other"}`,
		"trailing object": `{}` + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/station/node-operations", strings.NewReader(body))
			ctx.Set(authSubjectKey, "admin")
			(&Gateway{stationClient: client, rawClient: fake.NewSimpleClientset()}).handleCreateNodeOperation(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	request := nodeOperationTestRequest()
	request.SSHAddress = "bad address"
	if got := nodeOperationCall(&Gateway{stationClient: client, rawClient: fake.NewSimpleClientset()}, request); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid SSH address status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestCreateNodeOperationStoresCredentialWithoutReturningIt(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	raw := fake.NewSimpleClientset()
	request := nodeOperationTestRequest()
	recorder := nodeOperationCall(&Gateway{stationClient: client, rawClient: raw, jwtSigningKey: []byte(strings.Repeat("k", 32))}, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), request.SSHPassword) || strings.Contains(recorder.Body.String(), "node-auth-") || strings.Contains(recorder.Body.String(), "credentialSecret") {
		t.Fatalf("credential material or Secret reference leaked: %s", recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response["spec"].(map[string]any)["containerdRuntime"]; got != "containerd" {
		t.Fatalf("default runtime not canonicalized: %#v", got)
	}
	name := nodeOperationName(request.ClusterID, request.RequestID)
	secret, err := raw.CoreV1().Secrets(nodeOperationCredentialNamespace).Get(t.Context(), nodeOperationCredentialName(name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["ssh-password"]) != request.SSHPassword || secret.Annotations["station.rlark.io/operation-name"] != name || secret.Annotations["station.rlark.io/operation-namespace"] != "agent-a" {
		t.Fatalf("operation credential ownership or data invalid: %#v", secret.ObjectMeta)
	}
}

func TestCreateNodeOperationPersistsSelectedContainerdRuntime(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	request := nodeOperationTestRequest()
	request.ContainerdRuntime = "fleet"
	recorder := nodeOperationCall(&Gateway{stationClient: client, rawClient: fake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("k", 32))}, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response["spec"].(map[string]any)["containerdRuntime"]; got != "fleet" {
		t.Fatalf("runtime not persisted: %#v", got)
	}
	request.ContainerdRuntime = "custom"
	if got := nodeOperationCall(&Gateway{stationClient: client, rawClient: fake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("k", 32))}, request); got.Code != http.StatusBadRequest {
		t.Fatalf("unsupported runtime status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestCreateNodeOperationCanExplicitlyRequestStaleKubeadmResetOnlyForJoin(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	gateway := &Gateway{stationClient: client, rawClient: fake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("k", 32))}
	request := nodeOperationTestRequest()
	request.ResetExistingKubeadm = true
	recorder := nodeOperationCall(gateway, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("join status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	spec := response["spec"].(map[string]any)
	if spec["resetExistingKubeadm"] != true {
		t.Fatalf("reset intent was not persisted: %#v", spec)
	}
	request.Action = "Remove"
	request.NodeUID = "node-uid"
	if got := nodeOperationCall(gateway, request); got.Code != http.StatusBadRequest {
		t.Fatalf("remove with stale reset status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestCreateNodeOperationRejectsRequestIDReuse(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	gateway := &Gateway{stationClient: client, rawClient: fake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("k", 32))}
	request := nodeOperationTestRequest()
	if got := nodeOperationCall(gateway, request); got.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", got.Code, got.Body.String())
	}
	request.NodeName = "worker-b"
	if got := nodeOperationCall(gateway, request); got.Code != http.StatusConflict {
		t.Fatalf("reused request ID status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestCreateNodeOperationRejectsCredentialChangesOnReplay(t *testing.T) {
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"))
	gateway := &Gateway{stationClient: client, rawClient: fake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("k", 32))}
	request := nodeOperationTestRequest()
	if got := nodeOperationCall(gateway, request); got.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", got.Code, got.Body.String())
	}
	request.SSHPassword = "different-secret"
	if got := nodeOperationCall(gateway, request); got.Code != http.StatusConflict {
		t.Fatalf("credential replay status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestNodeOperationRouteRequiresAdministratorJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := []byte(strings.Repeat("k", 32))
	gateway := &Gateway{stationClient: stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a")), rawClient: fake.NewSimpleClientset(), jwtSigningKey: key}
	router := gin.New()
	gateway.RegisterRoutes(router)
	userToken, _, err := gateway.issueJWT("user", "user")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := gateway.issueJWT("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(nodeOperationTestRequest())
	for _, test := range []struct {
		name   string
		token  string
		status int
	}{{"missing token", "", http.StatusUnauthorized}, {"user role", userToken, http.StatusForbidden}, {"admin role", adminToken, http.StatusAccepted}} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/station/node-operations", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
}

func TestEnsureNodeOperationCredentialRejectsForeignSecret(t *testing.T) {
	op := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation", "metadata": map[string]any{"name": "op", "namespace": "agent", "uid": "current-uid"}}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: nodeOperationCredentialNamespace, Annotations: map[string]string{"station.rlark.io/operation-name": "op", "station.rlark.io/operation-namespace": "agent", "station.rlark.io/operation-uid": "old-uid"}}}
	if nodeOperationCredentialMatches(foreign, op) {
		t.Fatal("accepted Secret bound to a different NodeOperation UID")
	}
}

func TestSafeNodeOperationRedactsCredentialReferenceWithoutMutatingSource(t *testing.T) {
	op := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation",
		"metadata": map[string]any{"name": "op", "namespace": "agent", "uid": "uid"},
		"spec":     map[string]any{"action": "Join", "credentialSecret": "secret-name", "nodeName": "worker"},
	}}
	response := safeNodeOperation(op)
	spec := response["spec"].(map[string]any)
	if _, exists := spec["credentialSecret"]; exists {
		t.Fatal("credential Secret reference was returned")
	}
	if _, exists := op.Object["spec"].(map[string]any)["credentialSecret"]; !exists {
		t.Fatal("response normalization mutated the source object")
	}
}
