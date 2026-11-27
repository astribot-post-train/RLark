package stationidentity

import (
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AdmissionBootstrap supplies the verified Station API endpoints and artifact digest.
type AdmissionBootstrap interface {
	Address() string
	TunnelAddress() string
	Digest(context.Context, string) (string, error)
}

// NodeClassReader reads the NodeClass referenced by an admission.
type NodeClassReader interface {
	Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error)
}

// leafUplinkOf returns the device a machine of this admission stands
// behind, when its site declares one: the SiteNetwork's entry for the node
// names the site, or else the admission, or else the site of its cluster.
func leafUplinkOf(network *unstructured.Unstructured, clusterID, nodeName, site string) string {
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, raw := range nodes {
		entry, _ := raw.(map[string]interface{})
		if named, _ := entry["site"].(string); entry["clusterID"] == clusterID && entry["nodeName"] == nodeName && named != "" {
			site = named
		}
	}
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	for _, raw := range sites {
		entry, _ := raw.(map[string]interface{})
		clusters, _, _ := unstructured.NestedStringSlice(entry, "clusters")
		if site != "" && entry["name"] != site || site == "" && !slices.Contains(clusters, clusterID) {
			continue
		}
		if gateway, _, _ := unstructured.NestedString(entry, "leafUplink", "gateway"); gateway != "" {
			return gateway
		}
	}
	return ""
}

// APIForAdmission is where a machine reaches Station API to redeem its code: one the SiteNetwork declares with its key
// has its tunnel before it joins (a new site's machines, which do not reach Station API otherwise) and redeems through
// it; any other at the bootstrap address.
func APIForAdmission(network *unstructured.Unstructured, clusterID, nodeName string, bootstrap AdmissionBootstrap) string {
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, raw := range nodes {
		entry, _ := raw.(map[string]interface{})
		if key, _ := entry["publicKey"].(string); entry["clusterID"] == clusterID && entry["nodeName"] == nodeName && key != "" {
			return bootstrap.TunnelAddress()
		}
	}
	return bootstrap.Address()
}

// JoinCommand is the one line the operator runs on the machine. The machine
// has no CA of ours yet: the line fetches the CA unverified, checks it
// against the digest it carries, and verifies every connection after that
// with it. A machine of a class that stands behind a device of its own
// (NodeClass, uplink) on a site that says where that device is (leafUplink)
// first makes the device let it out, running the device's setup only if it
// is, byte for byte, the file Station API serves now.
func JoinCommand(ctx context.Context, network *unstructured.Unstructured, clusterID, nodeName, site, class, code, caDigest string, bootstrap AdmissionBootstrap, classes NodeClassReader) (string, error) {
	address := APIForAdmission(network, clusterID, nodeName, bootstrap)
	trust := fmt.Sprintf(`c=$(mktemp) && curl --connect-timeout 15 -fsSk https://%s/v1/bootstrap/ca -o $c && echo "%s  $c" | sha256sum -c - && curl -fsS --cacert $c https://%s/b/%s`,
		address, caDigest, address, code)
	first := ""
	if gateway := leafUplinkOf(network, clusterID, nodeName, site); gateway != "" && class != "" {
		declared, err := classes.Get(ctx, class, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("read NodeClass %s: %w", class, err)
		}
		login, _, _ := unstructured.NestedString(declared.Object, "spec", "access", "entry", "user")
		if _, declares, _ := unstructured.NestedMap(declared.Object, "spec", "uplink", "setup"); declares && login != "" {
			digest, err := bootstrap.Digest(ctx, "/b/"+code+"/uplink")
			if err != nil {
				return "", fmt.Errorf("read the uplink setup of NodeClass %s: %w", class, err)
			}
			first = fmt.Sprintf(`ssh -t %s@%s 'f=$(mktemp) %s/uplink -o $f && echo "%s  $f" | sha256sum -c - && sudo sh $f' && `, login, gateway, trust, digest)
		}
	}
	return first + trust + " | sudo sh", nil
}
