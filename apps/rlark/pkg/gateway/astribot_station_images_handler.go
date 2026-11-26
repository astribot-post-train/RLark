package gateway

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

func (g *Gateway) registerStationRunningImages(admin *gin.RouterGroup) {
	routes := stations.RunningImageRoutes{
		StationClient:    g.stationClient,
		Clusters:         stationClusterGVR,
		Enrollments:      networkEnrollmentGVR,
		ClusterNamespace: g.clusterNamespace,
		ReadMember: func(ctx context.Context, agentID, path string) (*http.Response, error) {
			return g.proxyKubeRequest(ctx, http.MethodGet, agentID, path, nil)
		},
		RespondKubeError: respondKubeError,
		RespondError: func(c *gin.Context, status int, kind stations.ErrorKind, message string) {
			codes := [...]string{
				stations.InvalidQuery:        CodeInvalidQueryParameter,
				stations.Conflict:            CodeResourceConflict,
				stations.NotFound:            CodeResourceNotFound,
				stations.UpstreamUnavailable: CodeUpstreamUnavailable,
			}
			respondError(c, status, codes[kind], message)
		},
	}
	routes.Register(admin)
}
