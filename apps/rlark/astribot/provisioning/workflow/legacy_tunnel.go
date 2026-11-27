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

// A residual worker installation is eligible only for the same cluster and
// node recorded under the host marker lock. Keep legacy tunnel-only reuse.
const workerJoinIdentity = `
if [ -f "$marker.identity" ]; then
  [ "$(cat "$marker.identity")" = "$join_identity" ] ||
    { echo 'host belongs to a different SSH join identity' >&2; exit 1; }
elif [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then
  echo 'residual Kubernetes worker has no matching SSH join identity' >&2; exit 1
fi
`

const resetDetachedWorker = `
if [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then
  socket=$(sed -n 's|.*--container-runtime-endpoint=\(unix://[^ "]*\).*|\1|p' /var/lib/kubelet/kubeadm-flags.env 2>/dev/null || true)
  test -x /opt/rlark/bin/kubeadm
  systemctl stop kubelet
  /opt/rlark/bin/kubeadm reset --force ${socket:+--cri-socket "$socket"}
  [ ! -e /etc/kubernetes/kubelet.conf ] && [ ! -e /etc/kubernetes/bootstrap-kubelet.conf ]
fi
`

func joinHostIdentity(j *rlark.NodeJoin) string {
	return "join_identity=" + nodessh.Quote(j.Spec.ClusterID+"/"+j.Spec.NodeName) + "\n"
}

const rememberJoinHostIdentity = `
if [ -e "$marker.identity" ]; then
  [ "$(cat "$marker.identity")" = "$join_identity" ] ||
    { echo 'host belongs to a different SSH join identity' >&2; exit 1; }
fi
(umask 077; printf '%s\n' "$join_identity" > "$marker.identity")
`

// authorizeLegacyTunnel binds a surviving historical tunnel to the pinned SSH
// host and the original WireGuard key. A live Node (even with another UID)
// prevents taking over or removing the old tunnel.
func (g *Service) authorizeLegacyTunnel(ctx context.Context, machine provisioning.Machine, j *rlark.NodeJoin) (string, error) {
	if j.UID == "" || j.Status.Facts.PublicKey == "" || j.Status.HostKey == "" || g.hooks.ProxyKubeRequest == nil {
		return "", errors.New("historical tunnel has no verified host key or tunnel identity")
	}
	out, err := machine.Run(ctx, "set -eu\nmarker="+joinTunnelHostMarker+"\n"+joinHostIdentity(j)+lockHostMarker+detachedJoinHost+workerJoinIdentity+priorTunnelMarker+
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
	if err := g.confirmNativeNodeAbsent(ctx, j); err != nil {
		return "", err
	}
	// Machine.Run captures complete lines and can normalize the final newline.
	// Check the hash in the same shell representation used for the read.
	sum := sha256.Sum256([]byte(strings.TrimRight(document, "\n")))
	return markerDigest + "/" + fmt.Sprintf("%x", sum), nil
}

func (g *Service) confirmNativeNodeAbsent(ctx context.Context, j *rlark.NodeJoin) error {
	if g.hooks.ProxyKubeRequest == nil {
		return errors.New("member API access is unavailable")
	}
	response, err := g.hooks.ProxyKubeRequest(ctx, http.MethodGet, provisionAgentID(j.Spec.ClusterID), "api/v1/nodes/"+j.Spec.NodeName, nil)
	if err != nil {
		return fmt.Errorf("verify historical Node absence: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("member API did not confirm that Node %s is absent (HTTP %d)", j.Spec.NodeName, response.StatusCode)
	}
	var status metav1.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil || status.Reason != metav1.StatusReasonNotFound ||
		status.Details == nil || status.Details.Kind != "nodes" || status.Details.Name != j.Spec.NodeName {
		return errors.New("member API did not return a native Node NotFound status")
	}
	return nil
}

func claimDetachedTunnel(j *rlark.NodeJoin, digest string) string {
	markerDigest, documentDigest, _ := strings.Cut(digest, "/")
	return "set -eu\nmarker=" + joinTunnelHostMarker + "\n" + joinHostIdentity(j) + lockHostMarker + detachedJoinHost + workerJoinIdentity + priorTunnelMarker + `
[ "${marker_sha%% *}" = ` + nodessh.Quote(markerDigest) + ` ] ||
  { echo 'historical tunnel owner changed while verifying its identity' >&2; exit 1; }
current=$(cat ` + legacyTunnelConfig + `)
sum=$(printf '%s' "$current" | sha256sum)
[ "${sum%% *}" = ` + nodessh.Quote(documentDigest) + ` ] ||
  { echo 'historical tunnel changed while verifying its identity' >&2; exit 1; }
` + resetDetachedWorker + `
rm -f "$marker.node" "$marker.identity"
(umask 077; printf '%s\n' ` + nodessh.Quote(string(j.UID)) + ` > "$marker")
` + rememberJoinHostIdentity
}
