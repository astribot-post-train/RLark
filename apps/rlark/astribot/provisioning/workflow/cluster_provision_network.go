package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

func (g *Service) provisionNetwork(ctx context.Context) (provisioning.Network, error) {
	object, err := stationidentity.ReadSiteNetwork(ctx, g.stationClient)
	if err != nil {
		return provisioning.Network{}, fmt.Errorf("read the SiteNetwork: %w", err)
	}
	if object.GetName() == "" {
		return provisioning.Network{}, errors.New("there is no SiteNetwork (station-system/default): a new site is declared in it")
	}
	return provisioning.Network{Object: object}, nil
}

// admissionAddresses are the tunnel addresses the node admissions hold (pinned, or allocated by station-controller):
// a machine that joined by a code has its address there, not in the SiteNetwork.
func (g *Service) admissionAddresses(ctx context.Context) ([]string, error) {
	admissions, err := g.stationClient.Resource(nodeAdmissionGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list node admissions: %w", err)
	}
	var out []string
	for _, admission := range admissions.Items {
		for _, path := range [][]string{{"spec", "address"}, {"status", "address"}} {
			if address, _, _ := unstructured.NestedString(admission.Object, path...); address != "" {
				out = append(out, address)
			}
		}
	}
	return out, nil
}

// clusterNetworks are the Pod and Service networks of a cluster, as its API shows them: kubeadm's configuration
// where there is one, each node's Pod CIDR, and the /16 around the API's own Service address.
func (g *Service) clusterNetworks(ctx context.Context, cluster string) ([]netip.Prefix, error) {
	agent := provisionAgentID(cluster)
	get := func(path string, into any) (bool, error) {
		resp, err := g.hooks.ProxyKubeRequest(ctx, http.MethodGet, agent, path, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("%s answered %d", path, resp.StatusCode)
		}
		return true, json.NewDecoder(resp.Body).Decode(into)
	}
	var out []netip.Prefix
	add := func(cidr string) {
		if prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err == nil && prefix.Addr().Is4() {
			out = append(out, prefix.Masked())
		}
	}
	var kubeadm struct {
		Data map[string]string `json:"data"`
	}
	if found, err := get("api/v1/namespaces/kube-system/configmaps/kubeadm-config", &kubeadm); err != nil {
		return nil, err
	} else if found {
		var config struct {
			Networking struct {
				PodSubnet     string `json:"podSubnet"`
				ServiceSubnet string `json:"serviceSubnet"`
			} `json:"networking"`
		}
		if err := yaml.Unmarshal([]byte(kubeadm.Data["ClusterConfiguration"]), &config); err == nil {
			for _, cidr := range strings.Split(config.Networking.PodSubnet+","+config.Networking.ServiceSubnet, ",") {
				add(cidr)
			}
		}
	}
	var nodes struct {
		Items []struct {
			Spec struct {
				PodCIDRs []string `json:"podCIDRs"`
			} `json:"spec"`
		} `json:"items"`
	}
	if _, err := get("api/v1/nodes", &nodes); err != nil {
		return nil, err
	}
	for _, node := range nodes.Items {
		for _, cidr := range node.Spec.PodCIDRs {
			add(cidr)
		}
	}
	var service struct {
		Spec struct {
			ClusterIP string `json:"clusterIP"`
		} `json:"spec"`
	}
	if _, err := get("api/v1/namespaces/default/services/kubernetes", &service); err != nil {
		return nil, err
	}
	if address, err := netip.ParseAddr(service.Spec.ClusterIP); err == nil && address.Is4() {
		out = append(out, netip.PrefixFrom(address, 16).Masked())
	}
	return out, nil
}

