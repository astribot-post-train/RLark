package workflow

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	"github.com/rlinf/rlark/apps/rlark/astribot/release"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationmember"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// clearClusterProvisionRequest takes away a making that stopped, or what the making of a cluster that was deleted
// left. Password logs in to its machine to put it back as it was; MachineGone says the machine is not there to be
// put back (reinstalled, returned), and only RLark's own records go.
type clearClusterProvisionRequest struct {
	Password    string `json:"password"`
	User        string `json:"user"`
	MachineGone bool   `json:"machine_gone"`
}

// clearMachineScript undoes on the machine what the stages did there: the control plane this installer made (which
// refuses while other nodes are in its cluster, and keeps a last snapshot), and the tunnel. An installer still
// running there is one whose stage was cut off (the Gateway restarted): no making runs now, so it is stopped first;
// this script and the shells it runs under hold the installer's name in their own command lines and are left out.
// The machine keeps its tunnel key and the installer: a making that starts over finds the same machine.
const clearMachineScript = `set -eu
marker=` + provisionHostMarker + "\n" + lockHostMarker + allowUnclaimedHostStart + provisionHostUnused + allowUnclaimedHostEnd + verifyLockedHostMarker + verifyProvisionHostCluster + `B=` + provisioning.BootstrapDir + `
mine=" "; p=$$; while [ "$p" -gt 1 ]; do mine="$mine$p "; p=$(awk '{print $4}' /proc/$p/stat); done
for p in $(pgrep -f 'control-plane\.sh|station-bootstrap control-plane' || true); do
  case "$mine" in *" $p "*) ;; *) kill "$p" 2>/dev/null || true ;; esac
done
pkill -x kubeadm || true
if [ -x $B/station-bootstrap ] && [ -d /var/lib/rlark/control-plane ]; then (cd $B && ./station-bootstrap control-plane undo); fi
systemctl disable --now rlark-station-gateway >/dev/null 2>&1 || true
rm -f /etc/systemd/system/rlark-station-gateway.service /etc/rlark/station/bootstrap.json
rm -rf /var/lib/station/wg
ip link del st-wg0 2>/dev/null || true
systemctl daemon-reload
` + finishHostCleanup

// HandleClearClusterProvision answers POST /api/v1/cluster-provisions/:name/clear: a making that stopped half way,
// or the record of one whose cluster was deleted, is taken away so that the site, the cluster's name and the machine
// can be used again. Until then they are held: the SiteNetwork has the site, the control plane's name and the hub by
// its key, and a new cluster of the name is refused. It answers what it did, a line a step.
func (g *Service) HandleClearClusterProvision(c *gin.Context) {
	var req clearClusterProvisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		g.http.RespondError(c, http.StatusBadRequest, g.http.CodeInvalidRequestBody, err.Error())
		return
	}
	ctx := c.Request.Context()
	provisions := g.kubeClient.RlinfV1alpha1().ClusterProvisions()
	p, err := provisions.Get(ctx, c.Param("name"), metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "get cluster provision: "+err.Error(), err)
		return
	}
	cluster, err := g.kubeClient.RlinfV1alpha1().Clusters().Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		g.http.RespondKubeError(c, err.Error(), err)
		return
	}
	listed := err == nil
	reached := slices.ContainsFunc(p.Status.Stages, func(s rlarkv1alpha1.ClusterProvisionStage) bool {
		return s.Name == stageControlPlane && s.State != rlarkv1alpha1.ClusterProvisionStagePending
	})
	switch {
	case p.UID == "":
		g.http.RespondError(c, http.StatusConflict, g.http.CodeResourceConflict, "the provision has no UID")
		return
	case listed && (cluster.UID == "" || string(cluster.UID) != p.Annotations[provisionClusterReceipt] ||
		cluster.Annotations[ownership.ProvisionUID] != string(p.UID)):
		g.http.RespondError(c, http.StatusConflict, g.http.CodeResourceConflict, "the live Cluster UID is not bound to this provision")
		return
	case p.Status.Phase == rlarkv1alpha1.ClusterProvisionSucceeded && listed:
		g.http.RespondError(c, http.StatusConflict, g.http.CodeResourceConflict, "cluster "+p.Name+" is made and listed: delete the cluster first; clearing then takes away what its making declared")
		return
	case reached && req.Password == "" && !req.MachineGone:
		g.http.RespondError(c, http.StatusBadRequest, g.http.CodeInvalidRequest, "the making reached the machine: give its SSH password so that the machine is put back too, or machine_gone when it is not there any more")
		return
	case !g.provisions.start(p.Name):
		g.http.RespondError(c, http.StatusConflict, g.http.CodeResourceConflict, "the making of "+p.Name+" is running")
		return
	}
	defer g.provisions.end(p.Name)
	password := []byte(req.Password)
	defer clear(password)
	if user := strings.TrimSpace(req.User); user != "" {
		p.Spec.Machine.User = user
	}
	if p.Status.Phase == rlarkv1alpha1.ClusterProvisionRunning {
		g.http.RespondError(c, http.StatusConflict, g.http.CodeResourceConflict, "the provision is running")
		return
	}
	var clusterUID types.UID
	if listed {
		clusterUID = cluster.UID
	}
	if !reached {
		password = nil
	}
	steps, err := g.clearProvision(ctx, p, password, clusterUID)
	if err != nil {
		status := http.StatusBadGateway
		if apierrors.IsConflict(err) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"success": false, "message": err.Error(), "steps": steps})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "steps": steps})
}

