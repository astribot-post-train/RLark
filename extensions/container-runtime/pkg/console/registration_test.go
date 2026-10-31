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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const registrationPath = prefix + "clusters/cloud-agents/cloud/nodes"

func registrationBody(t *testing.T, req registerNodeRequest) string {
	t.Helper()
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return string(data)
}
func registrationFixture(t *testing.T) (*API, registerNodeRequest, client.WithWatch) {
	t.Helper()
	a, cc, _, _ := fixture(t)
	c := a.Client.(client.WithWatch)
	c = interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, base client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetUID(types.UID(uuid.NewString()))
		return base.Create(ctx, obj, opts...)
	}})
	a.Client = c
	a.Registrar = provision.Service{Client: c}
	return a, registerNodeRequest{OperationID: uuid.NewString(), ClusterUID: string(cc.UID), Name: "node-2", InstanceID: "verified-incarnation", PrivateAddress: "10.0.0.3", Environment: "prepared", NetworkScope: "cloud-private", AuthorizedGPUUUIDs: []string{"GPU-2"}}, c
}
func TestNodeRegistrationResumesPartialCreateAndLostResponse(t *testing.T) {
	a, req, base := registrationFixture(t)
	failCompanion := true
	management := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*ext.ContainerNode); ok && failCompanion {
			return errors.New("private upstream credential detail")
		}
		return c.Create(ctx, obj, opts...)
	}})
	a.Client = management
	a.Registrar = provision.Service{Client: management}
	require.True(t, getDetail(t, a).NodeRegistrationEnabled)
	body := registrationBody(t, req)
	first := request(t, a, http.MethodPost, registrationPath, body)
	require.Equal(t, 503, first.Code, first.Body.String())
	require.NotContains(t, first.Body.String(), "private upstream")
	var retained native.Node
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: req.Name}, &retained))
	nativeUID := retained.UID
	failCompanion = false
	done := request(t, a, http.MethodPost, registrationPath, body)
	require.Equal(t, 201, done.Code, done.Body.String())
	// The committed response may also be lost. An identical request recovers the
	// exact node pair, even after a user has cordoned the native node.
	retained.Spec.Unschedulable = true
	require.NoError(t, base.Update(context.Background(), &retained))
	retry := request(t, a, http.MethodPost, registrationPath, body)
	require.Equal(t, 201, retry.Code, retry.Body.String())
	require.JSONEq(t, done.Body.String(), retry.Body.String())
	var cn ext.ContainerNode
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: req.Name}, &cn))
	require.Equal(t, string(nativeUID), cn.Spec.Node.UID)
	require.Equal(t, req.OperationID, cn.Annotations[ext.AgentCredentialAnnotation])
	require.Empty(t, cn.Status.Conditions)
	require.Nil(t, cn.Status.LastHeartbeatTime)
	require.False(t, cn.Status.Schedulable)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(&retained), &retained))
	require.True(t, retained.Spec.Unschedulable)
	req.OperationID = uuid.NewString()
	conflict := request(t, a, http.MethodPost, registrationPath, registrationBody(t, req))
	require.Equal(t, 409, conflict.Code)
}
func TestNodeRegistrationRejectsInvalidForeignAndReplacedIdentity(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*testing.T, *API, *registerNodeRequest, client.Client)
		status int
	}{
		{"disabled", func(_ *testing.T, a *API, _ *registerNodeRequest, _ client.Client) { a.Registrar = nil }, 404},
		{"missing operation", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.OperationID = "" }, 400},
		{"missing cluster UID", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.ClusterUID = "" }, 400},
		{"stale companion UID", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.ClusterUID = "replaced" }, 409},
		{"public address", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.PrivateAddress = "8.8.8.8" }, 400},
		{"no GPU authorization", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.AuthorizedGPUUUIDs = nil }, 400},
		{"duplicate GPU request", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) {
			r.AuthorizedGPUUUIDs = []string{"GPU-2", "GPU-2"}
		}, 400},
		{"invalid name", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) { r.Name = "../node" }, 400},
		{"GPU already registered", func(_ *testing.T, _ *API, r *registerNodeRequest, _ client.Client) {
			r.AuthorizedGPUUUIDs = []string{"GPU-1"}
		}, 409},
		{"address already registered", func(t *testing.T, _ *API, r *registerNodeRequest, c client.Client) {
			var cn ext.ContainerNode
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: "node-1"}, &cn))
			cn.Spec.NetworkScope = r.NetworkScope
			require.NoError(t, c.Update(context.Background(), &cn))
			r.PrivateAddress = cn.Spec.PrivateAddress
		}, 409},
		{"foreign tenant", func(t *testing.T, _ *API, _ *registerNodeRequest, c client.Client) {
			var nc native.Cluster
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "cloud"}, &nc))
			nc.Labels[tenant.IDLabel] = "team-b"
			require.NoError(t, c.Update(context.Background(), &nc))
		}, 409},
		{"replaced native", func(t *testing.T, _ *API, _ *registerNodeRequest, c client.Client) {
			var nc native.Cluster
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "cloud"}, &nc))
			nc.UID = "replacement"
			require.NoError(t, c.Update(context.Background(), &nc))
		}, 409},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, req, c := registrationFixture(t)
			tt.change(t, a, &req, c)
			w := request(t, a, http.MethodPost, registrationPath, registrationBody(t, req))
			require.Equal(t, tt.status, w.Code, w.Body.String())
			var nodes native.NodeList
			require.NoError(t, c.List(context.Background(), &nodes))
			require.Len(t, nodes.Items, 1)
			var companions ext.ContainerNodeList
			require.NoError(t, c.List(context.Background(), &companions))
			require.Len(t, companions.Items, 1)
		})
	}
}

