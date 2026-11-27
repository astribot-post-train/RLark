package stationidentity

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DiscoverySource identifies the Hub running discovery.
type DiscoverySource struct {
	ClusterID string `json:"clusterID"`
	NodeName  string `json:"nodeName"`
}

// DiscoverySettings describes discovery configured for one site.
type DiscoverySettings struct {
	Enabled             bool            `json:"enabled"`
	Ranges              []string        `json:"ranges"`
	IntervalSeconds     int64           `json:"intervalSeconds"`
	From                DiscoverySource `json:"from"`
	IncludeUnclassified bool            `json:"includeUnclassified,omitempty"`
	SSHPorts            []int64         `json:"sshPorts,omitempty"`
}

// SiteNetworkSite is the console view of a declared site.
type SiteNetworkSite struct {
	Name      string             `json:"name"`
	Clusters  []string           `json:"clusters"`
	Discovery *DiscoverySettings `json:"discovery"`
}

// SiteNetworkView is the console overview of a SiteNetwork.
type SiteNetworkView struct {
	ResourceVersion string            `json:"resourceVersion"`
	Generation      int64             `json:"generation"`
	Sites           []SiteNetworkSite `json:"sites"`
	Members         []map[string]any  `json:"members"`
	NodeClasses     []map[string]any  `json:"nodeClasses"`
	Status          map[string]any    `json:"status"`
}

// SiteNetworkDiscovery reads discovery settings for the named site.
func SiteNetworkDiscovery(network *unstructured.Unstructured, site string) *DiscoverySettings {
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	for _, raw := range sites {
		entry, _ := raw.(map[string]any)
		if entry["name"] != site {
			continue
		}
		discovery, ok := entry["discovery"].(map[string]any)
		if !ok || discovery == nil {
			return nil
		}
		view := &DiscoverySettings{}
		view.Enabled, _, _ = unstructured.NestedBool(discovery, "enabled")
		view.Ranges, _, _ = unstructured.NestedStringSlice(discovery, "ranges")
		view.IntervalSeconds, _, _ = unstructured.NestedInt64(discovery, "intervalSeconds")
		view.IncludeUnclassified, _, _ = unstructured.NestedBool(discovery, "includeUnclassified")
		ports, _, _ := unstructured.NestedSlice(discovery, "sshPorts")
		for _, rawPort := range ports {
			if port, ok := rawPort.(int64); ok {
				view.SSHPorts = append(view.SSHPorts, port)
			}
			if port, ok := rawPort.(int); ok {
				view.SSHPorts = append(view.SSHPorts, int64(port))
			}
		}
		view.From.ClusterID, _, _ = unstructured.NestedString(discovery, "from", "clusterID")
		view.From.NodeName, _, _ = unstructured.NestedString(discovery, "from", "nodeName")
		return view
	}
	return nil
}

