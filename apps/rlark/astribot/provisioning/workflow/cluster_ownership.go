package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const provisionClusterReceipt = "rlark.io/provisioned-cluster-uid"

// Reserve the name before agent registration can create a Cluster independently.
// A later attempt to reserve the same name needs the same provisioning receipt.
func (g *Service) reserveProvisionCluster(ctx context.Context, p *rlarkv1alpha1.ClusterProvision) (types.UID, error) {
	if p.UID == "" {
		return "", fmt.Errorf("cluster provision %s has no UID", p.Name)
	}
	client := g.kubeClient.RlinfV1alpha1().Clusters()
	cluster, err := client.Get(ctx, p.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && p.Annotations[provisionClusterReceipt] == "" {
		cluster, err = client.Create(ctx, &rlarkv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Annotations: map[string]string{ownership.ProvisionUID: string(p.UID)}},
			Spec:       rlarkv1alpha1.ClusterSpec{AgentNamespace: p.Name},
		}, metav1.CreateOptions{})
	}
	if err != nil {
		return "", err
	}
	if cluster.UID == "" || cluster.DeletionTimestamp != nil || cluster.Spec.AgentNamespace != p.Name ||
		cluster.Annotations[ownership.ProvisionUID] != string(p.UID) {
		return "", fmt.Errorf("cluster %s does not belong to this provision", p.Name)
	}
	provisions := g.kubeClient.RlinfV1alpha1().ClusterProvisions()
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := provisions.Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != p.UID || current.DeletionTimestamp != nil {
			return fmt.Errorf("cluster provision %s was replaced", p.Name)
		}
		if uid := current.Annotations[provisionClusterReceipt]; uid != "" {
			if uid == string(cluster.UID) {
				return nil
			}
			return fmt.Errorf("cluster %s was replaced", p.Name)
		}
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, provisionClusterReceipt, string(cluster.UID))
		_, err = provisions.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
	return cluster.UID, err
}

func (g *Service) checkProvisionCluster(ctx context.Context, p *rlarkv1alpha1.ClusterProvision, uid types.UID) error {
	cluster, err := g.kubeClient.RlinfV1alpha1().Clusters().Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if uid == "" || p.UID == "" || cluster.UID != uid || cluster.DeletionTimestamp != nil ||
		cluster.Spec.AgentNamespace != p.Name || cluster.Annotations[ownership.ProvisionUID] != string(p.UID) {
		return fmt.Errorf("the live Cluster UID is not bound to this provision")
	}
	return nil
}

func siteProvisionAnnotation(cluster string) string {
	// A DNS label can be 63 characters, so hash it into the annotation name.
	hash := sha256.Sum256([]byte(cluster))
	return fmt.Sprintf("rlark.io/site-provision-%x", hash[:16])
}

func siteProvisionReceipt(provision, network types.UID, name, node, address, key, site string) string {
	payload, _ := json.Marshal([]string{string(provision), string(network), name, node, address, key, site})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

const provisionHostMarker = "/var/lib/rlark/cluster-provision.uid"

func claimProvisionHost(uid types.UID) string {
	return claimHostMarker(provisionHostMarker, uid, `[ ! -d /var/lib/rlark/control-plane ]`)
}

func claimHostMarker(path string, uid types.UID, unused string) string {
	return "set -eu\nowner=" + nodessh.Quote(string(uid)) + "\nmarker=" + nodessh.Quote(path) + `
test -n "$owner"
install -d /var/lib/rlark
exec 8>"$marker.lock"
flock -x 8
if [ -e "$marker" ]; then
  saved=$(cat "$marker")
  case "$saved" in
    "$owner") ;;
    cleared-*) (umask 077; printf '%s\n' "$owner" > "$marker");;
    *) echo 'host belongs to another attempt' >&2; exit 1;;
  esac
else
  ` + unused + `
  (umask 077; printf '%s\n' "$owner" > "$marker")
fi
`
}

const verifyHostMarker = `
exec 8>"$marker.lock"
flock -x 8
if [ -f "$marker" ] && [ "$(cat "$marker")" = "cleared-$owner" ]; then
  echo cleared=yes
  exit 0
fi
if [ ! -f "$marker" ] || [ "$(cat "$marker")" != "$owner" ]; then
  echo 'host has no matching ownership record' >&2
  exit 1
fi
`

const finishHostCleanup = `
rm -f "$marker.cluster"
printf 'cleared-%s\n' "$owner" > "$marker"
echo cleared=yes
`

// kube-system supplies a stable UID for the Kubernetes installation on the
// host. A marker alone can survive an unrelated reinstall on the same machine.
const readHostClusterUID = `
native_cluster_uid() {
  timeout 15 ` + provisioning.BootstrapDir + `/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get namespace kube-system -o jsonpath='{.metadata.uid}'
}
`

func recordProvisionHostCluster(uid types.UID) string {
	return "set -eu\nowner=" + nodessh.Quote(string(uid)) + "\nmarker=" + provisionHostMarker + "\n" + verifyHostMarker + readHostClusterUID + `
native=$(native_cluster_uid)
test -n "$native"
if [ -e "$marker.cluster" ]; then
  test "$(cat "$marker.cluster")" = "$native"
else
  (umask 077; printf '%s\n' "$native" > "$marker.cluster")
fi
`
}

const verifyProvisionHostCluster = readHostClusterUID + `
if [ -e "$marker.cluster" ] || [ -e /etc/kubernetes/admin.conf ]; then
  test -s "$marker.cluster"
  native=$(native_cluster_uid)
  test -n "$native"
  test "$native" = "$(cat "$marker.cluster")"
fi
`
