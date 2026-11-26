package gateway

import (
	"encoding/json"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rlinf/rlark/apps/rlark/pkg/log"
)

func (g *Gateway) handleGetSiteNetwork(c *gin.Context) {
	network, err := g.siteNetwork(c.Request.Context())
	if err != nil {
		respondKubeError(c, "SiteNetwork request failed", err)
		return
	}
	if network.GetName() == "" {
		respondError(c, http.StatusNotFound, CodeResourceNotFound, "SiteNetwork not found")
		return
	}
	classes, err := g.stationClient.Resource(nodeClassGVR).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		respondKubeError(c, "NodeClass request failed", err)
		return
	}
	c.JSON(http.StatusOK, stationidentity.SiteNetworkViewOf(network, classes))
}

type updateSiteDiscoveryRequest struct {
	ResourceVersion string          `json:"resourceVersion"`
	Discovery       json.RawMessage `json:"discovery"`
}

func (g *Gateway) handleUpdateSiteDiscovery(c *gin.Context) {
	var request updateSiteDiscoveryRequest
	if !decodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, false) {
		return
	}
	if request.ResourceVersion == "" {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, "resourceVersion is required")
		return
	}
	if len(request.Discovery) == 0 {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, "discovery is required")
		return
	}
	site := c.Param("site")
	if len(validation.IsDNS1123Label(site)) != 0 || len(site) > 63 {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, "site must be a DNS label of at most 63 characters")
		return
	}
	var discovery *stationidentity.DiscoverySettings
	if len(request.Discovery) != 0 && strings.TrimSpace(string(request.Discovery)) != "null" {
		var value struct {
			Enabled             *bool                            `json:"enabled"`
			Ranges              []string                         `json:"ranges"`
			IntervalSeconds     *int64                           `json:"intervalSeconds"`
			From                *stationidentity.DiscoverySource `json:"from"`
			IncludeUnclassified bool                             `json:"includeUnclassified,omitempty"`
			SSHPorts            []int64                          `json:"sshPorts,omitempty"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(request.Discovery)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			respondError(c, http.StatusBadRequest, CodeInvalidRequestBody, "the request must be one JSON object of the documented fields")
			return
		}
		if value.Enabled == nil {
			respondError(c, http.StatusBadRequest, CodeInvalidRequest, "discovery.enabled is required")
			return
		}
		interval := int64(30)
		if value.IntervalSeconds != nil {
			interval = *value.IntervalSeconds
		}
		from := stationidentity.DiscoverySource{}
		if value.From != nil {
			from = *value.From
		}
		if value.From == nil {
			respondError(c, http.StatusBadRequest, CodeInvalidRequest, "from must name a Hub of the network")
			return
		}
		discovery = &stationidentity.DiscoverySettings{Enabled: *value.Enabled, Ranges: value.Ranges, IntervalSeconds: interval, From: from, IncludeUnclassified: value.IncludeUnclassified, SSHPorts: value.SSHPorts}
		if err := stationidentity.ValidateSiteDiscovery(discovery); err != nil {
			respondError(c, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
	}

	ctx := c.Request.Context()
	network, err := g.siteNetwork(ctx)
	if err != nil {
		respondKubeError(c, "SiteNetwork request failed", err)
		return
	}
	if network.GetName() == "" {
		respondError(c, http.StatusNotFound, CodeResourceNotFound, "SiteNetwork not found")
		return
	}
	if network.GetResourceVersion() != request.ResourceVersion {
		respondError(c, http.StatusConflict, CodeResourceConflict, "the SiteNetwork changed meanwhile; reload and try again")
		return
	}
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	found := false
	var previous *stationidentity.DiscoverySettings
	for i, raw := range sites {
		entry, _ := raw.(map[string]any)
		if entry["name"] != site {
			continue
		}
		found = true
		previous = stationidentity.SiteNetworkDiscovery(network, site)
		if discovery == nil {
			delete(entry, "discovery")
		} else {
			value := map[string]any{
				"enabled": discovery.Enabled, "ranges": stringInterfaces(discovery.Ranges),
				"intervalSeconds":     discovery.IntervalSeconds,
				"from":                map[string]any{"clusterID": discovery.From.ClusterID, "nodeName": discovery.From.NodeName},
				"includeUnclassified": discovery.IncludeUnclassified,
			}
			if len(discovery.SSHPorts) > 0 {
				ports := make([]interface{}, len(discovery.SSHPorts))
				for i, port := range discovery.SSHPorts {
					ports[i] = port
				}
				value["sshPorts"] = ports
			}
			if err := stationidentity.ValidateSiteDiscoveryMember(network, discovery.From); err != nil {
				respondError(c, http.StatusBadRequest, CodeInvalidRequest, err.Error())
				return
			}
			if _, err := g.stationClient.Resource(stationClusterGVR).Get(ctx, discovery.From.ClusterID, metav1.GetOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					respondError(c, http.StatusBadRequest, CodeInvalidRequest, "from must name a Hub of the network")
				} else {
					respondKubeError(c, "Cluster request failed", err)
				}
				return
			}
			entry["discovery"] = value
		}
		sites[i] = entry
		break
	}
	if !found {
		respondError(c, http.StatusNotFound, CodeResourceNotFound, "site not found")
		return
	}
	if err := unstructured.SetNestedSlice(network.Object, sites, "spec", "sites"); err != nil {
		respondError(c, http.StatusInternalServerError, CodeInternalError, "failed to update SiteNetwork")
		return
	}
	classes, err := g.stationClient.Resource(nodeClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		respondKubeError(c, "NodeClass request failed", err)
		return
	}
	updated, err := g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Update(ctx, network, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			respondError(c, http.StatusConflict, CodeResourceConflict, "the SiteNetwork changed meanwhile; reload and try again")
			return
		}
		respondKubeError(c, "SiteNetwork update failed", err)
		return
	}
	log.FromContext(ctx).WithName("audit").Info("site network discovery", "action", "update", "actor", c.GetString(authSubjectKey),
		"site", site, "beforeEnabled", discoveryEnabled(previous), "beforeRanges", discoveryRanges(previous),
		"beforeIntervalSeconds", discoveryInterval(previous), "beforeFrom", discoveryFrom(previous),
		"enabled", discovery != nil && discovery.Enabled, "ranges", discoveryRanges(discovery),
		"intervalSeconds", discoveryInterval(discovery), "from", discoveryFrom(discovery),
		"resourceVersion", request.ResourceVersion, "newResourceVersion", updated.GetResourceVersion())
	c.JSON(http.StatusOK, stationidentity.SiteNetworkViewOf(updated, classes))
}

func stringInterfaces(values []string) []any {
	result := make([]any, len(values))
	for i := range values {
		result[i] = values[i]
	}
	return result
}

func discoveryRanges(discovery *stationidentity.DiscoverySettings) []string {
	if discovery == nil {
		return nil
	}
	return discovery.Ranges
}

func discoveryEnabled(discovery *stationidentity.DiscoverySettings) bool {
	return discovery != nil && discovery.Enabled
}

func discoveryInterval(discovery *stationidentity.DiscoverySettings) int64 {
	if discovery == nil {
		return 0
	}
	return discovery.IntervalSeconds
}

func discoveryFrom(discovery *stationidentity.DiscoverySettings) string {
	if discovery == nil {
		return ""
	}
	return discovery.From.ClusterID + "/" + discovery.From.NodeName
}