func (r *provisionRun) declare(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (string, error) {
	facts := &p.Status.Facts
	release, err := r.g.currentRelease(ctx)
	if err != nil {
		return "", err
	}
	facts.Release = release.Name
	registryHost, _, err := net.SplitHostPort(release.Registry)
	if err != nil {
		return "", fmt.Errorf("the release's registry %q is not host:port", release.Registry)
	}
	server, err := url.Parse(r.g.serverAddress)
	if err != nil || server.Hostname() == "" {
		return "", fmt.Errorf("the Gateway's --server-address %q names no host", r.g.serverAddress)
	}
	others, err := r.g.kubeClient.RlinfV1alpha1().ClusterProvisions().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list cluster provisions: %w", err)
	}
	otherAddresses, err := r.g.admissionAddresses(ctx)
	if err != nil {
		return "", err
	}
	var taken []netip.Prefix
	for _, other := range others.Items {
		if other.Name == p.Name {
			continue
		}
		otherAddresses = append(otherAddresses, other.Status.Facts.TunnelAddress)
		for _, cidr := range []string{other.Status.Facts.PodSubnet, other.Status.Facts.ServiceSubnet} {
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				taken = append(taken, prefix)
			}
		}
	}
	var unread []string
	if facts.PodSubnet == "" {
		clusters, err := r.g.kubeClient.RlinfV1alpha1().Clusters().List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("list clusters: %w", err)
		}
		for _, cluster := range clusters.Items {
			if cluster.Name == p.Name {
				continue
			}
			if cluster.Status.Phase != rlarkv1alpha1.ClusterOnline && cluster.Status.Phase != rlarkv1alpha1.ClusterDowngraded {
				unread = append(unread, cluster.Name)
				continue
			}
			networks, err := r.g.clusterNetworks(ctx, cluster.Name)
			if err != nil {
				return "", fmt.Errorf("read the networks of %s: %w", cluster.Name, err)
			}
			taken = append(taken, networks...)
		}
	}

	var readback string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		network, err := r.g.provisionNetwork(ctx)
		if err != nil {
			return err
		}
		if site := network.SiteOf(p.Name); site != "" && site != p.Spec.Site {
			return fmt.Errorf("the SiteNetwork already has %s in site %s", p.Name, site)
		}
		if network.SiteOf(p.Name) == "" && network.HasSite(p.Spec.Site) {
			return fmt.Errorf("the SiteNetwork already has a site %s", p.Spec.Site)
		}
		origin := network.NameAddress(provisioning.OriginName)
		if origin == "" {
			return fmt.Errorf("the SiteNetwork's names have no %s: a new site pulls its images from it", provisioning.OriginName)
		}
		if network.NameAddress(server.Hostname()) == "" {
			return fmt.Errorf("the SiteNetwork's names have no %s: a new cluster's agent reaches RLark by it", server.Hostname())
		}
		if facts.TunnelAddress == "" {
			for _, node := range network.Nodes() {
				if node.ClusterID == p.Name && node.NodeName == facts.NodeName {
					facts.TunnelAddress = node.Address
				}
			}
		}
		if facts.TunnelAddress == "" {
			if facts.TunnelAddress, err = network.AllocateAddress(otherAddresses); err != nil {
				return err
			}
		}
		if facts.PodSubnet == "" {
			networkTaken := slices.Clone(taken)
			if pool, _, _ := unstructured.NestedString(network.Object.Object, "spec", "pool"); pool != "" {
				if prefix, err := netip.ParsePrefix(pool); err == nil {
					networkTaken = append(networkTaken, prefix)
				}
			}
			for _, node := range network.Nodes() {
				for _, cidr := range node.Advertise {
					if prefix, err := netip.ParsePrefix(cidr); err == nil {
						networkTaken = append(networkTaken, prefix)
					}
				}
			}
			for _, route := range facts.Routes {
				if prefix, err := netip.ParsePrefix(route); err == nil {
					networkTaken = append(networkTaken, prefix)
				}
			}
			if facts.PodSubnet, facts.ServiceSubnet, err = provisioning.ChooseSubnets(networkTaken); err != nil {
				return err
			}
		}
		facts.ControlPlaneName = "cp." + p.Name + ".rlark.internal"
		if address := network.NameAddress(facts.ControlPlaneName); address != "" && address != facts.TunnelAddress {
			return fmt.Errorf("the SiteNetwork's names give %s the address %s, not this cluster's control plane's", facts.ControlPlaneName, address)
		}
		object := network.Object
		names, _, _ := unstructured.NestedSlice(object.Object, "spec", "names")
		if network.NameAddress(facts.ControlPlaneName) == "" {
			names = append(names, map[string]interface{}{"name": facts.ControlPlaneName, "address": facts.TunnelAddress})
		}
		sites, _, _ := unstructured.NestedSlice(object.Object, "spec", "sites")
		if !network.HasSite(p.Spec.Site) {
			// Mesh: the site's later hubs reach its control plane, which is on its first hub's tunnel address.
			sites = append(sites, map[string]interface{}{
				"name": p.Spec.Site, "clusters": []interface{}{p.Name}, "mesh": true,
				"names": []interface{}{map[string]interface{}{"name": registryHost, "address": origin}},
			})
		}
		nodes, _, _ := unstructured.NestedSlice(object.Object, "spec", "nodes")
		nodes = provisioning.WithHub(nodes, provisioning.HubEntry(p.Name, facts.NodeName, facts.TunnelAddress, facts.PrivateAddress, facts.PublicKey, p.Spec.Machine.TunnelEndpoint))
		for path, value := range map[string][]interface{}{"names": names, "sites": sites, "nodes": nodes} {
			if err := unstructured.SetNestedSlice(object.Object, value, "spec", path); err != nil {
				return err
			}
		}
		if _, err := r.g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			return err
		}
		readback = fmt.Sprintf("site %s; %s → %s; node %s hub at %s, endpoint %s; %s → %s; Pod %s, Service %s",
			p.Spec.Site, facts.ControlPlaneName, facts.TunnelAddress, facts.NodeName, facts.TunnelAddress, p.Spec.Machine.TunnelEndpoint, registryHost, origin, facts.PodSubnet, facts.ServiceSubnet)
		return nil
	})
	if err != nil {
		return readback, err
	}
	if len(unread) > 0 {
		readback += "; not read (offline): " + strings.Join(unread, ", ")
	}
	if _, err := r.g.declaredDocument(ctx, p.Name, facts.NodeName); err != nil {
		return readback, err
	}
	return readback + "; status.declared holds its tunnel document", nil
}

