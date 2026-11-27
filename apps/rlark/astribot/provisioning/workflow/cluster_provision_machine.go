package workflow

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DialFunc logs in to the machine with its password. A hostKey (SHA256 fingerprint) that is not empty is
// the key the machine must show; an empty one takes the key it shows, which is answered.
type DialFunc func(ctx context.Context, machine rlarkv1alpha1.ClusterProvisionMachine, password []byte, hostKey string) (provisioning.Machine, string, error)

func (r *provisionRun) precheck(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (string, error) {
	machine, err := r.needMachine(ctx, p)
	if err != nil {
		return "", err
	}
	facts, readback, err := provisioning.ReadMachine(ctx, machine, provisioning.DiskFloor)
	p.Status.Facts = facts
	if err != nil {
		return readback, err
	}
	if facts.NodeName == "" || len(facts.NodeName) > 63 || strings.ContainsAny(facts.NodeName, "._ ") || strings.Trim(facts.NodeName, "-") != facts.NodeName {
		return readback, fmt.Errorf("the machine's hostname %q is not a node name (lower-case letters, digits and -)", facts.NodeName)
	}
	if taken, err := r.g.nodeNameTaken(ctx, facts.NodeName); err != nil {
		return readback, err
	} else if taken != "" {
		return readback, fmt.Errorf("a node named %s is already %s: rename the machine (hostnamectl set-hostname) and retry", facts.NodeName, taken)
	}
	return readback, nil
}

// nodeNameTaken says where a node of that name already is, if anywhere: a node of a cluster, or one the
// SiteNetwork declares.
func (g *Service) nodeNameTaken(ctx context.Context, name string) (string, error) {
	nodes, err := g.kubeClient.RlinfV1alpha1().Nodes("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		if node.Name == name {
			return "a node of " + node.Namespace, nil
		}
	}
	network, err := g.provisionNetwork(ctx)
	if err != nil {
		return "", err
	}
	for _, node := range network.Nodes() {
		if node.NodeName == name {
			return "declared in the SiteNetwork for " + node.ClusterID, nil
		}
	}
	return "", nil
}

func (r *provisionRun) bundle(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (string, error) {
	release, err := r.g.currentRelease(ctx)
	if err != nil {
		return "", err
	}
	machine, err := r.needMachine(ctx, p)
	if err != nil {
		return "", err
	}
	installed, readback, err := provisioning.SendInstaller(ctx, r.g.stationBootstrap, machine, release.Registry, p.Status.Facts.BundleSHA256, func(line string) { r.line(ctx, stageBundle, line) })
	facts := &p.Status.Facts
	facts.BundleSHA256, facts.PublicKey, facts.RegistryCA = installed.SHA256, installed.PublicKey, installed.RegistryCA
	return readback, err
}

func (r *provisionRun) controlPlane(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (string, error) {
	machine, err := r.needMachine(ctx, p)
	if err != nil {
		return "", err
	}
	facts := p.Status.Facts
	network, err := r.g.provisionNetwork(ctx)
	if err != nil {
		return "", err
	}
	document := network.Declared(p.Name, facts.NodeName)
	if document == "" {
		return "", fmt.Errorf("the SiteNetwork publishes no tunnel document for %s (status.declared): station-controller has not taken the declaration", facts.NodeName)
	}
	if _, err := machine.Run(ctx, claimProvisionHost(p.UID), nil, nil); err != nil {
		return "", fmt.Errorf("claim the control-plane host: %w", err)
	}
	// The machine's tunnel first, waited for on its peers' reports (awaitTunnel): the installer's own wait for a
	// handshake (station-gateway --await-peers) has no Station API to ask on a declared hub.
	if _, err := machine.Run(ctx, tunnelScript, strings.NewReader(document), nil); err != nil {
		return "", err
	}
	var seen rlarkv1alpha1.NodeJoinFacts
	tunnel, err := r.g.awaitTunnel(ctx, machine, p.Name+"/"+facts.NodeName, document, p.Spec.Machine.TunnelEndpoint, joinClock(), &seen,
		func(message string) { r.line(ctx, stageControlPlane, message) })
	if err != nil {
		return tunnel, err
	}
	out, err := machine.Run(ctx, "export RLARK_PROVISION_UID="+nodessh.Quote(string(p.UID))+"\n"+provisioning.ControlPlaneScript(facts), strings.NewReader(document), func(line string) { r.line(ctx, stageControlPlane, line) })
	if err != nil {
		return tunnel, err
	}
	if _, err := machine.Run(ctx, recordProvisionHostCluster(p.UID), nil, nil); err != nil {
		return tunnel, fmt.Errorf("record the host's Kubernetes cluster UID: %w", err)
	}
	if ready := provisioning.OutputValues(out)["ready"]; ready != "True" {
		return tunnel + "; node " + facts.NodeName + " Ready=" + ready, fmt.Errorf("the control plane is made, but node %s is not Ready", facts.NodeName)
	}
	// The management plane reaches the API through the tunnel: the first handshake shows the hub's UDP port is open.
	address := net.JoinHostPort(facts.TunnelAddress, "6443")
	var dialErr error
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		var connection net.Conn
		if connection, dialErr = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address); dialErr == nil {
			_ = connection.Close()
			break
		}
	}
	readback := fmt.Sprintf("%s; node %s Ready; %s", tunnel, facts.NodeName, provisioning.LastLine(out, "Done"))
	if dialErr != nil {
		return readback, fmt.Errorf("the management plane does not reach the API at %s through the tunnel (does UDP %s reach the machine?): %v", address, p.Spec.Machine.TunnelEndpoint, dialErr)
	}
	return readback + "; the API answers at " + address + " through the tunnel", nil
}

// needAdmin reads the cluster's administrator credential from the machine, once in a run, into memory only. The
// API is reached at the control plane's tunnel address, by the name its certificate holds.
func (r *provisionRun) needAdmin(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (*rest.Config, error) {
	if r.admin != nil {
		return r.admin, nil
	}
	machine, err := r.needMachine(ctx, p)
	if err != nil {
		return nil, err
	}
	kubeconfig, err := machine.Run(ctx, "cat /etc/kubernetes/admin.conf", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("read the cluster's admin.conf: %w", err)
	}
	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("the cluster's admin.conf: %w", err)
	}
	config.Host = "https://" + net.JoinHostPort(p.Status.Facts.TunnelAddress, "6443")
	config.ServerName = p.Status.Facts.ControlPlaneName
	r.admin = config
	return config, nil
}
