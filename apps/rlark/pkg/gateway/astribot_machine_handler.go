package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

var machineGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "machines"}

type machineRejoinRequest struct {
	ClusterID  string `json:"cluster_id"`
	RequestID  string `json:"request_id"`
	RobotModel string `json:"robot_model,omitempty"`
}

func (g *Gateway) handleRejoinMachine(c *gin.Context) {
	var request machineRejoinRequest
	if !decodeOneJSON(c, maxNodeOperationRequestBytes, &request, false) {
		return
	}
	if len(validation.IsDNS1123Subdomain(request.ClusterID)) != 0 || len(validation.IsDNS1123Subdomain(request.RequestID)) != 0 || len(request.RequestID) > 120 || (request.RobotModel != "" && len(validation.IsDNS1123Label(request.RobotModel)) != 0) {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_REJOIN_REQUEST", "error": "cluster_id, request_id, and robot_model must be valid registered identifiers"})
		return
	}
	ctx := c.Request.Context()
	namespace, err := g.stationNamespace(c, request.ClusterID)
	if err != nil {
		return
	}
	machine, err := g.stationClient.Resource(machineGVR).Namespace(machineNamespace).Get(ctx, c.Param("name"), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"code": "MACHINE_NOT_FOUND", "error": "machine not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": "MACHINE_LOOKUP_FAILED", "error": "machine lookup failed"})
		return
	}
	clusterID, _, _ := unstructured.NestedString(machine.Object, "status", "clusterID")
	nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeName")
	serial, _, _ := unstructured.NestedString(machine.Object, "status", "serial")
	phase, _, _ := unstructured.NestedString(machine.Object, "status", "phase")
	rejoinID, _, _ := unstructured.NestedString(machine.Object, "spec", "rejoinRequestID")
	startedRejoinID, _, _ := unstructured.NestedString(machine.Object, "status", "rejoinRequestID")
	className, _, _ := unstructured.NestedString(machine.Object, "spec", "admit", "nodeClass")
	if className == "" {
		className, _, _ = unstructured.NestedString(machine.Object, "spec", "nodeClass")
	}
	if clusterID != request.ClusterID || nodeName == "" || serial == "" {
		c.JSON(http.StatusConflict, gin.H{"code": "MACHINE_NOT_JOINED", "error": "machine is not joined to the requested cluster"})
		return
	}
	if rejoinID == request.RequestID {
		data, operationErr := g.rejoinOperationOrPending(ctx, machine, request.RequestID, nodeName, namespace, className)
		if operationErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"code": "NODE_OPERATION_LOOKUP_FAILED", "error": "node operation lookup failed"})
			return
		}
		setRejoinOperationLocation(c, request.ClusterID, nodeName, request.RequestID)
		c.JSON(http.StatusAccepted, gin.H{"data": data})
		return
	}
	if rejoinID != "" {
		if startedRejoinID != rejoinID {
			c.JSON(http.StatusConflict, gin.H{"code": "REJOIN_IN_PROGRESS", "error": "another rejoin request is active"})
			return
		}
		operationName, _, _ := unstructured.NestedString(machine.Object, "status", "operation")
		previous, previousErr := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace).Get(ctx, operationName, metav1.GetOptions{})
		previousPhase := ""
		if previousErr == nil {
			previousPhase, _, _ = unstructured.NestedString(previous.Object, "status", "phase")
		}
		if previousErr != nil || (previousPhase != "Succeeded" && previousPhase != "Failed" && previousPhase != "Blocked") {
			c.JSON(http.StatusConflict, gin.H{"code": "REJOIN_IN_PROGRESS", "error": "another rejoin request is active"})
			return
		}
		if phase != "Joined" && !(phase == "Admitted" && (previousPhase == "Failed" || previousPhase == "Blocked")) {
			c.JSON(http.StatusConflict, gin.H{"code": "MACHINE_NOT_JOINED", "error": "machine is not in a retryable joined state"})
			return
		}
	} else if phase != "Joined" {
		c.JSON(http.StatusConflict, gin.H{"code": "MACHINE_NOT_JOINED", "error": "machine is not joined to the requested cluster"})
		return
	}
	admission, err := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace).Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusConflict, gin.H{"code": "NODE_ADMISSION_NOT_FOUND", "error": "the existing NodeAdmission is missing"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": "NODE_ADMISSION_LOOKUP_FAILED", "error": "NodeAdmission lookup failed"})
		return
	}
	admissionCluster, _, _ := unstructured.NestedString(admission.Object, "spec", "clusterID")
	admissionSerial, _, _ := unstructured.NestedString(admission.Object, "spec", "serial")
	category, _, _ := unstructured.NestedString(admission.Object, "spec", "machine", "category")
	admissionModel, _, _ := unstructured.NestedString(admission.Object, "spec", "machine", "model")
	admissionPhase, _, _ := unstructured.NestedString(admission.Object, "status", "phase")
	if admissionCluster != request.ClusterID || admissionSerial != serial || category != "Robot" || admissionPhase != "Joined" {
		c.JSON(http.StatusConflict, gin.H{"code": "NODE_IDENTITY_MISMATCH", "error": "NodeAdmission identity does not match the Robot Machine"})
		return
	}
	class, err := g.stationClient.Resource(nodeClassGVR).Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"code": "ROBOT_NODE_CLASS_REQUIRED", "error": "the existing Robot NodeClass is unavailable"})
		return
	}
	kind, _, _ := unstructured.NestedString(class.Object, "spec", "kind")
	classModel, _, _ := unstructured.NestedString(class.Object, "spec", "model")
	if classModel == "" {
		classModel, _, _ = unstructured.NestedString(class.Object, "spec", "labels", "rlark.io/robot-model")
	}
	if kind != "Robot" && classModel == "" {
		c.JSON(http.StatusConflict, gin.H{"code": "ROBOT_NODE_CLASS_REQUIRED", "error": "the existing NodeClass is not a Robot class"})
		return
	}
	originalModel := admissionModel
	if originalModel == "" {
		originalModel, _, _ = unstructured.NestedString(machine.Object, "spec", "admit", "model")
	}
	requestedModel := request.RobotModel
	model := originalModel
	if originalModel == "" {
		model = requestedModel
		if model == "" {
			model = classModel
		}
	} else if classModel != "" && originalModel != classModel {
		if requestedModel != classModel {
			c.JSON(http.StatusConflict, gin.H{"code": "ROBOT_MODEL_MISMATCH", "error": "robot_model must resolve the existing model conflict by matching the NodeClass"})
			return
		}
		model = classModel
	} else if requestedModel != "" && requestedModel != originalModel {
		c.JSON(http.StatusConflict, gin.H{"code": "ROBOT_MODEL_MISMATCH", "error": "robot_model does not match the existing node identity"})
		return
	}
	if model == "" || classModel != model {
		c.JSON(http.StatusBadRequest, gin.H{"code": "ROBOT_MODEL_REQUIRED", "error": "a registered RobotModel matching the NodeClass is required"})
		return
	}
	if _, err := g.stationClient.Resource(robotModelGVR).Get(ctx, model, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusBadRequest, gin.H{"code": "UNKNOWN_ROBOT_MODEL", "error": "robot_model is not registered"})
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"code": "ROBOT_MODEL_LOOKUP_FAILED", "error": "RobotModel lookup failed"})
		}
		return
	}
	observedModel, _, _ := unstructured.NestedString(machine.Object, "status", "observedIdentity", "model")
	observedCategory, _, _ := unstructured.NestedString(machine.Object, "status", "observedIdentity", "category")
	identityVerified, _, _ := unstructured.NestedString(machine.Object, "status", "identityVerified")
	if (observedModel != "" && observedModel != model) || (observedCategory != "" && observedCategory != "Robot") || identityVerified == "False" {
		c.JSON(http.StatusConflict, gin.H{"code": "ROBOT_IDENTITY_MISMATCH", "error": "observed Robot identity does not match the registered model"})
		return
	}
	access, found, _ := unstructured.NestedMap(class.Object, "spec", "access")
	if !found || access["entry"] == nil || access["node"] == nil {
		c.JSON(http.StatusConflict, gin.H{"code": "NODE_CLASS_CREDENTIALS_UNAVAILABLE", "error": "NodeClass access configuration is incomplete"})
		return
	}
	operations, err := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": "NODE_OPERATION_LOOKUP_FAILED", "error": "node operation lookup failed"})
		return
	}
	for i := range operations.Items {
		op := &operations.Items[i]
		action, _, _ := unstructured.NestedString(op.Object, "spec", "action")
		target, _, _ := unstructured.NestedString(op.Object, "spec", "nodeName")
		phase, _, _ := unstructured.NestedString(op.Object, "status", "phase")
		if action == "Join" && target == nodeName && phase != "Succeeded" && phase != "Failed" && phase != "Blocked" {
			c.JSON(http.StatusConflict, gin.H{"code": "JOIN_IN_PROGRESS", "error": "the node already has an active Join operation"})
			return
		}
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": machine.GetResourceVersion()}, "spec": map[string]any{"rejoinRequestID": request.RequestID, "rejoinRequestedBy": c.GetString(authSubjectKey)}})
	if _, err := g.stationClient.Resource(machineGVR).Namespace(machineNamespace).Patch(ctx, machine.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		latest, readErr := g.stationClient.Resource(machineGVR).Namespace(machineNamespace).Get(ctx, machine.GetName(), metav1.GetOptions{})
		if readErr == nil {
			latestID, _, _ := unstructured.NestedString(latest.Object, "spec", "rejoinRequestID")
			if latestID == request.RequestID {
				data, operationErr := g.rejoinOperationOrPending(ctx, latest, request.RequestID, nodeName, namespace, className)
				if operationErr != nil {
					c.JSON(http.StatusBadGateway, gin.H{"code": "NODE_OPERATION_LOOKUP_FAILED", "error": "node operation lookup failed"})
					return
				}
				setRejoinOperationLocation(c, request.ClusterID, nodeName, request.RequestID)
				c.JSON(http.StatusAccepted, gin.H{"data": data})
				return
			}
		}
		c.JSON(http.StatusConflict, gin.H{"code": "REJOIN_REQUEST_CONFLICT", "error": "machine changed while the rejoin was being requested"})
		return
	}
	_ = unstructured.SetNestedField(machine.Object, request.RequestID, "spec", "rejoinRequestID")
	_ = unstructured.SetNestedField(machine.Object, c.GetString(authSubjectKey), "spec", "rejoinRequestedBy")
	setRejoinOperationLocation(c, request.ClusterID, nodeName, request.RequestID)
	data, operationErr := g.rejoinOperationOrPending(ctx, machine, request.RequestID, nodeName, namespace, className)
	if operationErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": "NODE_OPERATION_LOOKUP_FAILED", "error": "node operation lookup failed"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": data})
}