// clearProvision releases verified host and management state, then removes
// the original Cluster and provisioning record with delete preconditions.
func (g *Service) clearProvision(ctx context.Context, p *rlarkv1alpha1.ClusterProvision, password []byte, clusterUID types.UID) ([]string, error) {
	var steps []string
	facts := p.Status.Facts
	if len(password) > 0 && p.UID != "" && p.Status.HostKey != "" {
		machine, _, err := g.provisionDial(ctx, p.Spec.Machine, password, p.Status.HostKey)
		if err != nil {
			return steps, fmt.Errorf("log in to %s@%s: %w", p.Spec.Machine.User, p.Spec.Machine.Address, err)
		}
		out, err := machine.Run(ctx, "owner="+nodessh.Quote(string(p.UID))+"\n"+clearMachineScript, nil, nil)
		_ = machine.Close()
		if err != nil {
			return steps, fmt.Errorf("put the machine back: %w", err)
		}
		steps = append(steps, "机器 "+p.Spec.Machine.Address+"：撤掉这次建的控制面和隧道（"+provisioning.LastLine(out, "control plane")+"）；隧道密钥和安装包留在机器上")
	} else {
		steps = append(steps, "机器 "+p.Spec.Machine.Address+" 没动（没给密码或已不在）：它要再用，先在机器上撤掉旧控制面")
	}
	credentials := []struct{ namespace, name string }{
		{stationMemberCredentialNamespace, stationmember.SecretName(p.Name)},
		{stationOperationCredentialNamespace, stationmember.SecretName(p.Name)},
		{release.CredentialNamespace, release.CredentialName(p.Name)},
		{g.namespace, "rlark-agent-cert-" + provisionAgentID(p.Name)},
	}
	for _, credential := range credentials {
		namespace, name := credential.namespace, credential.name
		client := g.rawClient.CoreV1().Secrets(namespace)
		secret, err := client.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return steps, fmt.Errorf("read credential %s/%s: %w", namespace, name, err)
		}
		if !ownership.CredentialMatches(secret, p.UID) {
			steps = append(steps, fmt.Sprintf("保留凭据 %s/%s：没有匹配的 UID 归属记录", namespace, name))
			continue
		}
		opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}}
		if err := client.Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
			return steps, fmt.Errorf("delete the credential %s/%s: %w", namespace, name, err)
		}
		steps = append(steps, fmt.Sprintf("删掉本次创建的凭据 %s/%s", namespace, name))
	}
	// Station admissions and enrollments belong to individual nodes.
	// Provisioning does not own that namespace's records.
	controlPlaneName := "cp." + p.Name + ".rlark.internal"
	clearedSite := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := stationidentity.ReadSiteNetwork(ctx, g.stationClient)
		if err != nil {
			return err
		}
		receiptKey := siteProvisionAnnotation(p.Name)
		annotations := object.GetAnnotations()
		if p.UID == "" || object.GetUID() == "" || annotations[receiptKey] !=
			siteProvisionReceipt(p.UID, object.GetUID(), controlPlaneName, facts.NodeName, facts.TunnelAddress, facts.PublicKey, p.Spec.Site) {
			return nil
		}
		network := provisioning.Network{Object: object}
		var names, sites, nodes []interface{}
		for _, entry := range network.Entries("spec", "names") {
			if entry["name"] != controlPlaneName || entry["address"] != facts.TunnelAddress {
				names = append(names, entry)
			}
		}
		for _, entry := range network.Entries("spec", "sites") {
			clusters, _, _ := unstructured.NestedStringSlice(entry, "clusters")
			if entry["name"] != p.Spec.Site || !slices.Contains(clusters, p.Name) {
				sites = append(sites, entry)
				continue
			}
			if clusters = slices.DeleteFunc(clusters, func(cluster string) bool { return cluster == p.Name }); len(clusters) > 0 {
				if err := unstructured.SetNestedStringSlice(entry, clusters, "clusters"); err != nil {
					return err
				}
				sites = append(sites, entry)
			}
		}
		for _, entry := range network.Entries("spec", "nodes") {
			if entry["clusterID"] != p.Name || entry["nodeName"] != facts.NodeName || entry["publicKey"] != facts.PublicKey ||
				entry["address"] != facts.TunnelAddress {
				nodes = append(nodes, entry)
			}
		}
		for path, value := range map[string][]interface{}{"names": names, "sites": sites, "nodes": nodes} {
			if err := unstructured.SetNestedSlice(object.Object, value, "spec", path); err != nil {
				return err
			}
		}
		delete(annotations, receiptKey)
		object.SetAnnotations(annotations)
		_, err = g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Update(ctx, object, metav1.UpdateOptions{})
		clearedSite = err == nil
		return err
	})
	if err != nil {
		return steps, fmt.Errorf("take the making's declarations out of the SiteNetwork: %w", err)
	}
	if clearedSite {
		steps = append(steps, "SiteNetwork：已清理本次创建的站点声明")
	} else {
		steps = append(steps, "SiteNetwork：没有匹配的 UID 归属记录，保留现有声明")
	}
	if clusterUID != "" {
		opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &clusterUID}}
		if err := g.kubeClient.RlinfV1alpha1().Clusters().Delete(ctx, p.Name, opts); err != nil && !apierrors.IsNotFound(err) {
			return steps, fmt.Errorf("delete the Cluster the making listed: %w", err)
		}
		steps = append(steps, "删掉本次创建的集群 "+p.Name)
	}
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID, ResourceVersion: &p.ResourceVersion}}
	if err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Delete(ctx, p.Name, opts); err != nil && !apierrors.IsNotFound(err) {
		return steps, fmt.Errorf("delete the record of the making: %w", err)
	}
	return append(steps, "删掉这次新建的记录"), nil
}
