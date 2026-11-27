package workflow

import (
	"context"
	"fmt"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const legacyNativeClusterUID = "rlark.io/provisioned-native-cluster-uid"

func claimLegacyProvisionHost(owner, native types.UID) string {
	return "set -eu\nmarker=" + provisionHostMarker + "\n" + lockHostMarker + readHostClusterUID +
		"owner=" + nodessh.Quote(string(owner)) + "\nexpected=" + nodessh.Quote(string(native)) + `
if [ -e "$marker" ]; then
  case "$(cat "$marker")" in
    "cleared-$owner") return_already_cleared=yes;;
    "$owner") ;;
    *) echo 'control plane belongs to a different provision' >&2; exit 1;;
  esac
fi
if [ "${return_already_cleared:-}" != yes ]; then
  observed=$(native_cluster_uid)
  [ -n "$observed" ] && [ "$observed" = "$expected" ] ||
    { echo 'the historical native kube-system UID does not match' >&2; exit 1; }
  [ ! -e "$marker.cluster" ] || [ "$(cat "$marker.cluster")" = "$observed" ]
  (umask 077; printf '%s\n' "$observed" > "$marker.cluster")
  (umask 077; printf '%s\n' "$owner" > "$marker")
fi
`
}

// Preserve the evidence-bearing provision when cleanup encounters an
// unreviewed historical dependency. Newer records have an original Cluster
// receipt even after the Cluster is deleted.
func (g *Service) requireLegacyProvisionReceipts(ctx context.Context, p *rlark.ClusterProvision) error {
	reject := func(message string) error {
		return apierrors.NewConflict(rlark.Resource("clusterprovisions"), p.Name, fmt.Errorf("%s; review and migrate its ownership before clearing", message))
	}
	for _, ref := range g.provisionCredentials(p.Name) {
		secret, err := g.rawClient.CoreV1().Secrets(ref.namespace).Get(ctx, ref.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !ownership.CredentialMatches(secret, p.UID) {
			return reject("a historical provision credential has no ownership receipt")
		}
	}
	network, err := stationidentity.ReadSiteNetwork(ctx, g.stationClient)
	if err != nil {
		return err
	}
	if present, _ := legacyProvisionNetwork(p, network); present && network.GetAnnotations()[siteProvisionAnnotation(p.Name)] == "" {
		return reject("the historical site declaration has no ownership receipt")
	}
	return nil
}