func setRejoinOperationLocation(c *gin.Context, clusterID, nodeName, requestID string) {
	c.Header("Location", "/api/v1/station/node-operations/"+rejoinOperationName(nodeName, requestID)+"?cluster_id="+clusterID)
}

func rejoinOperationName(nodeName, requestID string) string {
	digest := sha256.Sum256([]byte(nodeName + "\x00" + requestID))
	return "join-rejoin-" + hex.EncodeToString(digest[:8])
}

func (g *Gateway) rejoinOperationOrPending(ctx context.Context, machine *unstructured.Unstructured, requestID, nodeName, namespace, className string) (map[string]any, error) {
	operation, err := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace).Get(ctx, rejoinOperationName(nodeName, requestID), metav1.GetOptions{})
	if err == nil {
		return safeNodeOperation(operation), nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	return rejoinOperationView(machine, requestID, nodeName, namespace, className), nil
}

func rejoinOperationView(machine *unstructured.Unstructured, requestID, nodeName, namespace, className string) map[string]any {
	name := rejoinOperationName(nodeName, requestID)
	clusterID, _, _ := unstructured.NestedString(machine.Object, "status", "clusterID")
	requestedBy, _, _ := unstructured.NestedString(machine.Object, "spec", "rejoinRequestedBy")
	return map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation", "metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":   map[string]any{"requestID": "rejoin-" + requestID, "action": "Join", "nodeName": nodeName, "clusterID": clusterID, "admission": nodeName, "nodeClass": className, "requestedBy": requestedBy},
		"status": map[string]any{"phase": "Pending"}}
}

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
