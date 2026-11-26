package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"io"
	"net"
	"strings"
	"time"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// provisionDialFunc logs in to the machine with its password. A hostKey (SHA256 fingerprint) that is not empty is
// the key the machine must show; an empty one takes the key it shows, which is answered.
type provisionDialFunc func(ctx context.Context, machine rlarkv1alpha1.ClusterProvisionMachine, password []byte, hostKey string) (provisioning.Machine, string, error)

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
func (g *Gateway) nodeNameTaken(ctx context.Context, name string) (string, error) {
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
	installed, readback, err := r.g.sendInstaller(ctx, machine, release.Registry, p.Status.Facts.BundleSHA256, func(line string) { r.line(ctx, stageBundle, line) })
	facts := &p.Status.Facts
	facts.BundleSHA256, facts.PublicKey, facts.RegistryCA = installed.sha256, installed.publicKey, installed.registryCA
	return readback, err
}

// installedMachine is what sendInstaller leaves on a machine: Station API's installer, of sha256, and the machine's
// tunnel key, made there; registryCA is the CA of the site registry the installer trusts.
type installedMachine struct {
	sha256, publicKey, registryCA string
}

// sendInstaller gives the machine Station API's installer, unless the one it holds (of sha256 have) is that one
// already, and has the machine make its tunnel key, which never leaves it. While it streams, progress is told how
// much has gone every 5 s.
func (g *Gateway) sendInstaller(ctx context.Context, machine provisioning.Machine, registry, have string, progress func(string)) (installedMachine, string, error) {
	var installed installedMachine
	want, err := g.stationBootstrap.BundleDigest(ctx)
	if err != nil {
		return installed, "", fmt.Errorf("read the installer's sha256 from Station API: %w", err)
	}
	var out, readback string
	if have == want {
		if out, err = machine.Run(ctx, provisioning.TunnelKeyScript(registry), nil, nil); err != nil {
			return installed, "", err
		}
		readback = fmt.Sprintf("the installer of sha256 %s… is on the machine already, not sent again", short(want))
	} else {
		installer, size, err := g.stationBootstrap.Open(ctx, "/v1/bootstrap/bundle")
		if err != nil {
			return installed, "", fmt.Errorf("take the installer from Station API: %w", err)
		}
		defer installer.Close()
		sent := &sentCounter{reader: installer, size: size, progress: progress, started: time.Now()}
		if out, err = machine.Run(ctx, provisioning.BundleScript(registry, want), sent, nil); err != nil {
			return installed, "", err
		}
		readback = fmt.Sprintf("installer sha256 %s… sent (%s in %s)", short(provisioning.OutputValues(out)["sha256"]), megabytes(sent.count), time.Since(sent.started).Round(time.Second))
	}
	values := provisioning.OutputValues(out)
	ca, _ := base64.StdEncoding.DecodeString(values["ca"])
	installed = installedMachine{sha256: want, publicKey: values["publickey"], registryCA: string(ca)}
	readback += "; tunnel public key " + installed.publicKey
	if values["sha256"] != "" && values["sha256"] != want {
		return installed, readback, fmt.Errorf("the machine holds an installer of sha256 %s, Station API's is %s", values["sha256"], want)
	}
	if !strings.Contains(installed.registryCA, "BEGIN CERTIFICATE") {
		return installed, readback, fmt.Errorf("the installer holds no CA of %s (certs.d/%s/ca.crt)", registry, registry)
	}
	if len(installed.publicKey) != 44 {
		return installed, readback, fmt.Errorf("station-gateway answered no WireGuard public key (%q)", installed.publicKey)
	}
	return installed, readback, nil
}

// sentCounter counts what the installer's stream has given, and tells progress every 5 s.
type sentCounter struct {
	reader   io.Reader
	size     int64
	count    int64
	progress func(string)
	started  time.Time
	told     time.Time
}

func (s *sentCounter) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	s.count += int64(n)
	if now := time.Now(); now.Sub(s.told) >= 5*time.Second {
		s.told = now
		rate := float64(s.count) / max(now.Sub(s.started).Seconds(), 1) / (1 << 20)
		s.progress(fmt.Sprintf("sent %s of %s, %.1f MB/s", megabytes(s.count), megabytes(s.size), rate))
	}
	return n, err
}

func megabytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MB", n>>20)
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
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
	out, err := machine.Run(ctx, provisioning.ControlPlaneScript(facts), strings.NewReader(document), func(line string) { r.line(ctx, stageControlPlane, line) })
	if err != nil {
		return tunnel, err
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
