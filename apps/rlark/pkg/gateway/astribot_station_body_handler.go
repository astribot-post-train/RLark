package gateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

//go:embed astribot_s1u_capabilities.json
var s1uCapabilities json.RawMessage

var s1uBodyVariants = func() map[string]bool {
	var profile struct {
		Variants []struct{ ID string }
	}
	if err := json.Unmarshal(s1uCapabilities, &profile); err != nil {
		panic(err) // A bundled capability profile is a build-time invariant.
	}
	variants := make(map[string]bool, len(profile.Variants))
	for _, variant := range profile.Variants {
		variants[variant.ID] = true
	}
	return variants
}()

func stationModelCapabilities(model string) json.RawMessage {
	if model == "s1u" {
		return s1uCapabilities
	}
	return nil
}

type bodyStateSample struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Age        *float64  `json:"ageSeconds,omitempty"`
	SourceAt   *float64  `json:"sourceUnixSeconds,omitempty"`
	Frame      string    `json:"frame,omitempty"`
	Position   []float64 `json:"position,omitempty"`
	Velocity   []float64 `json:"velocity,omitempty"`
	Quaternion []float64 `json:"quaternion,omitempty"`
}

type stationBodySnapshot struct {
	Version   int               `json:"version"`
	NodeUID   string            `json:"nodeUID"`
	Body      string            `json:"body"`
	Joints    []bodyStateSample `json:"joints"`
	Endpoints []bodyStateSample `json:"endpoints"`
}

func (g *Gateway) handleStationBodyState(c *gin.Context) {
	name, uid, clusterID := c.Param("name"), c.Query("node_uid"), c.Query("cluster_id")
	if len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsDNS1123Subdomain(clusterID)) > 0 || uid == "" {
		respondError(c, http.StatusBadRequest, CodeInvalidQueryParameter, "valid node name, cluster_id and node_uid are required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	observation := g.stationObservation()
	agentID, ok := observation.ResolveStation(c, name, uid, func(error) {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "Could not verify cluster ownership")
	})
	if !ok {
		return
	}
	var node corev1.Node
	if status, err := observation.ReadMemberObject(ctx, agentID, "/api/v1/nodes/"+url.PathEscape(name), &node); err != nil {
		if status == http.StatusNotFound {
			respondError(c, http.StatusNotFound, CodeResourceNotFound, "Node is absent from the member cluster")
		} else {
			respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "Member node is unavailable")
		}
		return
	}
	if node.Name != name || string(node.UID) != uid {
		respondError(c, http.StatusConflict, CodeResourceConflict, "Member node identity does not match the site node record")
		return
	}
	model, body := node.Labels["rlark.io/robot-model"], node.Labels["rlark.io/robot-body"]
	capabilities := stationModelCapabilities(model)
	c.Header("Cache-Control", "no-store")
	if capabilities == nil || !s1uBodyVariants[body] {
		c.JSON(http.StatusOK, gin.H{"state": "unsupported"})
		return
	}

	unavailable := func() {
		c.JSON(http.StatusOK, gin.H{"state": "unavailable", "body": body, "capabilities": capabilities})
	}
	upstreamError := func() {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "Body observer is unavailable")
	}

	// Only the installed model Addon may act as the source of body state.
	const daemonSet = "s1u-robot"
	var addon appsv1.DaemonSet
	addonPath := "/apis/apps/v1/namespaces/" + stations.RobotAddonNamespace + "/daemonsets/" + daemonSet
	if status, err := observation.ReadMemberObject(ctx, agentID, addonPath, &addon); err != nil {
		if status == http.StatusNotFound {
			unavailable()
		} else {
			upstreamError()
		}
		return
	}
	if addon.Name != daemonSet || addon.Namespace != stations.RobotAddonNamespace || addon.UID == "" {
		upstreamError()
		return
	}

	podsPath := "/api/v1/namespaces/" + stations.RobotAddonNamespace + "/pods"
	query := url.Values{"labelSelector": {stations.RobotAddonSelector}, "fieldSelector": {"spec.nodeName=" + name}}
	var pods corev1.PodList
	if _, err := observation.ReadMemberObject(ctx, agentID, podsPath+"?"+query.Encode(), &pods); err != nil {
		upstreamError()
		return
	}
	owned := func(pod corev1.Pod) bool {
		owner := metav1.GetControllerOf(&pod)
		return owner != nil && owner.Kind == "DaemonSet" && owner.Name == daemonSet && owner.UID == addon.UID &&
			pod.Namespace == stations.RobotAddonNamespace && pod.Spec.NodeName == name && pod.DeletionTimestamp == nil &&
			pod.Status.Phase == corev1.PodRunning
	}
	active := slices.IndexFunc(pods.Items, owned)
	if active < 0 {
		unavailable()
		return
	}
	pod := pods.Items[active]
	response, err := g.proxyKubeRequest(ctx, http.MethodGet, agentID,
		podsPath+"/"+url.PathEscape(pod.Name)+":"+stations.RobotCameraPort+"/proxy/body-state", nil)
	if err != nil {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "Body observer is unavailable")
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		unavailable()
		return
	}
	var snapshot stationBodySnapshot
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&snapshot) != nil ||
		snapshot.Version != 1 || snapshot.NodeUID != uid || snapshot.Body != body {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "Body observer returned an unavailable or mismatched snapshot")
		return
	}
	c.JSON(http.StatusOK, gin.H{"state": "available", "body": body, "capabilities": capabilities, "snapshot": snapshot})
}
