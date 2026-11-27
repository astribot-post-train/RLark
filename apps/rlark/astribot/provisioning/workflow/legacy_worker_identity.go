package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// The historical bundle recorded the original node name in join.yaml.
// Print only the node name and the public CA fingerprint, never the join token
// or the worker's client keys. Compare both again under the marker lock.
const readLegacyWorkerIdentity = `
legacy_worker_identity() {
  config=/etc/kubernetes/kubelet.conf
  [ -f "$config" ] || config=/etc/kubernetes/bootstrap-kubelet.conf
  name=$(sed -n '/^nodeRegistration:$/,/^[^ ]/{s/^  name: "\\([^"]*\\)"$/\\1/p;}' ` + provisioning.BootstrapDir + `/join.yaml)
  ca=$(/opt/rlark/bin/kubectl --kubeconfig "$config" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}')
  server=$(/opt/rlark/bin/kubectl --kubeconfig "$config" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.server}')
  [ -n "$name" ] && [ -n "$ca" ] && [ -n "$server" ] || return 1
  digest=$({ printf '%s\n' "$server"; printf '%s' "$ca" | base64 -d; } | sha256sum)
  printf '%s/%s' "$name" "${digest%% *}"
}
`

func (g *Service) verifyLegacyWorkerIdentity(ctx context.Context, j *rlark.NodeJoin, reported string) error {
	name, digest, ok := strings.Cut(reported, "/")
	raw, decodeErr := hex.DecodeString(digest)
	if !ok || name != j.Spec.NodeName || decodeErr != nil || len(raw) != sha256.Size {
		return fmt.Errorf("the historical worker does not identify the requested node")
	}
	response, err := g.hooks.ProxyKubeRequest(ctx, http.MethodGet, provisionAgentID(j.Spec.ClusterID), "api/v1/namespaces/kube-public/configmaps/cluster-info", nil)
	if err != nil {
		return fmt.Errorf("verify the historical worker CA: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var info core.ConfigMap
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&info) != nil || info.Name != "cluster-info" || info.Namespace != "kube-public" {
		return fmt.Errorf("the member API did not provide its native cluster-info")
	}
	config, err := clientcmd.Load([]byte(info.Data["kubeconfig"]))
	if err != nil || len(config.Clusters) != 1 {
		return fmt.Errorf("the member cluster-info does not identify one Kubernetes CA")
	}
	for _, cluster := range config.Clusters {
		material := append([]byte(cluster.Server+"\n"), cluster.CertificateAuthorityData...)
		if cluster.Server != "" && len(cluster.CertificateAuthorityData) > 0 && fmt.Sprintf("%x", sha256.Sum256(material)) == digest {
			return nil
		}
	}
	return fmt.Errorf("the historical worker CA does not match the member cluster")
}

func legacyWorkerCheck(proof string) string {
	if proof == "" {
		return workerJoinIdentity
	}
	return readLegacyWorkerIdentity + `
[ ! -e "$marker.identity" ] &&
[ "$(legacy_worker_identity)" = ` + nodessh.Quote(proof) + ` ] ||
  { echo 'historical worker identity changed while verifying its ownership' >&2; exit 1; }
`
}
