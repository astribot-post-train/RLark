package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type nodeNetworkSummaryClient interface {
	Read(context.Context, string, string) (stationidentity.NodeNetworkSummary, error)
}

func (g *Gateway) handleNodeNetwork(c *gin.Context) {
	clusterID, nodeName := c.Query("cluster_id"), c.Param("name")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	cluster, err := g.kubeClient.RlinfV1alpha1().Clusters().Get(c.Request.Context(), clusterID, metav1.GetOptions{})
	if err != nil || (cluster.Spec.Delisted && c.GetString(authRoleKey) != "admin") {
		c.JSON(http.StatusNotFound, gin.H{"error": "cluster not found"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	if _, err := g.kubeClient.RlinfV1alpha1().Nodes(namespace).Get(c.Request.Context(), nodeName, metav1.GetOptions{}); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	summary, err := g.networkSummary.Read(c.Request.Context(), clusterID, nodeName)
	if err != nil {
		if errors.Is(err, stationidentity.ErrNetworkCredentialRejected) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "network summary service authentication failed"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "network summary unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, summary)
}
