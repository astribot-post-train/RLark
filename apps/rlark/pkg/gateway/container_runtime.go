package gateway

import (
	"errors"
	"net/http"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	management "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/console"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/submission"
	"github.com/gin-gonic/gin"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (g *Gateway) initContainerRuntime(cfg *rest.Config) error {
	if !g.config.ContainerRuntimeEnabled {
		if g.config.ContainerRuntimeJobSubmission || g.config.ContainerRuntimeNodeRegistration || g.config.ContainerRuntimeDiagnosticsOrigin != "" {
			return errors.New("container runtime options require --enable-container-runtime")
		}
		return nil
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{native.AddToScheme, ext.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	config := rest.CopyConfig(cfg)
	config.Timeout = 15 * time.Second
	// Uncached reads are required for UID, version and ownership fences.
	managementClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	api := &management.API{Client: managementClient, Creator: provision.Service{Client: managementClient}, Auth: management.GatewayIdentity{}}
	if g.config.ContainerRuntimeNodeRegistration {
		api.Registrar = provision.Service{Client: managementClient}
	}
	if g.config.ContainerRuntimeJobSubmission {
		api.Jobs = submission.Service{Client: managementClient}
	}
	if origin := g.config.ContainerRuntimeDiagnosticsOrigin; origin != "" {
		api.Logs, err = management.NewLogProxy(origin)
		if err != nil {
			return err
		}
		api.Stats, err = management.NewStatsProxy(origin)
		if err != nil {
			return err
		}
	}
	g.containerRuntime = api
	return nil
}

// Routes are explicit so no future extension route bypasses Gateway review,
// JWT authentication, or administrator authorization.
func (g *Gateway) registerContainerRuntimeRoutes(admin *gin.RouterGroup) {
	api := admin.Group("/container-runtime")
	api.GET("/clusters", g.serveContainerRuntime)
	api.POST("/clusters", g.serveContainerRuntime)
	api.GET("/clusters/:namespace/:name", g.serveContainerRuntime)
	api.PATCH("/clusters/:namespace/:name", g.serveContainerRuntime)
	api.POST("/clusters/:namespace/:name/nodes", g.serveContainerRuntime)
	api.PATCH("/clusters/:namespace/:name/nodes/:node", g.serveContainerRuntime)
	api.GET("/clusters/:namespace/:name/jobs", g.serveContainerRuntime)
	api.POST("/clusters/:namespace/:name/jobs", g.serveContainerRuntime)
	api.PATCH("/clusters/:namespace/:name/jobs/:job/suspend", g.serveContainerRuntime)
	api.GET("/logs", g.serveContainerRuntime)
	api.GET("/stats", g.serveContainerRuntime)
}

func (g *Gateway) serveContainerRuntime(c *gin.Context) {
	if g.containerRuntime == nil {
		respondError(c, http.StatusServiceUnavailable, CodeUpstreamUnavailable, "container runtime extension is disabled")
		return
	}
	// A typed context value is created only after the Gateway middlewares have
	// verified the JWT and admin role. No client header can supply this identity.
	r := management.WithGatewayAdministrator(c.Request, c.GetString(authSubjectKey))
	// Private diagnostic reads revalidate the same browser JWT. Gateway also
	// supports its normal cookie session; convert only an already verified token.
	if r.Header.Get("Authorization") == "" {
		if token, err := c.Cookie("rlark_access_token"); err == nil {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}
	g.containerRuntime.ServeHTTP(c.Writer, r)
}
