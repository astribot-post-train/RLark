package gateway

import (
	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

func (g *Gateway) registerStationCameraRoutes(nodes gin.IRouter) {
	routes := stations.CameraRoutes{
		ReadMember:   g.readStationMember,
		RespondError: respondStationError,
		AuthorizeNode: func(c *gin.Context, namespace, name string) bool {
			if !g.authorizeTenantObjectByName(c, "nodes", namespace, name) {
				return false
			}
			if _, err := g.accessors["nodes"].get(c.Request.Context(), namespace, name, metav1.GetOptions{}); err != nil {
				respondKubeError(c, err.Error(), err)
				return false
			}
			return true
		},
	}
	routes.Register(nodes, requireOperator())
}
