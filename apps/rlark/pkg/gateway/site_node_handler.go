package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	siteNodeGVR           = schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "nodes"}
	networkEnrollmentGVR  = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "networkenrollments"}
	siteNodeRobotLabel    = "rlark.io/robot-id"
	siteNodeWorkstationID = "rlark.io/workstation-id"
)

// siteNode is the read-only view a scheduler of work (a Fleet Run) needs of
// one machine: who it is, whether it is there, and the one address the other
// machines reach it at. It carries nothing about what runs on the machine.
type siteNode struct {
	ClusterID string `json:"cluster_id"`
	NodeName  string `json:"node_name"`
	// SiteNodeID is the Station object's name: stable across reinstalls.
	SiteNodeID string `json:"site_node_id"`
	Sequence   int64  `json:"sequence"`
	// BindingVersion increases when an administrator rebinds the identity to
	// a reinstalled machine; a consumer that cached it knows to look again.
	BindingVersion int64  `json:"binding_version"`
	Phase          string `json:"phase"`
	// Online is the heartbeat alone: the node's agent reported within the
	// last 90 s. The three fields after it are separate facts; a consumer
	// that needs the node reachable checks Tunnel as well.
	Online bool `json:"online"`
	// Tunnel is "verified" when the node reaches every peer that is itself
	// online, "failed" when it does not, "unknown" when it reports no probes.
	Tunnel string `json:"tunnel"`
	// RLarkNode is "ready" when the RLark Node of the same name is Ready.
	RLarkNode    string `json:"rlark_node"`
	LastSeenTime string `json:"last_seen_time,omitempty"`
	// BootTime changes when the machine restarts, also when the restart was
	// too short to show as offline; Reboots counts the changes seen.
	BootTime      string            `json:"boot_time,omitempty"`
	Reboots       int64             `json:"reboots"`
	Labels        map[string]string `json:"labels,omitempty"`
	Network       *siteNodeNetwork  `json:"network,omitempty"`
	RobotID       string            `json:"robot_id,omitempty"`
	WorkstationID string            `json:"workstation_id,omitempty"`
}

type siteNodeNetwork struct {
	State string `json:"state"`
	Site  string `json:"site,omitempty"`
	Role  string `json:"role,omitempty"`
	// Address is the one address every other member reaches the node at.
	Address       string `json:"address,omitempty"`
	TunnelAddress string `json:"tunnel_address,omitempty"`
}

// handleGetSiteNode answers GET /api/v1/site-nodes/:name?cluster_id=. It is a
// lookup by Node name: callers keep the name in their own configuration and
// read everything else here.
func (g *Gateway) handleGetSiteNode(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	clusterID, nodeName := c.Query("cluster_id"), c.Param("name")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	ctx := c.Request.Context()
	stations, err := g.stationClient.Resource(stationRecordGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station API request failed"})
		return
	}
	var station *unstructured.Unstructured
	for i := range stations.Items {
		bound, _, _ := unstructured.NestedString(stations.Items[i].Object, "spec", "binding", "nodeName")
		retired, _, _ := unstructured.NestedBool(stations.Items[i].Object, "spec", "retired")
		if bound != nodeName || retired {
			continue
		}
		if station != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "more than one identity is bound to this node"})
			return
		}
		station = &stations.Items[i]
	}
	if station == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site node not found"})
		return
	}
	result := siteNode{ClusterID: clusterID, NodeName: nodeName, SiteNodeID: station.GetName()}
	result.Sequence, _, _ = unstructured.NestedInt64(station.Object, "spec", "sequence")
	result.BindingVersion, _, _ = unstructured.NestedInt64(station.Object, "spec", "binding", "version")
	result.Phase, _, _ = unstructured.NestedString(station.Object, "status", "phase")
	result.LastSeenTime, _, _ = unstructured.NestedString(station.Object, "status", "lastSeenTime")
	result.BootTime, _, _ = unstructured.NestedString(station.Object, "status", "bootTime")
	result.Reboots, _, _ = unstructured.NestedInt64(station.Object, "status", "reboots")
	result.Online = result.Phase == "Bound"
	result.Tunnel, result.RLarkNode = "unknown", "unknown"
	conditions, _, _ := unstructured.NestedSlice(station.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]interface{})
		status, _ := condition["status"].(string)
		switch condition["type"] {
		case "NetworkVerified":
			result.Tunnel = map[string]string{"True": "verified", "False": "failed"}[status]
		case "RLarkLinked":
			result.RLarkNode = map[string]string{"True": "ready", "False": "not-ready"}[status]
		}
	}
	if result.Tunnel == "" {
		result.Tunnel = "unknown"
	}
	if result.RLarkNode == "" {
		result.RLarkNode = "unknown"
	}

	enrollment, err := g.stationClient.Resource(networkEnrollmentGVR).Namespace(namespace).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "network enrollment query failed"})
		return
	}
	if err == nil {
		// An answer for an earlier binding describes a machine that is gone.
		if version, _, _ := unstructured.NestedInt64(enrollment.Object, "spec", "bindingVersion"); version == result.BindingVersion {
			status, _, _ := unstructured.NestedMap(enrollment.Object, "status")
			text := func(key string) string { value, _ := status[key].(string); return value }
			result.Network = &siteNodeNetwork{State: text("phase"), Site: text("site"), Role: text("role"), Address: text("address"), TunnelAddress: text("tunnelAddress")}
		}
	}
	node, err := g.stationClient.Resource(siteNodeGVR).Namespace(namespace).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "node query failed"})
		return
	}
	if err == nil {
		result.Labels = node.GetLabels()
		result.RobotID, result.WorkstationID = result.Labels[siteNodeRobotLabel], result.Labels[siteNodeWorkstationID]
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}
