package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	management "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/console"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rlinf/rlark/apps/rlark/pkg/configs"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestContainerRuntimeUsesGatewayAdministratorBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scheme := runtime.NewScheme()
	require.NoError(t, ext.AddToScheme(scheme))
	reads := 0
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		reads++
		return c.List(ctx, list, opts...)
	}})
	g := &Gateway{config: Config{KubeClientConfig: configs.DefaultKubernetesClientConfig()}, jwtSigningKey: []byte("gateway-test-key-at-least-32-bytes"), containerRuntime: &management.API{Client: reader, Auth: management.GatewayIdentity{}}}
	router := gin.New()
	g.RegisterRoutes(router)
	for _, method := range []string{"GET", "POST", "PATCH"} {
		path := "/api/v1/container-runtime/clusters"
		if method == "PATCH" {
			path += "/agents/cloud"
		}
		for _, role := range []string{"", "user", "admin"} {
			t.Run(method+"/"+role, func(t *testing.T) {
				r := httptest.NewRequest(method, path, strings.NewReader("{}"))
				r.Header.Set("X-Role", "admin")
				r.Header.Set("X-Subject", "forged-administrator")
				if role != "" {
					token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{Role: role, RegisteredClaims: jwt.RegisteredClaims{Subject: "alice", Issuer: "rlark-gateway", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString(g.jwtSigningKey)
					require.NoError(t, err)
					r.Header.Set("Authorization", "Bearer "+token)
				}
				prior := reads
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				if role != "admin" {
					want := 401
					if role == "user" {
						want = 403
					}
					require.Equal(t, want, w.Code)
					require.Equal(t, prior, reads)
				} else if method == "GET" {
					require.Equal(t, 200, w.Code, w.Body.String())
					require.JSONEq(t, `{"items":[]}`, w.Body.String())
				} else {
					require.NotEqual(t, 401, w.Code)
					require.NotEqual(t, 403, w.Code)
				}
			})
		}
	}
	// Core handlers reject the same forged headers without trusted middleware.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/container-runtime/clusters", nil)
	r.Header.Set("X-Role", "admin")
	g.containerRuntime.ServeHTTP(w, r)
	require.Equal(t, 401, w.Code)
}

func TestContainerRuntimeDisabledAndConfigurationValidation(t *testing.T) {
	g := &Gateway{}
	require.NoError(t, g.initContainerRuntime(nil))
	require.Nil(t, g.containerRuntime)
	g.config.ContainerRuntimeJobSubmission = true
	require.ErrorContains(t, g.initContainerRuntime(nil), "require --enable-container-runtime")
	g.config.ContainerRuntimeEnabled = true
	g.config.ContainerRuntimeDiagnosticsOrigin = "http://example.com"
	require.ErrorContains(t, g.initContainerRuntime(&rest.Config{Host: "https://127.0.0.1"}), "loopback")
	// No connection or background effects occur while constructing the handler.
	g.config.ContainerRuntimeDiagnosticsOrigin = "http://127.0.0.1:9446"
	require.NoError(t, g.initContainerRuntime(&rest.Config{Host: "https://127.0.0.1"}))
	require.NotNil(t, g.containerRuntime)
}

func TestContainerRuntimeDisabledReturnsUnavailableAfterAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &Gateway{jwtSigningKey: []byte("gateway-test-key-at-least-32-bytes")}
	router := gin.New()
	g.RegisterRoutes(router)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{Subject: "alice", Issuer: "rlark-gateway", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString(g.jwtSigningKey)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/container-runtime/clusters", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "container runtime extension is disabled")
}

func TestContainerRuntimeDiagnosticsPreserveIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	queries := make([]string, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		require.Equal(t, "Bearer verified", r.Header.Get("Authorization"))
		if r.URL.Path == management.LogPath {
			_, _ = w.Write([]byte(`{"data":"b2s=","truncated":false,"retentionLimitReached":false}`))
		} else {
			w.WriteHeader(http.StatusConflict)
		}
	}))
	defer upstream.Close()
	logs, err := management.NewLogProxy(upstream.URL)
	require.NoError(t, err)
	stats, err := management.NewStatsProxy(upstream.URL)
	require.NoError(t, err)
	g := &Gateway{containerRuntime: &management.API{Auth: management.GatewayIdentity{}, Logs: logs, Stats: stats}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	g.containerRuntime = &management.API{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Auth: management.GatewayIdentity{}, Logs: logs, Stats: stats}
	query := "?namespace=agents&allocation=lock&allocationUID=exact-allocation&taskUID=exact-task"
	for _, path := range []string{management.LogPath, management.StatsPath} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(authSubjectKey, "alice")
		c.Request = httptest.NewRequest(http.MethodGet, path+query, nil)
		c.Request.Header.Set("Authorization", "Bearer verified")
		g.serveContainerRuntime(c)
	}
	require.Equal(t, []string{query[1:], query[1:]}, queries)
}
