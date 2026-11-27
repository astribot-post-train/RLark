package gateway

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

func (g *Gateway) registerStationObservation(admin *gin.RouterGroup) {
	g.stationObservation().Register(admin)
}

// stationObservation is how the routes that read a station's member cluster reach it: its running images and
// its body state.
func (g *Gateway) stationObservation() stations.ObservationRoutes {
	return stations.ObservationRoutes{
		StationClient:    g.stationClient,
		Clusters:         stationClusterGVR,
		Enrollments:      networkEnrollmentGVR,
		ClusterNamespace: g.clusterNamespace,
		ReadMember:       g.readStationMember,
		RespondKubeError: respondKubeError,
		RespondError:     respondStationError,
	}
}

func (g *Gateway) readStationMember(ctx context.Context, agentID, path string) (*http.Response, error) {
	return g.proxyKubeRequest(ctx, http.MethodGet, agentID, path, nil)
}

func respondStationError(c *gin.Context, status int, kind stations.ErrorKind, message string) {
	respondError(c, status, stationErrorCode(kind), message)
}

func respondStationErrorWithDetails(c *gin.Context, status int, kind stations.ErrorKind, message string, details map[string]any) {
	respondErrorWithDetails(c, status, stationErrorCode(kind), message, details)
}

func stationErrorCode(kind stations.ErrorKind) string {
	codes := [...]string{
		stations.InvalidQuery:        CodeInvalidQueryParameter,
		stations.Conflict:            CodeResourceConflict,
		stations.NotFound:            CodeResourceNotFound,
		stations.UpstreamUnavailable: CodeUpstreamUnavailable,
		stations.InvalidRequest:      CodeInvalidRequest,
		stations.InvalidBody:         CodeInvalidRequestBody,
		stations.Forbidden:           CodeForbidden,
		stations.RobotBusy:           CodeRobotClaimBusy,
	}
	return codes[kind]
}
