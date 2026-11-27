package workflow

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	"github.com/rlinf/rlark/apps/rlark/astribot/release"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationmember"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type reviewedObject struct {
	UID             types.UID `json:"uid" binding:"required"`
	ResourceVersion string    `json:"resource_version" binding:"required"`
}
type provisionMigration struct {
	ProvisionResourceVersion string                    `json:"provision_resource_version"`
	ProvisionUID             types.UID                 `json:"provision_uid" binding:"required"`
	Evidence                 string                    `json:"evidence" binding:"required,max=256"`
	Cluster                  *reviewedObject           `json:"cluster"`
	SiteNetwork              *reviewedObject           `json:"site_network"`
	Credentials              map[string]reviewedObject `json:"credentials"`
	NativeClusterUID         types.UID                 `json:"native_cluster_uid"`
}

type provisionCredentialRef struct{ namespace, name string }

func (g *Service) provisionCredentials(name string) map[string]provisionCredentialRef {
	return map[string]provisionCredentialRef{
		"station-read":       {stationMemberCredentialNamespace, stationmember.SecretName(name)},
		"station-operations": {stationOperationCredentialNamespace, stationmember.SecretName(name)},
		"release":            {release.CredentialNamespace, release.CredentialName(name)},
		"agent-certificate":  {g.namespace, "rlark-agent-cert-" + provisionAgentID(name)},
	}
}