// declareHub declares a machine of a cluster's site as a hub by its key, at a tunnel address of its own (the one it
// has, or the lowest free), and answers that address and the SiteNetwork's generation that holds the declaration.
func (g *Service) declareHub(ctx context.Context, cluster, node, privateAddress, publicKey, endpoint string) (string, int64, error) {
	admitted, err := g.admissionAddresses(ctx)
	if err != nil {
		return "", 0, err
	}
	address := ""
	generation := int64(0)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		network, err := g.provisionNetwork(ctx)
		if err != nil {
			return err
		}
		if network.SiteOf(cluster) == "" {
			return fmt.Errorf("no site of the SiteNetwork has the cluster %s", cluster)
		}
		address = ""
		for _, entry := range network.Nodes() {
			if entry.ClusterID == cluster && entry.NodeName == node {
				address = entry.Address
			}
		}
		if address == "" {
			if address, err = network.AllocateAddress(admitted); err != nil {
				return err
			}
		}
		nodes, _, _ := unstructured.NestedSlice(network.Object.Object, "spec", "nodes")
		if err := unstructured.SetNestedSlice(network.Object.Object, provisioning.WithHub(nodes, provisioning.HubEntry(cluster, node, address, privateAddress, publicKey, endpoint)), "spec", "nodes"); err != nil {
			return err
		}
		written, err := g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Update(ctx, network.Object, metav1.UpdateOptions{})
		if err == nil {
			generation = written.GetGeneration()
		}
		return err
	})
	return address, generation, err
}

// declaredDocument waits for the first tunnel document station-controller publishes for a node declared by its key.
func (g *Service) declaredDocument(ctx context.Context, cluster, node string) (string, error) {
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		network, err := g.provisionNetwork(ctx)
		if err != nil {
			return "", err
		}
		if document := network.Declared(cluster, node); document != "" {
			return document, nil
		}
	}
	return "", fmt.Errorf("station-controller has published no tunnel document for %s in 2 min (SiteNetwork status.declared)", node)
}
