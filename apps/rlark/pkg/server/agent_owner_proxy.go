package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleAgentOwnerRead mediates the cluster-scoped owner reads required by
// Raw and Docker admission. Agent service accounts deliberately have no
// cluster-wide Job or Cluster RBAC; this proxy exposes only the caller's
// authoritative Cluster and Jobs with a live, controller-owned Task there.
func (s *Server) handleAgentOwnerRead(c *gin.Context, certMeta map[string]string) bool {
	if certMeta[apis.MetaCertRole] != "agent" || certMeta[apis.MetaAgentID] == "" {
		return false
	}
	path := strings.Trim(c.Param("path"), "/")
	prefix := "apis/" + api.GroupVersion.String() + "/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 1 || len(parts) > 2 || (parts[0] != "jobs" && parts[0] != "clusters") {
		return false
	}
	if c.Request.Method != http.MethodGet || len(c.Request.URL.Query()) != 0 {
		writeAgentOwnerDenied(c)
		return true
	}
	if s.rlarkClient == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusServiceUnavailable, Reason: metav1.StatusReasonServiceUnavailable, Message: "management API unavailable"})
		return true
	}
	namespace := apis.RLarkAgentNamespacePrefix + certMeta[apis.MetaAgentID]
	ctx := c.Request.Context()

	clusters, err := s.rlarkClient.RlinfV1alpha1().Clusters().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusServiceUnavailable, Reason: metav1.StatusReasonServiceUnavailable, Message: "management API unavailable"})
		return true
	}
	ownedClusters := make([]api.Cluster, 0, 1)
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.Spec.AgentNamespace == namespace && cluster.DeletionTimestamp.IsZero() {
			ownedClusters = append(ownedClusters, *cluster.DeepCopy())
		}
	}
	if len(ownedClusters) > 1 {
		writeAgentOwnerDenied(c)
		return true
	}

	if parts[0] == "clusters" {
		if len(parts) == 2 {
			if len(ownedClusters) != 1 || ownedClusters[0].Name != parts[1] {
				writeAgentOwnerDenied(c)
				return true
			}
			ownedClusters[0].APIVersion, ownedClusters[0].Kind = api.GroupVersion.String(), "Cluster"
			c.JSON(http.StatusOK, &ownedClusters[0])
			return true
		}
		clusters.Items = ownedClusters
		clusters.APIVersion, clusters.Kind = api.GroupVersion.String(), "ClusterList"
		c.JSON(http.StatusOK, clusters)
		return true
	}
	if len(parts) != 2 || len(ownedClusters) != 1 {
		writeAgentOwnerDenied(c)
		return true
	}
	job, err := s.rlarkClient.RlinfV1alpha1().Jobs().Get(ctx, parts[1], metav1.GetOptions{})
	if err != nil || !job.DeletionTimestamp.IsZero() || job.Spec.Suspended || tenant.IDOf(job) != tenant.IDOf(&ownedClusters[0]) {
		writeAgentOwnerDenied(c)
		return true
	}
	tasks, err := s.rlarkClient.RlinfV1alpha1().Tasks(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusServiceUnavailable, Reason: metav1.StatusReasonServiceUnavailable, Message: "management API unavailable"})
		return true
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		owner := metav1.GetControllerOf(task)
		if owner == nil || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "Job" || owner.Name != job.Name || string(owner.UID) != string(job.UID) || task.Annotations[utils.ParentUIDAnnotation] != string(job.UID) || tenant.IDOf(task) != tenant.IDOf(job) || task.DeletionTimestamp != nil {
			continue
		}
		clusterUID := ""
		switch task.Spec.Runtime.Kind {
		case "Docker":
			if binding := task.Spec.Placement.DockerBinding; binding != nil && binding.TaskUID == string(task.UID) && binding.JobUID == string(job.UID) {
				clusterUID = binding.ClusterUID
			}
		case "Raw":
			if binding := task.Spec.Placement.Binding; binding != nil && binding.TaskUID == string(task.UID) && binding.JobUID == string(job.UID) {
				clusterUID = binding.ClusterUID
			}
		}
		if clusterUID != "" && clusterUID == string(ownedClusters[0].UID) {
			job = job.DeepCopy()
			job.APIVersion, job.Kind = api.GroupVersion.String(), "Job"
			c.JSON(http.StatusOK, job)
			return true
		}
	}
	writeAgentOwnerDenied(c)
	return true
}

func writeAgentOwnerDenied(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusForbidden, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "agent owner read is not authorized"})
}
