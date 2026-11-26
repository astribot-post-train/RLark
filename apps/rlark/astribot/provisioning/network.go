// Package provisioning implements Astribot member setup policies.
package provisioning

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OriginName is the global name of the registry every site's registry is a copy of: a new site pulls from it
// through the tunnel until it has a cache of its own (site-registry, role=cache).
const OriginName = "origin.rlark.internal"

// NetworkNode is what a cluster's provision reads of a node the SiteNetwork declares.
type NetworkNode struct {
	ClusterID string   `json:"clusterID"`
	NodeName  string   `json:"nodeName"`
	Address   string   `json:"address"`
	Advertise []string `json:"-"`
}

// Network is the one SiteNetwork, read for a cluster's provision. It is changed as the object it is, so
// that whatever else it holds stays as it is.
type Network struct{ Object *unstructured.Unstructured }

// Entries returns the object entries at the supplied field path.
func (n Network) Entries(path ...string) []map[string]interface{} {
	raw, _, _ := unstructured.NestedSlice(n.Object.Object, path...)
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		if entry, ok := item.(map[string]interface{}); ok {
			out = append(out, entry)
		}
	}
	return out
}

// Nodes reads the node identities, addresses and advertised prefixes.
func (n Network) Nodes() []NetworkNode {
	var out []NetworkNode
	for _, entry := range n.Entries("spec", "nodes") {
		node := NetworkNode{}
		node.ClusterID, _ = entry["clusterID"].(string)
		node.NodeName, _ = entry["nodeName"].(string)
		node.Address, _ = entry["address"].(string)
		advertise, _, _ := unstructured.NestedSlice(entry, "advertise")
		for _, raw := range advertise {
			if a, ok := raw.(map[string]interface{}); ok {
				if cidr, _ := a["cidr"].(string); cidr != "" {
					node.Advertise = append(node.Advertise, cidr)
				}
			}
		}
		out = append(out, node)
	}
	return out
}

// NameAddress is the address the global names give name, if any.
func (n Network) NameAddress(name string) string {
	for _, entry := range n.Entries("spec", "names") {
		if entry["name"] == name {
			address, _ := entry["address"].(string)
			return address
		}
	}
	return ""
}

// SiteOf is the site that has cluster, if any.
func (n Network) SiteOf(cluster string) string {
	for _, entry := range n.Entries("spec", "sites") {
		clusters, _, _ := unstructured.NestedStringSlice(entry, "clusters")
		if slices.Contains(clusters, cluster) {
			name, _ := entry["name"].(string)
			return name
		}
	}
	return ""
}

// HasSite reports whether the network declares the site.
func (n Network) HasSite(site string) bool {
	return slices.ContainsFunc(n.Entries("spec", "sites"), func(entry map[string]interface{}) bool { return entry["name"] == site })
}

// Declared is the first tunnel document station-controller published for a node declared with its key, until
// the node is enrolled.
func (n Network) Declared(cluster, node string) string {
	for _, entry := range n.Entries("status", "declared") {
		if entry["clusterID"] == cluster && entry["nodeName"] == node {
			config, _ := entry["config"].(string)
			return config
		}
	}
	return ""
}

// AllocateAddress is the lowest free tunnel address of the pool that neither the SiteNetwork nor others (the node
// admissions', other makings') hold. The pool's first address is the management machine's.
func (n Network) AllocateAddress(others []string) (string, error) {
	pool, _, _ := unstructured.NestedString(n.Object.Object, "spec", "pool")
	prefix, err := netip.ParsePrefix(pool)
	if err != nil {
		return "", fmt.Errorf("the SiteNetwork's pool %q: %w", pool, err)
	}
	taken := map[string]bool{}
	for _, node := range n.Nodes() {
		taken[node.Address] = true
	}
	for _, entry := range n.Entries("spec", "names") {
		address, _ := entry["address"].(string)
		taken[address] = true
	}
	for _, address := range others {
		taken[address] = true
	}
	prefix = prefix.Masked()
	address := prefix.Addr().Next().Next()
	for ; prefix.Contains(address) && prefix.Contains(address.Next()); address = address.Next() {
		if !taken[address.String()] {
			return address.String(), nil
		}
	}
	return "", fmt.Errorf("the SiteNetwork's pool %s has no free address", pool)
}

// ChooseSubnets is the first pair of /16 Pod and Service subnets, from 10.200.0.0 on, that overlaps none of taken.
func ChooseSubnets(taken []netip.Prefix) (string, string, error) {
	for second := 200; second+1 < 256; second += 2 {
		pod := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(second), 0, 0}), 16)
		service := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(second + 1), 0, 0}), 16)
		if !slices.ContainsFunc(taken, pod.Overlaps) && !slices.ContainsFunc(taken, service.Overlaps) {
			return pod.String(), service.String(), nil
		}
	}
	return "", "", errors.New("no pair of /16 in 10.200.0.0–10.255.255.255 is free of every cluster's and the machine's networks")
}

// HubEntry is the SiteNetwork's declaration of a hub by its key: a machine that reaches Station API only through its
// tunnel. It listens on its tunnel endpoint's port; other sites dial the endpoint, and the machines of its own site
// its private address at that port (a cloud's machines behind one address do not reach each other at it).
func HubEntry(cluster, node, tunnelAddress, privateAddress, publicKey, endpoint string) map[string]interface{} {
	_, port, _ := net.SplitHostPort(endpoint)
	listen, _ := strconv.ParseInt(port, 10, 32)
	return map[string]interface{}{
		"clusterID": cluster, "nodeName": node, "role": "Hub", "address": tunnelAddress,
		"nodeAddress": privateAddress, "publicKey": publicKey, "listenPort": listen,
		"endpoints": []interface{}{
			map[string]interface{}{"address": endpoint, "scope": "Public"},
			map[string]interface{}{"address": net.JoinHostPort(privateAddress, port), "scope": "Site"},
		},
	}
}

// ValidTunnelEndpoint is an IPv4 ip:port with a port, as a SiteNetwork endpoint is.
func ValidTunnelEndpoint(endpoint string) bool {
	parsed, err := netip.ParseAddrPort(endpoint)
	return err == nil && parsed.Addr().Is4() && parsed.Port() != 0
}

// WithHub is nodes with the hub's entry in place of an earlier one of the same node.
func WithHub(nodes []interface{}, hub map[string]interface{}) []interface{} {
	i := slices.IndexFunc(nodes, func(raw interface{}) bool {
		entry, _ := raw.(map[string]interface{})
		return entry["clusterID"] == hub["clusterID"] && entry["nodeName"] == hub["nodeName"]
	})
	if i < 0 {
		return append(nodes, hub)
	}
	nodes[i] = hub
	return nodes
}
