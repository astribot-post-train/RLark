package stations

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

type MachineAdmissionRequest struct {
	ClusterID    string   `json:"cluster_id"`
	Machines     []string `json:"machines"`
	NodeClass    string   `json:"node_class,omitempty"`
	MachineType  string   `json:"machine_type,omitempty"`
	MachineModel string   `json:"machine_model,omitempty"`
	Model        string   `json:"model,omitempty"`
	// Labels replace, for these machines, the values of labels their class declares.
	Labels map[string]string `json:"labels,omitempty"`
}

type MachineAdmissionResult struct {
	Name     string `json:"name"`
	Admitted bool   `json:"admitted"`
	Error    string `json:"error,omitempty"`
}

// Validate checks the batch request before it resolves the selected cluster.
func (request *MachineAdmissionRequest) Validate() error {
	if request.ClusterID == "" || len(request.Machines) == 0 || len(request.Machines) > 500 {
		return errors.New("cluster_id and 1 to 500 machines are required")
	}
	if request.MachineType == "Server" || request.MachineType == "server" {
		return errors.New("machines of type Server must use the SSH NodeJoin path")
	}
	if request.MachineType != "" && request.MachineType != "Robot" && request.MachineType != "robot" {
		return errors.New("machine_type must be Robot; Server uses NodeJoin")
	}
	if request.MachineModel == "" {
		request.MachineModel = request.Model
	}
	for key, value := range request.Labels {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return errors.New("labels must be valid Kubernetes labels")
		}
	}
	return nil
}

// AdmitMachines records the administrator's admission decision independently for
// each selected Machine. The HTTP adapter authorizes the destination cluster.
func AdmitMachines(ctx context.Context, request MachineAdmissionRequest, joins map[string]*MachineJoin,
	resource, classes, models dynamic.ResourceInterface, actor string, requestedAt time.Time) []MachineAdmissionResult {
	classCache := map[string]*unstructured.Unstructured{}
	now := requestedAt.UTC().Format(time.RFC3339)
	answers := make([]MachineAdmissionResult, 0, len(request.Machines))
	for _, name := range request.Machines {
		answer := MachineAdmissionResult{Name: name}
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		var view Machine
		if err == nil {
			view = MachineView(object, joins, nil)
		}
		switch {
		case apierrors.IsNotFound(err):
			answer.Error = "no such machine"
		case err != nil:
			answer.Error = "Machine request failed"
		case view.Joining:
			answer.Error = "the machine is being joined to cluster " + view.AdmitClusterID + " already, as " + view.RequestedBy + " asked at " + view.RequestedAt
		case !view.Admissible:
			answer.Error = "the machine cannot be admitted as it is"
		}
		if answer.Error == "" {
			declaredClass, _, _ := unstructured.NestedString(object.Object, "spec", "nodeClass")
			className := request.NodeClass
			if className == "" {
				className = declaredClass
			}
			if className == "" {
				answer.Error = "a Robot admission requires node_class; Server machines use NodeJoin"
			}
			class := classCache[className]
			if answer.Error == "" && class == nil {
				class, err = classes.Get(ctx, className, metav1.GetOptions{})
				if err != nil {
					if apierrors.IsNotFound(err) {
						answer.Error = "no such NodeClass " + className
					} else {
						answer.Error = "NodeClass request failed"
					}
				} else {
					classCache[className] = class
				}
			}
			classLabels := map[string]string{}
			classModel, classKind := "", ""
			if class != nil {
				classLabels, _, _ = unstructured.NestedStringMap(class.Object, "spec", "labels")
				classModel, _, _ = unstructured.NestedString(class.Object, "spec", "model")
				classKind, _, _ = unstructured.NestedString(class.Object, "spec", "kind")
				if classModel == "" {
					classModel = classLabels["rlark.io/robot-model"]
				}
				if classKind == "" && (classModel != "" || classLabels["rlark.io/node-category"] == "robot") {
					classKind = "Robot"
				}
			}
			if answer.Error == "" && classKind != "" && classKind != "Robot" {
				answer.Error = "NodeClass " + className + " is not a Robot profile"
			}
			if answer.Error == "" && declaredClass != "" && declaredClass != className {
				answer.Error = "the selected NodeClass does not match this candidate"
			}
			fingerprints := []string{}
			if class != nil {
				fingerprints, _, _ = unstructured.NestedStringSlice(class.Object, "spec", "discovery", "hostKeyFingerprints")
			}
			candidateKey, _, _ := unstructured.NestedString(object.Object, "status", "entry", "hostKeyFingerprint")
			if answer.Error == "" && (candidateKey == "" || !slices.Contains(fingerprints, candidateKey)) {
				answer.Error = "the candidate host key is not declared by the selected Robot class"
			}
			model := request.MachineModel
			if model == "" {
				model = request.Model
			}
			if model == "" {
				model = classModel
			}
			if answer.Error == "" && classModel != "" && model != classModel {
				answer.Error = "the selected model does not match NodeClass " + className
			}
			if answer.Error == "" && model == "" {
				answer.Error = "machine_model is required for a Robot admission"
			}
			if answer.Error == "" && classModel == "" {
				if _, modelErr := models.Get(ctx, model, metav1.GetOptions{}); modelErr != nil {
					if apierrors.IsNotFound(modelErr) {
						answer.Error = "unknown RobotModel " + model
					} else {
						answer.Error = "RobotModel request failed"
					}
				}
			}
			admit := map[string]interface{}{"clusterID": request.ClusterID, "type": "Robot", "nodeClass": className, "model": model, "requestedBy": actor, "requestedAt": now}
			if len(request.Labels) > 0 {
				labels := map[string]interface{}{}
				for key, value := range request.Labels {
					if _, declared := classLabels[key]; !declared {
						answer.Error = "label " + key + " is not one NodeClass " + className + " declares"
					}
					labels[key] = value
				}
				admit["labels"] = labels
			}
			if answer.Error == "" {
				if err := unstructured.SetNestedMap(object.Object, admit, "spec", "admit"); err != nil {
					answer.Error = "Machine request failed"
				} else if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
					answer.Error = "the machine changed meanwhile; try again"
				} else if err != nil {
					log.FromContext(ctx).Error(err, "admit Machine", "machine", name)
					answer.Error = "Machine request failed"
				}
			}
		}
		answer.Admitted = answer.Error == ""
		log.FromContext(ctx).WithName("audit").Info("machine", "action", "admit", "actor", actor, "cluster", request.ClusterID, "machine", name, "admitted", answer.Admitted)
		answers = append(answers, answer)
	}
	return answers
}
