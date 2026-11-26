package stations

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
)

// ObservationRoutes is assembled by the Gateway after applying admin middleware.
type ObservationRoutes struct {
	StationClient         dynamic.Interface
	Clusters, Enrollments schema.GroupVersionResource
	ClusterNamespace      func(*gin.Context) (string, string, bool)
	ReadMember            func(context.Context, string, string) (*http.Response, error)
	RespondError          func(*gin.Context, int, ErrorKind, string)
	RespondKubeError      func(*gin.Context, string, error)
}

// ErrorKind describes the failure without coupling station routes to Gateway error codes.
type ErrorKind int

const (
	InvalidQuery ErrorKind = iota
	Conflict
	NotFound
	UpstreamUnavailable
	InvalidRequest
	InvalidBody
	Forbidden
	RobotBusy
)

func (r ObservationRoutes) Register(admin *gin.RouterGroup) {
	admin.GET("/:name/running-images", r.runningImages)
	admin.GET("/:name/body-state", r.bodyState)
}

type stationRunningImage struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Image     string `json:"image"`
	ImageID   string `json:"imageID,omitempty"`
}

// runningImages returns an admin-only snapshot of observed container
// images from the member cluster that owns the selected station record.
func (r ObservationRoutes) runningImages(c *gin.Context) {
	name, uid := c.Param("name"), c.Query("node_uid")
	if len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsDNS1123Subdomain(c.Query("cluster_id"))) > 0 || uid == "" {
		r.RespondError(c, http.StatusBadRequest, InvalidQuery, "valid node name, cluster_id and node_uid are required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	clusterID := c.Query("cluster_id")
	upstreamError := func(err error) {
		log.FromContext(ctx).Error(err, "Cannot read station running images", "cluster", clusterID, "node", name)
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, "Running container images are unavailable")
	}
	agentID, ok := r.resolveStation(c, name, uid, upstreamError)
	if !ok {
		return
	}

	// A Pod names its node without a UID. Check the member Node on both
	// sides of the read so name reuse during the snapshot cannot mix instances.
	verifyNode := func() bool {
		var node corev1.Node
		status, err := r.readMemberObject(ctx, agentID, "/api/v1/nodes/"+url.PathEscape(name), &node)
		switch {
		case status == http.StatusNotFound:
			r.RespondError(c, http.StatusNotFound, NotFound, "Node is absent from the member cluster")
		case err != nil:
			upstreamError(err)
		case node.Name != name || string(node.UID) != uid:
			r.RespondError(c, http.StatusConflict, Conflict, "Member node identity does not match the site node record")
		default:
			return true
		}
		return false
	}
	if !verifyNode() {
		return
	}
	query := url.Values{"fieldSelector": {fields.OneTermEqualSelector("spec.nodeName", name).String()}, "limit": {"200"}}
	images := []stationRunningImage{}
	for {
		var page corev1.PodList
		if _, err := r.readMemberObject(ctx, agentID, "/api/v1/pods?"+query.Encode(), &page); err != nil {
			upstreamError(err)
			return
		}
		for _, pod := range page.Items {
			if pod.Spec.NodeName != name || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				continue
			}
			for _, statuses := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
				for _, status := range statuses {
					if status.State.Running != nil {
						images = append(images, stationRunningImage{pod.Namespace, pod.Name, status.Name, status.Image, status.ImageID})
					}
				}
			}
		}
		if page.Continue == "" {
			break
		}
		if page.Continue == query.Get("continue") {
			upstreamError(fmt.Errorf("member Pod list repeated its continuation token"))
			return
		}
		query.Set("continue", page.Continue)
	}
	if !verifyNode() {
		return
	}
	slices.SortFunc(images, func(a, b stationRunningImage) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Pod, b.Pod), cmp.Compare(a.Container, b.Container))
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": images})
}

// resolveStation verifies the management namespace and the enrollment for an observed node instance.
func (r ObservationRoutes) resolveStation(c *gin.Context, name, uid string, upstreamError func(error)) (string, bool) {
	ctx := c.Request.Context()
	clusterID, namespace, ok := r.ClusterNamespace(c)
	if !ok {
		return "", false
	}
	agentID, prefixed := strings.CutPrefix(namespace, apis.RLarkAgentNamespacePrefix)
	if !prefixed || len(validation.IsDNS1123Label(agentID)) > 0 {
		r.RespondError(c, http.StatusConflict, Conflict, "Cluster namespace does not identify a Kubernetes agent")
		return "", false
	}

	clusters, err := r.StationClient.Resource(r.Clusters).List(ctx, metav1.ListOptions{})
	if err != nil {
		upstreamError(err)
		return "", false
	}
	owner, conflicted := "", false
	for _, cluster := range clusters.Items {
		if claims, _, _ := unstructured.NestedString(cluster.Object, "spec", "agentNamespace"); claims == namespace {
			conflicted = owner != "" && owner != cluster.GetName()
			owner = cluster.GetName()
			if conflicted {
				break
			}
		}
	}
	if conflicted || owner != clusterID {
		r.RespondError(c, http.StatusConflict, Conflict, "Cluster namespace ownership could not be verified")
		return "", false
	}

	record, err := r.StationClient.Resource(r.Enrollments).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		r.RespondKubeError(c, "Could not read site node record", err)
		return "", false
	}
	boundUID, _, _ := unstructured.NestedString(record.Object, "spec", "nodeUID")
	recordCluster, _, _ := unstructured.NestedString(record.Object, "spec", "clusterID")
	retired, _, _ := unstructured.NestedBool(record.Object, "spec", "retired")
	if retired || boundUID != uid || recordCluster != clusterID {
		r.RespondError(c, http.StatusConflict, Conflict, "Site node record no longer matches this node instance")
		return "", false
	}
	return agentID, true
}

func (r ObservationRoutes) readMemberObject(ctx context.Context, agentID, path string, object any) (int, error) {
	response, err := r.ReadMember(ctx, agentID, path)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.FromContext(ctx).V(1).Info("Could not close member Kubernetes response body", "error", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, fmt.Errorf("member Kubernetes API returned HTTP %d", response.StatusCode)
	}
	return response.StatusCode, json.NewDecoder(response.Body).Decode(object)
}
