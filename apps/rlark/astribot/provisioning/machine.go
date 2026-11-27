package provisioning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// Machine is a logged-in session on the first machine.
type Machine interface {
	// Run runs a script of RLark's own as root. stdin, when not nil, is what the script reads; each line it
	// prints goes to line, when not nil. It answers what the script printed; a script that fails answers an error
	// that carries its last lines.
	Run(ctx context.Context, script string, stdin io.Reader, line func(string)) (string, error)
	Close() error
}

const (
	// BootstrapDir is where the installer is unpacked on the machine, as Station API's join puts it.
	BootstrapDir = "/var/lib/rlark/bootstrap"
	// DiskFloor is the free space /var/lib needs: the installer, the images of the control plane and of
	// RLark's components.
	DiskFloor = 50 << 30
	// CacheVolume is the site cache's volume, made when the machine has room for it beside the floor.
	CacheVolume         = "site-cache=300G:/var/lib/rlark/site-cache"
	provisionCacheFloor = 300<<30 + DiskFloor
	// TunnelPort is the WireGuard port of a site's hub that has an address of its own.
	TunnelPort = 51820
)

// precheckScript reads what the stages need to know of the machine; it changes nothing but loading the
// wireguard module.
const precheckScript = `set -u
echo "hostname=$(hostname | tr '[:upper:]' '[:lower:]')"
. /etc/os-release && echo "os=$PRETTY_NAME"
echo "kernel=$(uname -r)"
[ -d /run/systemd/system ] && echo systemd=yes
command -v tar >/dev/null && echo tar=yes
modprobe wireguard 2>/dev/null && echo wireguard=yes
command -v nvidia-smi >/dev/null && nvidia-smi --query-gpu=name --format=csv,noheader | sed 's/^/gpu=/'
echo "diskfree=$(df -B1 --output=avail /var/lib | tail -1 | tr -d ' ')"
dev=$(ip -4 route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}')
[ -n "$dev" ] && ip -4 -o addr show dev "$dev" | awk '{split($4, a, "/"); print "address=" a[1]; exit}'
ip -4 route show | awk '$1 != "default" {print "route=" $1}'
ip -4 -o addr show | awk '{print "route=" $4}'
echo "bundle=$(cat ` + BootstrapDir + `/bundle.sha256 2>/dev/null)"
[ -s /var/lib/station/wg/private.key ] && [ -x /opt/rlark/bin/station-gateway ] && echo "publickey=$(/opt/rlark/bin/station-gateway --public-key)"
`

// ReadMachine reads what a machine that becomes a node of a site needs, and refuses one that lacks it: systemd, tar,
// the wireguard module, an address of its own network, floor bytes free in /var/lib. It reads too the sha256 of the
// installer the machine holds and its tunnel key, when it has them.
func ReadMachine(ctx context.Context, machine Machine, floor int64) (rlarkv1alpha1.ClusterProvisionFacts, string, error) {
	var facts rlarkv1alpha1.ClusterProvisionFacts
	out, err := machine.Run(ctx, precheckScript, nil, nil)
	if err != nil {
		return facts, "", err
	}
	seen := map[string]bool{}
	gpus := map[string]int32{}
	var free int64
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		seen[key] = true
		switch key {
		case "hostname":
			facts.NodeName = value
		case "os":
			facts.OS = value
		case "kernel":
			facts.Kernel = value
		case "gpu":
			gpus[strings.TrimSpace(value)]++
		case "diskfree":
			free, _ = strconv.ParseInt(value, 10, 64)
		case "address":
			facts.PrivateAddress = value
		case "bundle":
			facts.BundleSHA256 = value
		case "publickey":
			facts.PublicKey = value
		case "route":
			if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Addr().Is4() && !prefix.Addr().IsLoopback() {
				facts.Routes = append(facts.Routes, prefix.Masked().String())
			}
		}
	}
	for _, model := range slices.Sorted(maps.Keys(gpus)) {
		facts.GPUs = append(facts.GPUs, rlarkv1alpha1.ClusterProvisionGPU{Model: model, Count: gpus[model]})
	}
	slices.Sort(facts.Routes)
	facts.Routes = slices.Compact(facts.Routes)
	facts.DiskFree = fmt.Sprintf("%d GiB", free>>30)
	facts.CacheVolume = free >= provisionCacheFloor
	readback := fmt.Sprintf("%s, %s, kernel %s; %s; /var/lib %s free; private address %s", facts.NodeName, facts.OS, facts.Kernel, DescribeGPUs(facts.GPUs), facts.DiskFree, facts.PrivateAddress)

	var missing []string
	for key, what := range map[string]string{"systemd": "systemd", "tar": "tar", "wireguard": "the wireguard kernel module"} {
		if !seen[key] {
			missing = append(missing, what)
		}
	}
	slices.Sort(missing)
	switch {
	case len(missing) > 0:
		return facts, readback, fmt.Errorf("the machine has no %s", strings.Join(missing, ", "))
	case facts.PrivateAddress == "":
		return facts, readback, errors.New("the machine has no default route, so no address of its own network")
	case free < floor:
		return facts, readback, fmt.Errorf("/var/lib has %s free; the installer and the images need %d GiB", facts.DiskFree, floor>>30)
	}
	return facts, readback, nil
}

