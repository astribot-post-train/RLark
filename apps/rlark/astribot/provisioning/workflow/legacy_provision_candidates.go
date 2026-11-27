package workflow

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	migrationReview   = "review"
	migrationRecorded = "recorded"
	migrationAbsent   = "absent"
	migrationBlocked  = "blocked"
)

type migrationCandidate struct {
	Key       string          `json:"key"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace,omitempty"`
	CreatedAt *metav1.Time    `json:"created_at,omitempty"`
	State     string          `json:"state"`
	Identity  *reviewedObject `json:"identity,omitempty"`
}

type migrationCandidates struct {
	ProvisionUID             types.UID            `json:"provision_uid"`
	ProvisionResourceVersion string               `json:"provision_resource_version"`
	Candidates               []migrationCandidate `json:"candidates"`
	NativeState              string               `json:"native_state"`
}

func describeMigrationCandidate(key, name, namespace string, object metav1.Object, blocked, recorded bool) migrationCandidate {
	result := migrationCandidate{Key: key, Name: name, Namespace: namespace, State: migrationAbsent}
	if object == nil {
		return result
	}
	if created := object.GetCreationTimestamp(); !created.IsZero() {
		result.CreatedAt = &created
	}
	switch {
	case blocked || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil:
		result.State = migrationBlocked
	case recorded:
		result.State = migrationRecorded
	default:
		result.State = migrationReview
	}
	if result.State != migrationBlocked {
		result.Identity = &reviewedObject{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}
	}
	return result
}

func (g *Service) migrationProvision(c *gin.Context) (*rlark.ClusterProvision, bool) {
	p, err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "read historical provision", err)
		return nil, false
	}
	if p.UID == "" || p.ResourceVersion == "" || p.DeletionTimestamp != nil || p.Status.Phase == rlark.ClusterProvisionRunning {
		c.JSON(http.StatusConflict, gin.H{"error": "the provision is active or has no stable identity"})
		return nil, false
	}
	return p, true
}

// HandleClusterProvisionMigrationCandidates reads metadata for one historical
// provision. It returns candidates for human review, never an ownership receipt.
func (g *Service) HandleClusterProvisionMigrationCandidates(c *gin.Context) {
	p, ok := g.migrationProvision(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	c.Header("Cache-Control", "no-store")
	result := migrationCandidates{ProvisionUID: p.UID, ProvisionResourceVersion: p.ResourceVersion, NativeState: migrationBlocked}
	if p.Annotations[legacyNativeClusterUID] != "" {
		result.NativeState = migrationRecorded
	} else if p.Status.HostKey != "" && p.Spec.Machine.Address != "" {
		result.NativeState = migrationReview
	}
	cluster, err := g.kubeClient.RlinfV1alpha1().Clusters().Get(ctx, p.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		result.Candidates = append(result.Candidates, describeMigrationCandidate("cluster", p.Name, "", cluster, !compatibleProvisionCluster(p, cluster),
			p.Annotations[provisionClusterReceipt] == string(cluster.UID) && cluster.Annotations[ownership.ProvisionUID] == string(p.UID)))
	case apierrors.IsNotFound(err):
		result.Candidates = append(result.Candidates, describeMigrationCandidate("cluster", p.Name, "", nil, false, false))
	default:
		g.http.RespondKubeError(c, "read migration Cluster candidate", err)
		return
	}

	network, err := stationidentity.ReadSiteNetwork(ctx, g.stationClient)
	switch {
	case err == nil:
		present, matches := legacyProvisionNetwork(p, network)
		owner, receipt := provisionNetworkReceipt(p, network)
		if !present && owner == "" {
			result.Candidates = append(result.Candidates, describeMigrationCandidate("site-network", "default", "station-system", nil, false, false))
		} else {
			result.Candidates = append(result.Candidates, describeMigrationCandidate("site-network", network.GetName(), network.GetNamespace(), network,
				(owner != "" && owner != receipt) || (owner == "" && !matches), owner == receipt))
		}
	case apierrors.IsNotFound(err):
		result.Candidates = append(result.Candidates, describeMigrationCandidate("site-network", "default", "station-system", nil, false, false))
	default:
		g.http.RespondKubeError(c, "read migration SiteNetwork candidate", err)
		return
	}

	refs := g.provisionCredentials(p.Name)
	for _, key := range slices.Sorted(maps.Keys(refs)) {
		ref := refs[key]
		secret, err := g.rawClient.CoreV1().Secrets(ref.namespace).Get(ctx, ref.name, metav1.GetOptions{})
		switch {
		case err == nil:
			recorded := ownership.CredentialMatches(secret, p.UID)
			blocked := ownership.MarkReviewedCredential(secret.DeepCopy(), p.UID) != nil
			result.Candidates = append(result.Candidates, describeMigrationCandidate(key, ref.name, ref.namespace, secret, blocked, recorded))
		case apierrors.IsNotFound(err):
			result.Candidates = append(result.Candidates, describeMigrationCandidate(key, ref.name, ref.namespace, nil, false, false))
		default:
			g.http.RespondKubeError(c, "read migration credential candidate", err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

type migrationHostRequest struct {
	ProvisionUID             types.UID `json:"provision_uid"`
	ProvisionResourceVersion string    `json:"provision_resource_version"`
	Password                 string    `json:"password"`
}

// HandleClusterProvisionMigrationHost reads the native kube-system UID on the
// provision's pinned host. Neither SSH credentials nor the remote output are logged.
func (g *Service) HandleClusterProvisionMigrationHost(c *gin.Context) {
	var request migrationHostRequest
	if !g.http.DecodeOneJSON(c, 4096, &request, true) {
		return
	}
	password := []byte(request.Password)
	request.Password = ""
	defer clear(password)
	if len(password) == 0 || request.ProvisionUID == "" || request.ProvisionResourceVersion == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SSH password and a current migration preview are required"})
		return
	}
	p, ok := g.migrationProvision(c)
	if !ok {
		return
	}
	if p.UID != request.ProvisionUID || p.ResourceVersion != request.ProvisionResourceVersion || p.Status.HostKey == "" || p.Spec.Machine.Address == "" || p.Annotations[legacyNativeClusterUID] != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "the original host or provision changed; review the current candidates"})
		return
	}
	ctx := c.Request.Context()
	machine, _, err := g.provisionDial(ctx, p.Spec.Machine, password, p.Status.HostKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not connect to the recorded SSH host"})
		return
	}
	defer func() { _ = machine.Close() }()
	output, err := machine.Run(ctx, "set -eu\n"+readHostClusterUID+"\nnative_cluster_uid\n", nil, nil)
	uid := strings.TrimSpace(output)
	if err != nil || len(validation.IsDNS1123Label(uid)) != 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not verify the host's native kube-system UID"})
		return
	}
	current, err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "recheck historical provision", err)
		return
	}
	if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion || current.DeletionTimestamp != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "the provision changed during host inspection"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"uid": uid}})
}
