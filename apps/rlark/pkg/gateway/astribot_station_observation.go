package gateway

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

func (g *Gateway) registerStationObservation(admin *gin.RouterGroup) {
	routes := stations.ObservationRoutes{
		StationClient:    g.stationClient,
		Clusters:         stationClusterGVR,
		Enrollments:      networkEnrollmentGVR,
		ClusterNamespace: g.clusterNamespace,
		ReadMember:       g.readStationMember,
		RespondKubeError: respondKubeError,
		RespondError:     respondStationError,
	}
	routes.Register(admin)
}

func (g *Gateway) readStationMember(ctx context.Context, agentID, path string) (*http.Response, error) {
	return g.proxyKubeRequest(ctx, http.MethodGet, agentID, path, nil)
}

func respondStationError(c *gin.Context, status int, kind stations.ErrorKind, message string) {
	codes := [...]string{
		stations.InvalidQuery:        CodeInvalidQueryParameter,
		stations.Conflict:            CodeResourceConflict,
		stations.NotFound:            CodeResourceNotFound,
		stations.UpstreamUnavailable: CodeUpstreamUnavailable,
		stations.InvalidRequest:      CodeInvalidRequest,
	}
	respondError(c, status, codes[kind], message)
}