func DescribeGPUs(gpus []rlarkv1alpha1.ClusterProvisionGPU) string {
	if len(gpus) == 0 {
		return "no NVIDIA GPU"
	}
	parts := make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		parts = append(parts, fmt.Sprintf("%d × %s", gpu.Count, gpu.Model))
	}
	return strings.Join(parts, ", ")
}

// BundleScript takes the installer of sha256 expect from stdin, unpacks it where the join's installer is and marks it
// with its sha256 (bundle.sha256, which the code's line reads too), then makes the machine's tunnel key (TunnelKeyScript).
func BundleScript(registry, expect string) string {
	return `set -eu
D=` + BootstrapDir + `
install -d /var/lib/rlark /opt/rlark/bin /etc/rlark/station
cat > /var/lib/rlark/installer.tar.new
sum=$(sha256sum /var/lib/rlark/installer.tar.new | cut -d' ' -f1)
echo "sha256=$sum"
[ "$sum" = "` + expect + `" ] || { rm -f /var/lib/rlark/installer.tar.new; echo "the installer received is not of sha256 ` + expect + `" >&2; exit 1; }
rm -rf $D.new && mkdir $D.new && tar -x -C $D.new -f /var/lib/rlark/installer.tar.new
rm -rf $D && mv $D.new $D && rm /var/lib/rlark/installer.tar.new
echo "$sum" > $D/bundle.sha256
` + TunnelKeyScript(registry)
}

// TunnelKeyScript makes the machine's tunnel key from the installer it holds, which never leaves it. It prints the CA
// of the site registry the installer trusts, which the cluster's registry Addons are given.
func TunnelKeyScript(registry string) string {
	return `set -eu
D=` + BootstrapDir + `
install -d /opt/rlark/bin /etc/rlark/station
install -m 755 $D/bin/station-gateway /opt/rlark/bin/
echo "publickey=$(/opt/rlark/bin/station-gateway --public-key)"
echo "ca=$(base64 -w0 $D/certs.d/` + registry + `/ca.crt)"
`
}

// OutputValues reads the key=value lines a script printed.
func OutputValues(out string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	return values
}

// ControlPlaneScript puts the tunnel document the SiteNetwork published for the node (stdin) where the installer
// reads it, and makes the machine the cluster's control plane; run again, the installer goes on where it stopped.
func ControlPlaneScript(facts rlarkv1alpha1.ClusterProvisionFacts) string {
	// WORKLOADS: the first machine is the cluster's control plane and its worker too (its GPUs run the site's Jobs).
	env := fmt.Sprintf("CONTROL_PLANE_NAME=%s POD_SUBNET=%s SERVICE_SUBNET=%s NODE_NAME=%s WORKLOADS=yes", facts.ControlPlaneName, facts.PodSubnet, facts.ServiceSubnet, facts.NodeName)
	if facts.CacheVolume {
		env += " NODE_VOLUMES=" + CacheVolume
	}
	return `set -eu
umask 077
cat > /etc/rlark/station/bootstrap.json
cd ` + BootstrapDir + `
` + env + ` ./station-bootstrap control-plane 2>&1
K="` + BootstrapDir + `/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf"
$K wait --for=condition=Ready node/` + facts.NodeName + ` --timeout=300s >/dev/null || true
$K get node ` + facts.NodeName + ` -o jsonpath='ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
`
}

// LastLine is the last line of out that holds want, or its last line.
func LastLine(out, want string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], want) {
			return strings.TrimSpace(lines[i])
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
