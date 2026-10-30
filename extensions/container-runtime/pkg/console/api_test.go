package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var testKey = []byte("only-a-test-key-with-at-least-32-bytes")

func token(t *testing.T, c claims) string {
	t.Helper()
	v, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(testKey)
	require.NoError(t, err)
	return v
}
func adminToken(t *testing.T) string {
	return token(t, claims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
}
func request(t *testing.T, a http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken(t))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func TestGatewayAuthentication(t *testing.T) {
	_, err := NewGatewayJWT([]byte("short"))
	require.Error(t, err)
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	base := claims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	tests := []struct {
		name   string
		modify func(*claims)
		want   error
	}{
		{"admin", func(*claims) {}, nil},
		{"user", func(c *claims) { c.Role = "user" }, errForbidden},
		{"no expiry", func(c *claims) { c.ExpiresAt = nil }, errUnauthorized},
		{"expired", func(c *claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }, errUnauthorized},
		{"foreign issuer", func(c *claims) { c.Issuer = "foreign" }, errUnauthorized},
		{"no subject", func(c *claims) { c.Subject = "" }, errUnauthorized},
		{"future issued", func(c *claims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) }, errUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.modify(&c)
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+token(t, c))
			_, err := auth.Administrator(r)
			require.ErrorIs(t, err, tt.want)
		})
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "rlark_access_token", Value: adminToken(t)})
	r.Header.Set("X-Role", "admin")
	_, err = auth.Administrator(r)
	require.ErrorIs(t, err, errUnauthorized)
	bad, err := jwt.NewWithClaims(jwt.SigningMethodHS384, base).SignedString(testKey)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+bad)
	_, err = auth.Administrator(r)
	require.ErrorIs(t, err, errUnauthorized)
}
func metadata(name, ns, uid string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Generation: 1, ResourceVersion: "1", Labels: map[string]string{tenant.IDLabel: "team-a"}}
}
func trueCondition(typ string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Accepted", LastTransitionTime: metav1.Now()}
}
func fixture(t *testing.T) (*API, *ext.ContainerCluster, *ext.ContainerNode, *ext.AgentRelease) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, ext.AddToScheme(s))
	require.NoError(t, native.AddToScheme(s))
	nc := &native.Cluster{ObjectMeta: metadata("cloud", "", "native-cluster"), Spec: native.ClusterSpec{AgentNamespace: "cloud-agents"}}
	cc := &ext.ContainerCluster{ObjectMeta: metadata("cloud", "cloud-agents", "companion-cluster"), Spec: ext.ContainerClusterSpec{Cluster: ext.NativeObjectReference{Name: nc.Name, UID: string(nc.UID)}, AgentNamespace: "cloud-agents", Runtime: "Raw"}}
	nn := &native.Node{ObjectMeta: metadata("node-1", "cloud-agents", "native-node"), Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	cn := &ext.ContainerNode{ObjectMeta: metadata("node-1", "cloud-agents", "companion-node"), Spec: ext.ContainerNodeSpec{Cluster: cc.Spec.Cluster, Node: ext.NativeObjectReference{Name: nn.Name, Namespace: nn.Namespace, UID: string(nn.UID)}, Runtime: "Raw", InstanceID: "instance-1", PrivateAddress: "10.0.0.2", AuthorizedGPUUUIDs: []string{"GPU-1"}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, AgentCredentialID: "credential", Schedulable: true, LastHeartbeatTime: &metav1.Time{Time: time.Now()}, Conditions: []metav1.Condition{trueCondition("Ready")}}}
	cn.Annotations = map[string]string{ext.AgentCredentialAnnotation: "credential"}
	release := &ext.AgentRelease{ObjectMeta: metadata("v2", "cloud-agents", "release-v2"), Spec: ext.AgentReleaseSpec{Version: "v2", OS: "linux", Architecture: "amd64", Protocol: "v1", SHA256: strings.Repeat("a", 64), ArtifactPath: "releases/v2"}, Status: ext.AgentReleaseStatus{ObservedGeneration: 1, Available: true, Conditions: []metav1.Condition{trueCondition("Available")}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(nc, cc, nn, cn, release).Build()
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	return &API{Client: c, Auth: auth}, cc, cn, release
}
func getDetail(t *testing.T, a *API) *clusterDetailResponse {
	t.Helper()
	w := request(t, a, "GET", prefix+"clusters/cloud-agents/cloud", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	var d clusterDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
	return &d
}
func TestInventoryFencesAndTenantIsolation(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(*testing.T, *API, *ext.ContainerNode)
		ready, sched bool
	}{
		{"fresh", func(*testing.T, *API, *ext.ContainerNode) {}, true, true},
		{"stale", func(t *testing.T, a *API, n *ext.ContainerNode) {
			a.Now = func() time.Time { return n.Status.LastHeartbeatTime.Add(time.Minute) }
		}, false, false},
		{"future heartbeat", func(t *testing.T, a *API, n *ext.ContainerNode) {
			a.Now = func() time.Time { return n.Status.LastHeartbeatTime.Add(-time.Minute) }
		}, false, false},
		{"credential revoked", func(t *testing.T, a *API, n *ext.ContainerNode) {
			require.NoError(t, a.Client.Patch(context.Background(), n, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+ext.AgentCredentialAnnotation+`":"replacement"}}}`))))
		}, false, false},
		{"stale generation", func(t *testing.T, a *API, n *ext.ContainerNode) {
			n.Generation = 2
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), n))
		}, false, false},
		{"replaced native node", func(t *testing.T, a *API, n *ext.ContainerNode) {
			var nn native.Node
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
			require.NoError(t, a.Client.(client.Client).Delete(context.Background(), &nn))
			nn.UID = "replacement"
			nn.ResourceVersion = ""
			require.NoError(t, a.Client.(client.Client).Create(context.Background(), &nn))
		}, false, false},
		{"foreign lock fences", func(t *testing.T, a *API, n *ext.ContainerNode) {
			v := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: n.Namespace, Labels: map[string]string{tenant.IDLabel: "other"}}, Spec: ext.ContainerAllocationSpec{Node: n.Spec.Node, NodeRecordUID: string(n.UID), TenantID: "other"}}
			require.NoError(t, a.Client.(client.Client).Create(context.Background(), v))
		}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, _, n, _ := fixture(t)
			test.mutate(t, a, n)
			d := getDetail(t, a)
			require.Len(t, d.Nodes, 1)
			require.Equal(t, test.ready, d.Nodes[0].Ready)
			require.Equal(t, test.sched, d.Nodes[0].Schedulable)
			if test.name == "foreign lock fences" {
				require.Empty(t, d.Allocations)
			}
		})
	}
	a, _, n, r := fixture(t)
	n.Name = "foreign"
	n.UID = "foreign"
	n.ResourceVersion = ""
	n.Labels[tenant.IDLabel] = "other"
	require.NoError(t, a.Client.(client.Client).Create(context.Background(), n))
	r.Name = "foreign"
	r.UID = "foreign"
	r.ResourceVersion = ""
	r.Labels[tenant.IDLabel] = "other"
	require.NoError(t, a.Client.(client.Client).Create(context.Background(), r))
	d := getDetail(t, a)
	require.Len(t, d.Nodes, 1)
	require.Len(t, d.Releases, 1)
}

