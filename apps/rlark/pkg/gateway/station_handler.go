package gateway

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	stationClusterGVR     = schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "clusters"}
	stationRecordGVR      = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "stations"}
	stationObservationGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "stationobservations"}
)

func (g *Gateway) handleListStations(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	limit := int64(200)
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 500"})
			return
		}
		limit = parsed
	}
	options := metav1.ListOptions{Limit: limit, Continue: c.Query("continue")}
	resource := g.stationClient.Resource(stationRecordGVR)
	var records *unstructured.UnstructuredList
	var err error
	if clusterID := c.Query("cluster_id"); clusterID != "" {
		namespace, err := g.stationNamespace(c, clusterID)
		if err != nil {
			return
		}
		records, err = resource.Namespace(namespace).List(c.Request.Context(), options)
	} else {
		records, err = resource.List(c.Request.Context(), options)
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station API request failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": records.Items, "continue": records.GetContinue()})
}

func (g *Gateway) handleGetStation(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	station, err := g.stationClient.Resource(stationRecordGVR).Namespace(namespace).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Station not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station API request failed"})
		return
	}
	c.JSON(http.StatusOK, station.Object)
}

func (g *Gateway) handleListStationObservations(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	limit := int64(200)
	if raw := c.Query("limit"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 1 || parsed > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 500"})
			return
		}
		limit = parsed
	}
	observations, err := g.stationClient.Resource(stationObservationGVR).Namespace(namespace).List(c.Request.Context(), metav1.ListOptions{
		Limit: limit, Continue: c.Query("continue"),
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station observation query failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": observations.Items, "continue": observations.GetContinue()})
}

func (g *Gateway) stationNamespace(c *gin.Context, clusterID string) (string, error) {
	clusters, err := g.stationClient.Resource(stationClusterGVR).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Cluster API request failed"})
		return "", err
	}
	var namespace string
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.GetName() != clusterID {
			continue
		}
		value, found, nestedErr := unstructured.NestedString(cluster.Object, "spec", "agentNamespace")
		if nestedErr != nil || !found || value == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "Cluster namespace mapping is invalid"})
			return "", apierrors.NewBadRequest("invalid Cluster namespace mapping")
		}
		if namespace != "" {
			c.JSON(http.StatusConflict, gin.H{"error": "Cluster namespace mapping is ambiguous"})
			return "", apierrors.NewConflict(stationClusterGVR.GroupResource(), clusterID, nil)
		}
		namespace = value
	}
	if namespace == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Cluster not found"})
		return "", apierrors.NewNotFound(stationClusterGVR.GroupResource(), clusterID)
	}
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.GetName() == clusterID {
			continue
		}
		value, found, nestedErr := unstructured.NestedString(cluster.Object, "spec", "agentNamespace")
		if nestedErr == nil && found && value == namespace {
			c.JSON(http.StatusConflict, gin.H{"error": "Cluster namespace mapping is ambiguous"})
			return "", apierrors.NewConflict(stationClusterGVR.GroupResource(), clusterID, nil)
		}
	}
	return namespace, nil
}