// HandleMigrateClusterProvision records receipts for resources whose original
// ownership an administrator has verified against independent history.
func (g *Service) HandleMigrateClusterProvision(c *gin.Context) {
	var request provisionMigration
	if !g.http.DecodeOneJSON(c, 4096, &request, true) {
		return
	}
	if request.ProvisionUID == "" || strings.TrimSpace(request.Evidence) == "" || (request.Cluster == nil && request.SiteNetwork == nil && len(request.Credentials) == 0 && request.NativeClusterUID == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provision UID, evidence reference and at least one reviewed resource are required"})
		return
	}
	ctx := c.Request.Context()
	name := c.Param("name")
	if !g.provisions.start(name) {
		c.JSON(http.StatusConflict, gin.H{"error": "the provision is busy"})
		return
	}
	defer g.provisions.end(name)
	p, err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "read historical provision", err)
		return
	}
	if p.UID != request.ProvisionUID || (request.ProvisionResourceVersion != "" && p.ResourceVersion != request.ProvisionResourceVersion) || p.DeletionTimestamp != nil || p.Status.Phase == rlark.ClusterProvisionRunning {
		c.JSON(http.StatusConflict, gin.H{"error": "the provision UID changed or the operation is active"})
		return
	}
	actions, err := g.planLegacyProvision(ctx, p, request)
	if err != nil {
		g.http.RespondKubeError(c, "verify historical ownership: "+err.Error(), err)
		return
	}
	log.FromContext(ctx).WithName("audit").Info("historical provision ownership reviewed", "actor", g.http.Actor(c), "cluster", name, "provisionUID", p.UID, "evidence", request.Evidence)
	for _, action := range actions {
		if err := action(ctx); err != nil {
			g.http.RespondKubeError(c, "write historical ownership receipt: "+err.Error(), err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func verifyReviewedObject(object metav1.Object, reviewed reviewedObject, already bool) error {
	if reviewed.UID == "" || reviewed.ResourceVersion == "" || object.GetUID() != reviewed.UID || object.GetDeletionTimestamp() != nil ||
		(!already && object.GetResourceVersion() != reviewed.ResourceVersion) {
		return fmt.Errorf("the reviewed UID or resource version changed")
	}
	return nil
}

func (g *Service) planLegacyProvision(ctx context.Context, p *rlark.ClusterProvision, request provisionMigration) ([]func(context.Context) error, error) {
	reject := func(err error) error { return apierrors.NewConflict(rlark.Resource("clusterprovisions"), p.Name, err) }
	var actions []func(context.Context) error
	provisionChanged := false
	if request.NativeClusterUID != "" {
		if len(validation.IsDNS1123Label(string(request.NativeClusterUID))) != 0 || p.Status.HostKey == "" {
			return nil, reject(fmt.Errorf("a valid native kube-system UID and a recorded SSH fingerprint are required"))
		}
		if saved := p.Annotations[legacyNativeClusterUID]; saved != "" && saved != string(request.NativeClusterUID) {
			return nil, reject(fmt.Errorf("the original native cluster UID has already been recorded"))
		} else if saved == "" {
			metav1.SetMetaDataAnnotation(&p.ObjectMeta, legacyNativeClusterUID, string(request.NativeClusterUID))
			provisionChanged = true
		}
	}
	clusters := g.kubeClient.RlinfV1alpha1().Clusters()
	cluster, err := clusters.Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		if request.Cluster == nil {
			return nil, reject(fmt.Errorf("the live Cluster must be independently reviewed"))
		}
		savedCluster := p.Annotations[provisionClusterReceipt]
		savedOwner := cluster.Annotations[ownership.ProvisionUID]
		already := savedOwner == string(p.UID)
		bound := already && savedCluster == string(cluster.UID)
		if err := verifyReviewedObject(cluster, *request.Cluster, bound); err != nil {
			return nil, reject(err)
		}
		if !compatibleProvisionCluster(p, cluster) {
			return nil, reject(fmt.Errorf("the Cluster already has a different owner or namespace"))
		}
		if !already {
			metav1.SetMetaDataAnnotation(&cluster.ObjectMeta, ownership.ProvisionUID, string(p.UID))
			actions = append(actions, func(ctx context.Context) error {
				_, err := clusters.Update(ctx, cluster, metav1.UpdateOptions{})
				return err
			})
		}
		if savedCluster == "" {
			metav1.SetMetaDataAnnotation(&p.ObjectMeta, provisionClusterReceipt, string(cluster.UID))
			provisionChanged = true
		}
	} else if request.Cluster != nil {
		return nil, reject(fmt.Errorf("the reviewed Cluster no longer exists"))
	}
	refs := g.provisionCredentials(p.Name)
	for key, reviewed := range request.Credentials {
		ref, ok := refs[key]
		if !ok {
			return nil, reject(fmt.Errorf("unknown provision credential kind %q", key))
		}
		client := g.rawClient.CoreV1().Secrets(ref.namespace)
		secret, err := client.Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		already := ownership.CredentialMatches(secret, p.UID)
		if err := verifyReviewedObject(secret, reviewed, already); err != nil {
			return nil, reject(err)
		}
		if already {
			continue
		}
		if err := ownership.MarkReviewedCredential(secret, p.UID); err != nil {
			return nil, reject(err)
		}
		actions = append(actions, func(ctx context.Context) error {
			_, err := client.Update(ctx, secret, metav1.UpdateOptions{})
			return err
		})
	}
	if request.SiteNetwork != nil {
		object, err := stationidentity.ReadSiteNetwork(ctx, g.stationClient)
		if err != nil {
			return nil, err
		}
		key := siteProvisionAnnotation(p.Name)
		owner, receipt := provisionNetworkReceipt(p, object)
		if err := verifyReviewedObject(object, *request.SiteNetwork, owner == receipt); err != nil {
			return nil, reject(err)
		}
		if owner != "" && owner != receipt {
			return nil, reject(fmt.Errorf("the network declaration has a different owner"))
		}
		if owner == "" {
			if present, matches := legacyProvisionNetwork(p, object); !present || !matches {
				return nil, reject(fmt.Errorf("the reviewed SiteNetwork does not match the provision's original declaration"))
			}
			annotations := object.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[key] = receipt
			object.SetAnnotations(annotations)
			actions = append(actions, func(ctx context.Context) error {
				_, err := g.stationClient.Resource(siteNetworkGVR).Namespace(object.GetNamespace()).Update(ctx, object, metav1.UpdateOptions{})
				return err
			})
		}
	}
	if provisionChanged {
		actions = append(actions, func(ctx context.Context) error {
			_, err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Update(ctx, p, metav1.UpdateOptions{})
			return err
		})
	}

	return actions, nil
}

func compatibleProvisionCluster(p *rlark.ClusterProvision, cluster *rlark.Cluster) bool {
	savedCluster := p.Annotations[provisionClusterReceipt]
	savedOwner := cluster.Annotations[ownership.ProvisionUID]
	return cluster.Spec.AgentNamespace == p.Name && (savedCluster == "" || savedCluster == string(cluster.UID)) &&
		(savedOwner == "" || savedOwner == string(p.UID))
}

func provisionNetworkReceipt(p *rlark.ClusterProvision, object *unstructured.Unstructured) (owner, receipt string) {
	facts := p.Status.Facts
	return object.GetAnnotations()[siteProvisionAnnotation(p.Name)], siteProvisionReceipt(p.UID, object.GetUID(), "cp."+p.Name+".rlark.internal", facts.NodeName, facts.TunnelAddress, facts.PublicKey, p.Spec.Site)
}

// A historical declaration can be partially removed. Remaining entries must
// still agree with the provision, and sibling node and site records stay independent.
func legacyProvisionNetwork(p *rlark.ClusterProvision, object *unstructured.Unstructured) (present, matches bool) {
	f := p.Status.Facts
	matches = true
	n := provisioning.Network{Object: object}
	for _, entry := range n.Entries("spec", "names") {
		if entry["name"] == "cp."+p.Name+".rlark.internal" {
			present = true
			matches = matches && f.TunnelAddress != "" && entry["address"] == f.TunnelAddress
		}
	}
	for _, entry := range n.Entries("spec", "nodes") {
		if f.NodeName != "" && entry["clusterID"] == p.Name && entry["nodeName"] == f.NodeName {
			present = true
			matches = matches && f.PublicKey != "" && f.TunnelAddress != "" && entry["address"] == f.TunnelAddress && entry["publicKey"] == f.PublicKey
		}
	}
	for _, entry := range n.Entries("spec", "sites") {
		members, _, _ := unstructured.NestedStringSlice(entry, "clusters")
		if slices.Contains(members, p.Name) {
			present = true
			matches = matches && p.Spec.Site != "" && entry["name"] == p.Spec.Site
		}
	}
	return present, matches
}
