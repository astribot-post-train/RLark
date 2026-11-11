package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	raykubernetes "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/workload/ray/kubernetes"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
)

type exposedPortStatus struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	TargetPort int32  `json:"targetPort"`
	NodePort   int32  `json:"nodePort,omitempty"`
}

func (g *Gateway) handleTaskExposedPorts(c *gin.Context) {
	taskName := c.Param("name")
	tasks, err := g.kubeClient.RlinfV1alpha1().Tasks(metav1.NamespaceAll).List(c.Request.Context(), metav1.ListOptions{FieldSelector: "metadata.name=" + taskName})
	if err != nil {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "cannot read Task")
		return
	}
	if len(tasks.Items) == 0 {
		respondError(c, http.StatusNotFound, CodeResourceNotFound, "Task not found")
		return
	}
	if len(tasks.Items) != 1 {
		respondError(c, http.StatusConflict, CodeInvalidRequest, "Task name is ambiguous across clusters")
		return
	}
	task := &tasks.Items[0]
	scope, ok := g.tenantScopeFor(c, "tasks")
	if !ok {
		return
	}
	if !authorizeTenantObject(c, scope, tenant.IDOf(task)) {
		return
	}
	var ray api.RayExecutionParametersV1
	if err := json.Unmarshal(task.Spec.Workload.Parameters.Raw, &ray); err != nil || len(ray.ExposedPorts) == 0 {
		c.JSON(http.StatusOK, gin.H{"ports": []exposedPortStatus{}, "nodes": task.Status.ObservedNodes})
		return
	}
	agentID, ok := strings.CutPrefix(task.Namespace, apis.RLarkAgentNamespacePrefix)
	if !ok {
		respondError(c, http.StatusServiceUnavailable, CodeUpstreamUnavailable, "Task has no data-plane Agent")
		return
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", url.PathEscape(tenant.DefaultNamespace), url.PathEscape(raykubernetes.ExposedPortServiceName(task.Name)))
	resp, err := g.proxyKubeRequest(c.Request.Context(), http.MethodGet, agentID, path, nil)
	if err != nil {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "cannot reach data-plane Agent")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		c.JSON(http.StatusOK, gin.H{"ports": []exposedPortStatus{}, "nodes": task.Status.ObservedNodes, "state": "pending", "message": task.Status.Message})
		return
	}
	if resp.StatusCode != http.StatusOK {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "cannot read exposed-port Service")
		return
	}
	var service corev1.Service
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&service); err != nil {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "invalid Service response")
		return
	}
	if !validExposedPortService(&service, task.Name, string(task.UID), ray.ExposedPorts) {
		respondError(c, http.StatusConflict, CodeInvalidRequest, "exposed-port Service ownership mismatch")
		return
	}
	ports := make([]exposedPortStatus, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		ports = append(ports, exposedPortStatus{Name: port.Name, Protocol: string(port.Protocol), TargetPort: port.Port, NodePort: port.NodePort})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ports": ports, "nodes": task.Status.ObservedNodes, "namespace": task.Namespace, "state": "ready"})
}

func validExposedPortService(service *corev1.Service, taskName, taskUID string, declarations []api.ExposedPort) bool {
	if service.Namespace != tenant.DefaultNamespace || service.Name != raykubernetes.ExposedPortServiceName(taskName) || service.Annotations["rlark.io/management-task-uid"] != taskUID || service.Spec.Selector["rlark.io/management-task-uid"] != taskUID || service.Spec.Type != corev1.ServiceTypeNodePort || len(service.Spec.Ports) != len(declarations) {
		return false
	}
	byName := make(map[string]corev1.ServicePort, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		byName[port.Name] = port
	}
	if len(byName) != len(declarations) {
		return false
	}
	for _, declaration := range declarations {
		port, found := byName[declaration.Name]
		if !found || port.Protocol != corev1.Protocol(strings.ToUpper(declaration.Protocol)) || port.Port != declaration.TargetPort || port.NodePort == 0 || (declaration.NodePort != 0 && port.NodePort != declaration.NodePort) {
			return false
		}
	}
	return true
}
