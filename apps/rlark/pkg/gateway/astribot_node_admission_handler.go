package gateway

import (
	"context"
	"encoding/json"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	nodeAdmissionGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "nodeadmissions"}
	nodeClassGVR     = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "nodeclasses"}
	siteNetworkGVR   = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "sitenetworks"}
)

const (
	maxNodeAdmissionRequestBytes = 16 * 1024
)

// stationBootstrap is Station API as the Gateway sees it when it gives out a
// join code: where a new machine reaches it, and the digests of what it
// serves, read over the Gateway's own verified connection. The machine's
// command pins those digests before it trusts anything it fetches.
type stationBootstrap interface {
	stationidentity.AdmissionBootstrap
	// Open streams what Station API serves at path (the installer, which takes minutes to send), and its length.
	Open(ctx context.Context, path string) (io.ReadCloser, int64, error)
	// BundleDigest is the SHA-256, in hex, of the installer, as Station API says it (/v1/bootstrap/bundle.sha256).
	BundleDigest(ctx context.Context) (string, error)
}

// decodeOneJSON reads a request body that is one JSON object of into's fields, at most limit bytes (or nothing,
// when emptyAllowed). On failure it has answered.
func decodeOneJSON(c *gin.Context, limit int64, into any, emptyAllowed bool) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(into)
	if err == io.EOF && emptyAllowed {
		return true
	}
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the request must be one JSON object of the documented fields"})
		return false
	}
	return true
}

// siteNetwork reads the one SiteNetwork; a management plane without one has no sites.
func (g *Gateway) siteNetwork(ctx context.Context) (*unstructured.Unstructured, error) {
	network, err := g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Get(ctx, "default", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &unstructured.Unstructured{Object: map[string]interface{}{}}, nil
	}
	return network, err
}

func (g *Gateway) stationAPIFor(network *unstructured.Unstructured, clusterID, nodeName string) string {
	return stationidentity.APIForAdmission(network, clusterID, nodeName, g.stationBootstrap)
}

// handleListNodeAdmissions answers GET /api/v1/station/node-admissions?cluster_id=:
// every admission of a cluster, newest code first.
func (g *Gateway) handleListNodeAdmissions(c *gin.Context) {
	clusterID, namespace, ok := g.clusterNamespace(c)
	if !ok {
		return
	}
	list, err := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		return
	}
	joins, err := g.nodeJoinsOf(c.Request.Context(), clusterID)
	if err != nil {
		respondKubeError(c, "list node joins: "+err.Error(), err)
		return
	}
	now := time.Now()
	items := make([]stationidentity.Admission, 0, len(list.Items))
	for i := range list.Items {
		view := stationidentity.AdmissionView(&list.Items[i], now)
		for _, j := range joins {
			if j.Spec.NodeName == view.NodeName {
				view.JoinPhase, view.JoinStage = string(j.Status.Phase), j.Status.Stage
			}
		}
		items = append(items, view)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IssuedAt != items[j].IssuedAt {
			return items[i].IssuedAt > items[j].IssuedAt
		}
		return items[i].NodeName < items[j].NodeName
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleIssueNodeAdmission answers POST /api/v1/station/node-admissions: it admits a
// machine as the named node and answers the code once. A node that has an
// admission keeps it, as what declares its tunnel address; a new code for it
// replaces what the admission declares and opens it again.
func (g *Gateway) handleIssueNodeAdmission(c *gin.Context) {
	var request stationidentity.IssueAdmissionRequest
	if !decodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, false) {
		return
	}
	namespace, err := g.stationNamespace(c, request.ClusterID)
	if err != nil {
		return
	}
	issued, status, refused := g.admissionService().Issue(c.Request.Context(), c.GetString(authSubjectKey), namespace, &request, nil)
	if refused != nil {
		respondAdmissionError(c, refused)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, issued)
}

func (g *Gateway) admissionService() stationidentity.AdmissionService {
	return stationidentity.AdmissionService{
		SiteNetwork: g.siteNetwork,
		Admissions: func(namespace string) dynamic.ResourceInterface {
			return g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
		},
		NodeClasses: g.stationClient.Resource(nodeClassGVR),
		Bootstrap:   g.stationBootstrap,
	}
}

func respondAdmissionError(c *gin.Context, refused *stationidentity.AdmissionError) {
	c.JSON(refused.Status, gin.H{"error": refused.Error()})
}

// handleReissueNodeAdmission answers POST /api/v1/station/node-admissions/:name/reissue:
// a new code for what the admission declares; the earlier code stops working.
func (g *Gateway) handleReissueNodeAdmission(c *gin.Context) {
	var request stationidentity.ReissueAdmissionRequest
	if !decodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, true) {
		return
	}
	ttl, err := stationidentity.JoinCodeTTL(request.TTLSeconds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	admission, resource, ok := g.existingNodeAdmission(c)
	if !ok {
		return
	}
	issued, refused := g.admissionService().Reissue(c.Request.Context(), c.GetString(authSubjectKey), admission, resource, ttl)
	if refused != nil {
		respondAdmissionError(c, refused)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, issued)
}

// handleRevokeNodeAdmission answers POST /api/v1/station/node-admissions/:name/revoke:
// the code stops working now: its expiry is set to now, which Station API and
// station-controller already treat as the end of the code. A code a machine
// has redeemed is that machine's; it is not withdrawn here.
func (g *Gateway) handleRevokeNodeAdmission(c *gin.Context) {
	admission, resource, ok := g.existingNodeAdmission(c)
	if !ok {
		return
	}
	view, refused := g.admissionService().Revoke(c.Request.Context(), c.GetString(authSubjectKey), admission, resource)
	if refused != nil {
		respondAdmissionError(c, refused)
		return
	}
	c.JSON(http.StatusOK, gin.H{"admission": view})
}

func (g *Gateway) existingNodeAdmission(c *gin.Context) (*unstructured.Unstructured, dynamic.ResourceInterface, bool) {
	_, namespace, ok := g.clusterNamespace(c)
	if !ok {
		return nil, nil, false
	}
	resource := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
	admission, err := resource.Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "node admission not found"})
		return nil, nil, false
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		return nil, nil, false
	}
	return admission, resource, true
}
