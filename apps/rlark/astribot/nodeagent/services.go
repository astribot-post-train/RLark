package nodeagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"path"

	"github.com/gin-gonic/gin"
)

// RegisterServiceRoutes adds the node’s Unix socket services to its local router.
func RegisterServiceRoutes(router gin.IRouter) {
	registerServiceRoutes(router, HostRun+"/rlark/services")
}

func registerServiceRoutes(router gin.IRouter, directory string) {
	router.Any("/api/astribot/services/:service/*path", func(ctx *gin.Context) {
		handleNodeService(ctx, directory)
	})
}

func handleNodeService(ctx *gin.Context, directory string) {
	socket := path.Join(directory, ctx.Param("service")+".sock")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.URL.Path, r.Out.URL.RawPath = "http", "localhost", ctx.Param("path"), ""
			r.Out.Host = "localhost"
		},
		Transport: &http.Transport{
			DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(c, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}
	proxy.ServeHTTP(ctx.Writer, ctx.Request)
}