type listFailure struct{ Management }

func (c listFailure) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*ext.ContainerAllocationList); ok {
		return errors.New("sensitive transport detail")
	}
	return c.Management.List(ctx, list, opts...)
}
func TestUnavailableObservationDoesNotBecomeZero(t *testing.T) {
	a, _, _, _ := fixture(t)
	a.Client = listFailure{a.Client}
	w := request(t, a, "GET", prefix+"clusters", "")
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "sensitive")
	require.NotContains(t, w.Body.String(), "nodeCount")
}
func TestTargetPatchPreconditions(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*patchClusterRequest)
		mutate func(*testing.T, *API, *ext.ContainerCluster, *ext.AgentRelease)
		want   int
	}{
		{"success", func(*patchClusterRequest) {}, nil, 200},
		{"stale rv", func(r *patchClusterRequest) { r.ResourceVersion = "0" }, nil, 409},
		{"replaced cluster", func(r *patchClusterRequest) { r.UID = "replacement" }, nil, 409},
		{"replaced release", func(r *patchClusterRequest) { r.ReleaseUID = "replacement" }, nil, 409},
		{"missing release uid", func(r *patchClusterRequest) { r.ReleaseUID = "" }, nil, 400},
		{"wrong tenant", func(*patchClusterRequest) {}, func(t *testing.T, a *API, c *ext.ContainerCluster, r *ext.AgentRelease) {
			r.Labels[tenant.IDLabel] = "other"
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), r))
		}, 409},
		{"stale release", func(*patchClusterRequest) {}, func(t *testing.T, a *API, c *ext.ContainerCluster, r *ext.AgentRelease) {
			r.Generation = 2
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), r))
		}, 409},
		{"invalid native binding", func(*patchClusterRequest) {}, func(t *testing.T, a *API, c *ext.ContainerCluster, r *ext.AgentRelease) {
			c.Spec.Cluster.UID = "old"
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), c))
		}, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, c, _, release := fixture(t)
			target := "v2"
			req := patchClusterRequest{UID: string(c.UID), ResourceVersion: c.ResourceVersion, TargetAgentRelease: &target, ReleaseUID: string(release.UID)}
			test.change(&req)
			if test.mutate != nil {
				test.mutate(t, a, c, release)
			}
			data, err := json.Marshal(req)
			require.NoError(t, err)
			w := request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud", string(data))
			require.Equal(t, test.want, w.Code, w.Body.String())
			var got ext.ContainerCluster
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(c), &got))
			if test.want == 200 {
				require.Equal(t, "v2", got.Spec.TargetAgentRelease)
			} else {
				require.Empty(t, got.Spec.TargetAgentRelease)
			}
		})
	}
}
func TestStrictRequestAndAuthenticationBoundary(t *testing.T) {
	a, _, _, _ := fixture(t)
	for _, body := range []string{`{`, `{} {}`, `{"unknown":true}`, strings.Repeat("x", 65537), `null`} {
		w := request(t, a, "POST", prefix+"clusters", body)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", prefix+"clusters", nil))
	require.Equal(t, 401, w.Code)
	r := httptest.NewRequest("PATCH", prefix+"clusters/cloud-agents/cloud", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+adminToken(t))
	r.Header.Set("Origin", "https://foreign.example")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	require.Equal(t, 403, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.NotEmpty(t, w.Header().Get("X-Request-ID"))
}

type uncertainPatch struct {
	Management
	lost bool
}

func (c *uncertainPatch) Patch(ctx context.Context, o client.Object, p client.Patch, opts ...client.PatchOption) error {
	if err := c.Management.Patch(ctx, o, p, opts...); err != nil {
		return err
	}
	if !c.lost {
		c.lost = true
		return errors.New("committed response lost")
	}
	return nil
}
func TestTargetResponseLossRecoversThroughFreshRead(t *testing.T) {
	a, cc, _, release := fixture(t)
	uncertain := &uncertainPatch{Management: a.Client}
	a.Client = uncertain
	target := "v2"
	body, err := json.Marshal(patchClusterRequest{UID: string(cc.UID), ResourceVersion: cc.ResourceVersion, TargetAgentRelease: &target, ReleaseUID: string(release.UID)})
	require.NoError(t, err)
	w := request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud", string(body))
	require.Equal(t, 503, w.Code)
	// The committed intent is durable; an old resourceVersion must not overwrite it.
	w = request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud", string(body))
	require.Equal(t, 409, w.Code)
	got := getDetail(t, a)
	require.Equal(t, "v2", got.Cluster.TargetAgentRelease)
	require.NotEqual(t, cc.ResourceVersion, got.Cluster.ResourceVersion)
}

func TestCreateAPIRecoversExactRetainedOperation(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, native.AddToScheme, ext.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	management := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		o.SetUID(types.UID(uuid.NewString()))
		return c.Create(ctx, o, opts...)
	}})
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	a := &API{Client: management, Creator: provision.Service{Client: management}, Auth: auth}
	req := provision.ClusterRequest{OperationID: uuid.NewString(), Name: "new-cloud", AgentNamespace: "new-cloud-agents", TenantID: "team-a", Runtime: "Raw"}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	first := request(t, a, "POST", prefix+"clusters", string(data))
	require.Equal(t, 201, first.Code, first.Body.String())
	// Treat the first response as lost and repeat the exact body.
	again := request(t, a, "POST", prefix+"clusters", string(data))
	require.Equal(t, 201, again.Code, again.Body.String())
	require.JSONEq(t, first.Body.String(), again.Body.String())
	req.OperationID = uuid.NewString()
	data, err = json.Marshal(req)
	require.NoError(t, err)
	conflict := request(t, a, "POST", prefix+"clusters", string(data))
	require.Equal(t, 409, conflict.Code)
	var clusters ext.ContainerClusterList
	require.NoError(t, management.List(context.Background(), &clusters))
	require.Len(t, clusters.Items, 1)
}