// SiteNetworkViewOf builds the API view without mutating the inputs.
func SiteNetworkViewOf(network *unstructured.Unstructured, classes *unstructured.UnstructuredList) SiteNetworkView {
	view := SiteNetworkView{
		ResourceVersion: network.GetResourceVersion(),
		Generation:      network.GetGeneration(),
		Sites:           []SiteNetworkSite{},
		Members:         []map[string]any{},
		NodeClasses:     []map[string]any{},
		Status:          map[string]any{},
	}
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	for _, raw := range sites {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		clusters, _, _ := unstructured.NestedStringSlice(entry, "clusters")
		view.Sites = append(view.Sites, SiteNetworkSite{Name: name, Clusters: clusters, Discovery: SiteNetworkDiscovery(network, name)})
	}
	clusterSites := map[string]string{}
	for _, site := range view.Sites {
		for _, cluster := range site.Clusters {
			clusterSites[cluster] = site.Name
		}
	}
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, raw := range nodes {
		entry, _ := raw.(map[string]any)
		member := map[string]any{}
		for _, field := range []string{"clusterID", "nodeName", "role", "address", "site"} {
			if value, ok := entry[field]; ok {
				member[field] = value
			}
		}
		if member["site"] == nil {
			member["site"] = clusterSites[fmt.Sprint(entry["clusterID"])]
		}
		view.Members = append(view.Members, member)
	}
	for i := range classes.Items {
		class := &classes.Items[i]
		access, hasAccess, _ := unstructured.NestedMap(class.Object, "spec", "access")
		discovery, hasDiscovery, _ := unstructured.NestedMap(class.Object, "spec", "discovery")
		if !hasAccess || !hasDiscovery {
			continue
		}
		item := map[string]any{"name": class.GetName(), "hasAccess": true, "hasDiscovery": true}
		if port, found, _ := unstructured.NestedInt64(access, "entry", "port"); found {
			item["entryPort"] = port
		}
		fingerprints, _, _ := unstructured.NestedStringSlice(discovery, "hostKeyFingerprints")
		if fingerprints == nil {
			fingerprints = []string{}
		}
		item["hostKeyFingerprints"] = fingerprints
		view.NodeClasses = append(view.NodeClasses, item)
	}
	if status, found, _ := unstructured.NestedMap(network.Object, "status"); found {
		encoded, _ := json.Marshal(status)
		_ = json.Unmarshal(encoded, &view.Status)
		if view.Status == nil {
			view.Status = map[string]any{}
		}
		if declared, ok := view.Status["declared"].([]any); ok {
			for _, raw := range declared {
				entry, _ := raw.(map[string]any)
				delete(entry, "config")
			}
		}
	}
	// A SiteNetwork without status still answers with the arrays the console
	// expects, so a fresh site network does not read as a broken response.
	if _, ok := view.Status["conditions"]; !ok {
		view.Status["conditions"] = []any{}
	}
	if _, ok := view.Status["unreachable"]; !ok {
		view.Status["unreachable"] = []any{}
	}
	return view
}

// ValidateSiteDiscovery validates settings and canonicalizes the address ranges.
func ValidateSiteDiscovery(discovery *DiscoverySettings) error {
	if discovery == nil {
		return nil
	}
	if discovery.Ranges == nil || len(discovery.Ranges) < 1 || len(discovery.Ranges) > 8 {
		return fmt.Errorf("ranges must be 1 to 8 IPv4 prefixes of /22 or shorter")
	}
	seen := make(map[string]struct{}, len(discovery.Ranges))
	for i, raw := range discovery.Ranges {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 22 {
			return fmt.Errorf("ranges must be 1 to 8 IPv4 prefixes of /22 or shorter")
		}
		canonical := prefix.Masked().String()
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("ranges must be 1 to 8 IPv4 prefixes of /22 or shorter")
		}
		seen[canonical] = struct{}{}
		discovery.Ranges[i] = canonical
	}
	if discovery.IntervalSeconds < 10 || discovery.IntervalSeconds > 3600 {
		return fmt.Errorf("intervalSeconds must be between 10 and 3600")
	}
	if len(discovery.From.ClusterID) == 0 || len(discovery.From.ClusterID) > 128 ||
		len(discovery.From.NodeName) == 0 || len(discovery.From.NodeName) > 253 {
		return fmt.Errorf("from must name a Hub of the network")
	}
	if len(discovery.SSHPorts) > 4 {
		return fmt.Errorf("sshPorts must have at most 4 ports")
	}
	seenPorts := map[int64]bool{}
	for _, port := range discovery.SSHPorts {
		if port < 1 || port > 65535 || seenPorts[port] {
			return fmt.Errorf("sshPorts must contain unique ports between 1 and 65535")
		}
		seenPorts[port] = true
	}
	return nil
}

// ValidateSiteDiscoveryMember verifies that the source is a declared Hub.
func ValidateSiteDiscoveryMember(network *unstructured.Unstructured, from DiscoverySource) error {
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, raw := range nodes {
		entry, _ := raw.(map[string]any)
		if entry["clusterID"] == from.ClusterID && entry["nodeName"] == from.NodeName && entry["role"] == "Hub" {
			return nil
		}
	}
	return fmt.Errorf("from must name a Hub of the network")
}
