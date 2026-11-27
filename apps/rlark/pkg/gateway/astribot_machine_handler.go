package gateway

import (
	"context"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var machineGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "machines"}

// machineNamespace is where the Machines are: with the SiteNetwork.
const machineNamespace = "station-system"

// machineOperationLabel is on the NodeOperation that joins a Machine and names
// it, machineRequestAnnotation the Machine's generation it was started for
// (station-controller, MachineLabel and machineRequestAnnotation).
const (
	machineOperationLabel    = "station.rlark.io/machine"
	machineRequestAnnotation = "station.rlark.io/machine-generation"
)

// machineJoins are the NodeOperations that join Machines, by the Machine and
// the generation of the request each was started for.
func (g *Gateway) machineJoins(ctx context.Context) (map[string]*stations.MachineJoin, error) {
	joins, err := g.stationClient.Resource(nodeOperationGVR).Namespace("").List(ctx, metav1.ListOptions{LabelSelector: machineOperationLabel})
	if err != nil {
		return nil, err
	}
	joinOf := map[string]*stations.MachineJoin{}
	for i := range joins.Items {
		phase, _, _ := unstructured.NestedString(joins.Items[i].Object, "status", "phase")
		stage, _, _ := unstructured.NestedString(joins.Items[i].Object, "status", "stage")
		machineName := joins.Items[i].GetLabels()[machineOperationLabel]
		joinOf[machineName] = &stations.MachineJoin{Phase: phase, Stage: stage}
	}
	if g.kubeClient == nil {
		return joinOf, nil
	}
	nodeJoins, err := g.kubeClient.RlinfV1alpha1().NodeJoins().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range nodeJoins.Items {
		join := &nodeJoins.Items[i]
		machineName := join.Labels["station.rlark.io/machine"]
		if machineName == "" {
			continue
		}
		joinOf[machineName] = stations.MachineJoinFromNodeJoin(join)
	}
	return joinOf, nil
}

// handleListMachines answers GET /api/v1/station/machines?site=&phase=: the
// machines RLark knows of, newest sighting first, and their classes.
func (g *Gateway) handleListMachines(c *gin.Context) {
	ctx := c.Request.Context()
	list, err := g.stationClient.Resource(machineGVR).Namespace(machineNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Machine request failed"})
		return
	}
	classList, err := g.stationClient.Resource(nodeClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeClass request failed"})
		return
	}
	classes := map[string]stations.MachineClass{}
	for i := range classList.Items {
		classObject := &classList.Items[i]
		labels, _, _ := unstructured.NestedStringMap(classObject.Object, "spec", "labels")
		taints, _, _ := unstructured.NestedStringSlice(classObject.Object, "spec", "taints")
		kind, _, _ := unstructured.NestedString(classObject.Object, "spec", "kind")
		model, _, _ := unstructured.NestedString(classObject.Object, "spec", "model")
		if kind == "" && (labels["rlark.io/robot-model"] != "" || labels["rlark.io/node-category"] == "robot") {
			kind = "Robot"
		}
		discovery, hasDiscovery, _ := unstructured.NestedMap(classObject.Object, "spec", "discovery")
		fingerprints, _, _ := unstructured.NestedStringSlice(discovery, "hostKeyFingerprints")
		naming, _, _ := unstructured.NestedMap(classObject.Object, "spec", "naming")
		class := stations.MachineClass{Kind: kind, Model: model, Labels: labels, Taints: taints, Naming: naming, HasDiscovery: hasDiscovery, HostKeyFingerprints: fingerprints}
		if access, hasAccess, accessErr := g.nodeClassAccessOf(ctx, classObject); accessErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "NodeClass access status request failed"})
			return
		} else if hasAccess {
			class.Access = &stations.MachineClassAccess{Entry: stations.MachineCredentialStatus{Configured: access.Entry.Configured}, Node: stations.MachineCredentialStatus{Configured: access.Node.Configured}}
			if access.Sudo != nil {
				class.Access.Sudo = &stations.MachineCredentialStatus{Configured: access.Sudo.Configured}
			}
		}
		classes[classObject.GetName()] = class
	}
	joins, err := g.machineJoins(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeOperation request failed"})
		return
	}
	site, phase := c.Query("site"), c.Query("phase")
	items := make([]stations.Machine, 0, len(list.Items))
	for i := range list.Items {
		if view := stations.MachineView(&list.Items[i], joins, classes); (site == "" || view.Site == site) && (phase == "" || view.Phase == phase) {
			items = append(items, view)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].LastSeen > items[j].LastSeen })
	c.JSON(http.StatusOK, gin.H{"items": items, "classes": classes})
}

// handleAdmitMachines answers POST /api/v1/station/machines/admit: the
// administrator's decision that the named machines are to be nodes of a
// cluster. It is written on each Machine (spec.admit) with who and when;
// station-controller names, admits and joins them. Each machine is answered
// for by itself: one that cannot be admitted does not hold the others back.
func (g *Gateway) handleAdmitMachines(c *gin.Context) {
	var request stations.MachineAdmissionRequest
	if !decodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, false) {
		return
	}
	if err := request.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	// The cluster is a registered one; where its machines reach its API, Station API answers them.
	if _, err := g.stationNamespace(c, request.ClusterID); err != nil {
		return
	}
	joins, err := g.machineJoins(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeOperation request failed"})
		return
	}
	answers := stations.AdmitMachines(ctx, request, joins,
		g.stationClient.Resource(machineGVR).Namespace(machineNamespace),
		g.stationClient.Resource(nodeClassGVR),
		g.stationClient.Resource(stations.RobotModelsResource),
		c.GetString(authSubjectKey), time.Now())
	c.JSON(http.StatusOK, gin.H{"machines": answers})
}
