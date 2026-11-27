package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
)

const legacyTunnelConfig = "/etc/rlark/station/bootstrap.json"
const priorTunnelMarker = `
if [ -e "$marker" ]; then record=$(cat "$marker"); else record=; fi
marker_sha=$(printf '%s' "$record" | sha256sum)
`
const detachedJoinHost = `
[ ! -e /etc/kubernetes/kubelet.conf ] &&
[ ! -e /etc/kubernetes/admin.conf ] &&
[ ! -e /etc/kubernetes/manifests/kube-apiserver.yaml ] &&
[ ! -d /var/lib/rlark/control-plane ] ||
  { echo 'the historical SSH host still has a Kubernetes installation' >&2; exit 1; }
state=$(systemctl show rlark-join -p ActiveState --value)
case "$state" in
  inactive|failed) ;;
  *) echo 'the historical SSH host still has an active installer' >&2; exit 1;;
esac
`

// authorizeLegacyTunnel binds a surviving historical tunnel to the pinned SSH
// host and the original WireGuard key. A live Node (even with another UID)
// prevents taking over or removing the old tunnel.
func (g *Service) authorizeLegacyTunnel(ctx context.Context, machine provisioning.Machine, j *rlark.NodeJoin) (string, error) {
	if j.UID == "" || j.Status.Facts.PublicKey == "" || j.Status.HostKey == "" || g.hooks.ProxyKubeRequest == nil {
		return "", errors.New("historical tunnel has no verified host key or tunnel identity")
	}
	out, err := machine.Run(ctx, "set -eu\nmarker="+joinTunnelHostMarker+"\n"+lockHostMarker+detachedJoinHost+priorTunnelMarker+
		`printf '%s\n' "${marker_sha%% *}"; cat `+legacyTunnelConfig, nil, nil)
	if err != nil {
		return "", err
	}
	markerDigest, document, ok := strings.Cut(out, "\n")
	if !ok || len(markerDigest) != 64 {
		return "", errors.New("historical tunnel inspection returned no marker fingerprint")
	}
	var config struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(document), &config); err != nil || config.PublicKey != j.Status.Facts.PublicKey {
		return "", errors.New("historical tunnel does not match the pinned host's WireGuard key")
	}
	response, err := g.hooks.ProxyKubeRequest(ctx, http.MethodGet, provisionAgentID(j.Spec.ClusterID), "api/v1/nodes/"+j.Spec.NodeName, nil)
	if err != nil {
		return "", fmt.Errorf("verify historical Node absence: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("member API did not confirm that Node %s is absent (HTTP %d)", j.Spec.NodeName, response.StatusCode)
	}
	var status metav1.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil || status.Reason != metav1.StatusReasonNotFound ||
		status.Details == nil || status.Details.Kind != "nodes" || status.Details.Name != j.Spec.NodeName {
		return "", errors.New("member API did not return a native Node NotFound status")
	}
	// Machine.Run captures complete lines and can normalize the final newline.
	// Check the hash in the same shell representation used for the read.
	sum := sha256.Sum256([]byte(strings.TrimRight(document, "\n")))
	return markerDigest + "/" + fmt.Sprintf("%x", sum), nil
}

func claimDetachedTunnel(j *rlark.NodeJoin, digest string) string {
	markerDigest, documentDigest, _ := strings.Cut(digest, "/")
	return "set -eu\nmarker=" + joinTunnelHostMarker + "\n" + lockHostMarker + detachedJoinHost + priorTunnelMarker + `
[ "${marker_sha%% *}" = ` + nodessh.Quote(markerDigest) + ` ] ||
  { echo 'historical tunnel owner changed while verifying its identity' >&2; exit 1; }
current=$(cat ` + legacyTunnelConfig + `)
sum=$(printf '%s' "$current" | sha256sum)
[ "${sum%% *}" = ` + nodessh.Quote(documentDigest) + ` ] ||
  { echo 'historical tunnel changed while verifying its identity' >&2; exit 1; }
rm -f "$marker.node"
(umask 077; printf '%s\n' ` + nodessh.Quote(string(j.UID)) + ` > "$marker")
`
}
