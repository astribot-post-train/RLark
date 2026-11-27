package stations

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var RobotModelsResource = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "robotmodels"}

// RobotModelRoutes holds the data readers for the administrator model catalog.
type RobotModelRoutes struct {
	Station                                               dynamic.Interface
	NodeClasses, Nodes, Enrollments, Machines, Admissions schema.GroupVersionResource
	MachineNamespace                                      string
	NodeJoinModels                                        func(context.Context) ([]string, error)
	// Capabilities is what the console may show of a model's bodies (the station's body-state handler's).
	Capabilities func(model string) json.RawMessage
}

func (r RobotModelRoutes) Register(admin *gin.RouterGroup) {
	admin.GET("/station/robot-models", r.list)
	admin.DELETE("/station/robot-models/:name", r.remove)
}

type robotModelReferenceCounts struct {
	NodeClasses    int `json:"nodeClasses"`
	Nodes          int `json:"nodes"`
	Machines       int `json:"machines"`
	NodeAdmissions int `json:"nodeAdmissions"`
	NodeJoins      int `json:"nodeJoins"`
}

type robotModelView struct {
	Name         string                    `json:"name"`
	DisplayName  string                    `json:"displayName,omitempty"`
	Vendor       string                    `json:"vendor,omitempty"`
	Description  string                    `json:"description,omitempty"`
	References   robotModelReferenceCounts `json:"references"`
	Capabilities json.RawMessage           `json:"capabilities,omitempty"`
}

func (r RobotModelRoutes) references(c *gin.Context, name string) (robotModelReferenceCounts, error) {
	ctx := c.Request.Context()
	var counts robotModelReferenceCounts
	classes, err := r.Station.Resource(r.NodeClasses).List(ctx, metav1.ListOptions{})
	if err != nil {
		return counts, err
	}
	for i := range classes.Items {
		labels, _, _ := unstructured.NestedStringMap(classes.Items[i].Object, "spec", "labels")
		value, _, _ := unstructured.NestedString(classes.Items[i].Object, "spec", "model")
		if value == name || labels["rlark.io/robot-model"] == name {
			counts.NodeClasses++
		}
	}
	nodeReferences := map[string]struct{}{}
	nodes, err := r.Station.Resource(r.Nodes).List(ctx, metav1.ListOptions{})
	if err != nil {
		return counts, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		declared, _, _ := unstructured.NestedString(node.Object, "spec", "machine", "model")
		observed, _, _ := unstructured.NestedString(node.Object, "status", "observation", "identity", "model")
		if declared == name || observed == name || node.GetLabels()["station.rlark.io/machine-model"] == name || node.GetLabels()["rlark.io/robot-model"] == name {
			nodeReferences[node.GetNamespace()+"/"+node.GetName()] = struct{}{}
		}
	}
	enrollments, err := r.Station.Resource(r.Enrollments).List(ctx, metav1.ListOptions{})
	if err != nil {
		return counts, err
	}
	for i := range enrollments.Items {
		enrollment := &enrollments.Items[i]
		declared, _, _ := unstructured.NestedString(enrollment.Object, "spec", "machine", "model")
		observed, _, _ := unstructured.NestedString(enrollment.Object, "status", "observation", "identity", "model")
		if declared == name || observed == name || enrollment.GetLabels()["rlark.io/robot-model"] == name {
			nodeReferences[enrollment.GetNamespace()+"/"+enrollment.GetName()] = struct{}{}
		}
	}
	counts.Nodes = len(nodeReferences)
	machines, err := r.Station.Resource(r.Machines).Namespace(r.MachineNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return counts, err
	}
	for i := range machines.Items {
		machine := &machines.Items[i]
		admit, _, _ := unstructured.NestedString(machine.Object, "spec", "admit", "model")
		observed, _, _ := unstructured.NestedString(machine.Object, "status", "observedIdentity", "model")
		if admit == name || observed == name {
			counts.Machines++
		}
	}
	admissions, err := r.Station.Resource(r.Admissions).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return counts, err
	}
	for i := range admissions.Items {
		admission := &admissions.Items[i]
		model, _, _ := unstructured.NestedString(admission.Object, "spec", "machine", "model")
		if model == name {
			counts.NodeAdmissions++
		}
	}
	if r.NodeJoinModels != nil {
		models, err := r.NodeJoinModels(ctx)
		if err != nil {
			return counts, err
		}
		for _, installed := range models {
			if installed == name {
				counts.NodeJoins++
			}
		}
	}
	return counts, nil
}

func (r RobotModelRoutes) list(c *gin.Context) {
	list, err := r.Station.Resource(RobotModelsResource).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "RobotModel request failed"})
		return
	}
	items := make([]robotModelView, 0, len(list.Items))
	for i := range list.Items {
		object := &list.Items[i]
		counts, err := r.references(c, object.GetName())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "RobotModel reference request failed"})
			return
		}
		displayName, _, _ := unstructured.NestedString(object.Object, "spec", "displayName")
		vendor, _, _ := unstructured.NestedString(object.Object, "spec", "vendor")
		description, _, _ := unstructured.NestedString(object.Object, "spec", "description")
		items = append(items, robotModelView{Name: object.GetName(), DisplayName: displayName, Vendor: vendor, Description: description, References: counts, Capabilities: r.Capabilities(object.GetName())})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (r RobotModelRoutes) remove(c *gin.Context) {
	name := c.Param("name")
	resource := r.Station.Resource(RobotModelsResource)
	model, err := resource.Get(c.Request.Context(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "RobotModel not found"})
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"error": "RobotModel request failed"})
		}
		return
	}
	counts, err := r.references(c, name)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "RobotModel reference request failed"})
		return
	}
	if counts.NodeClasses+counts.Nodes+counts.Machines+counts.NodeAdmissions+counts.NodeJoins > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "RobotModel is referenced and cannot be deleted", "references": counts})
		return
	}
	if err := resource.Delete(c.Request.Context(), model.GetName(), metav1.DeleteOptions{}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "RobotModel could not be deleted"})
		return
	}
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}