func TestNodeRegistrationProtocolRejectsSpoofedAuthorityAndNonAdmins(t *testing.T) {
	a, req, _ := registrationFixture(t)
	valid := registrationBody(t, req)
	for _, body := range []string{strings.TrimSuffix(valid, "}") + `,"tenantID":"team-b"}`, strings.TrimSuffix(valid, "}") + `,"cluster":"foreign"}`, valid + "{}", `null`, strings.Repeat(" ", 65536) + valid} {
		w := request(t, a, http.MethodPost, registrationPath, body)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	for _, role := range []string{"", "user"} {
		r := httptest.NewRequest(http.MethodPost, registrationPath, strings.NewReader(valid))
		if role != "" {
			r.Header.Set("Authorization", "Bearer "+token(t, claims{Role: role, RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "user", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}))
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if role == "" {
			require.Equal(t, 401, w.Code)
		} else {
			require.Equal(t, 403, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, registrationPath, strings.NewReader(valid))
	r.Header.Set("Authorization", "Bearer "+adminToken(t))
	r.Header.Set("Origin", "https://foreign.example")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	require.Equal(t, 403, w.Code)
}

func TestDockerNodeRegistrationUsesDockerNativeAgentType(t *testing.T) {
	a, req, c := registrationFixture(t)
	var cc ext.ContainerCluster
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: "cloud"}, &cc))
	cc.Spec.Runtime = "Docker"
	require.NoError(t, c.Update(context.Background(), &cc))
	w := request(t, a, http.MethodPost, registrationPath, registrationBody(t, req))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var cn ext.ContainerNode
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: req.Name}, &cn))
	require.Equal(t, ext.Runtime("Docker"), cn.Spec.Runtime)
	var nn native.Node
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: req.Name}, &nn))
	require.Equal(t, native.AgentTypeDocker, nn.Spec.AgentType)
}

// A companion replaced after the API read must also be rejected by the shared
// service. The extra precondition is optional for legacy administrator requests.
func TestNodeRegistrationServiceRechecksCompanionInstance(t *testing.T) {
	a, req, c := registrationFixture(t)
	a.Registrar = registrationFunc(func(ctx context.Context, r provision.NodeRequest) (*ext.ContainerNode, error) {
		require.Equal(t, "team-a", r.TenantID)
		require.Equal(t, "native-cluster", r.ClusterUID)
		require.Equal(t, req.ClusterUID, r.ContainerClusterUID)
		var cc ext.ContainerCluster
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "cloud-agents", Name: "cloud"}, &cc))
		cc.UID = "replacement"
		require.NoError(t, c.Update(ctx, &cc))
		return (provision.Service{Client: c}).RegisterNode(ctx, r)
	})
	w := request(t, a, http.MethodPost, registrationPath, registrationBody(t, req))
	require.Equal(t, 409, w.Code, w.Body.String())
	var nodes native.NodeList
	require.NoError(t, c.List(context.Background(), &nodes))
	require.Len(t, nodes.Items, 1)
}

type registrationFunc func(context.Context, provision.NodeRequest) (*ext.ContainerNode, error)

func (f registrationFunc) RegisterNode(ctx context.Context, r provision.NodeRequest) (*ext.ContainerNode, error) {
	return f(ctx, r)
}

func TestNodeRegistrationUnavailableBindingAndDeletingCluster(t *testing.T) {
	a, req, c := registrationFixture(t)
	var cc ext.ContainerCluster
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-agents", Name: "cloud"}, &cc))
	cc.Finalizers = []string{"test/retain"}
	require.NoError(t, c.Update(context.Background(), &cc))
	require.NoError(t, c.Delete(context.Background(), &cc))
	require.False(t, getDetail(t, a).NodeRegistrationEnabled)
	require.Equal(t, 409, request(t, a, http.MethodPost, registrationPath, registrationBody(t, req)).Code)
	a.Registrar = nil
	require.False(t, getDetail(t, a).NodeRegistrationEnabled)
}
