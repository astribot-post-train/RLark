package gateway

import (
	"github.com/gin-gonic/gin"

	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

func (g *Gateway) discoveryScanHandler() gin.HandlerFunc {
	routes := stations.DiscoveryScanRoutes{
		StationClient: g.stationClient,
		Enrollments:   networkEnrollmentGVR,
		SiteNetworks:  siteNetworkGVR,
		SiteNetwork:   g.siteNetwork,
		DecodeRequest: func(c *gin.Context, into any) bool {
			return decodeOneJSON(c, maxNodeAdmissionRequestBytes, into, false)
		},
		Actor: func(c *gin.Context) string {
			return c.GetString(authSubjectKey)
		},
		RespondError:     respondStationError,
		RespondKubeError: respondKubeError,
	}
	return routes.Handle
}
