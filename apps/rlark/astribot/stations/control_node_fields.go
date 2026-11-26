package stations

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControlUpdateConflict reports attempts to change station fields in a Node replacement.
func ControlUpdateConflict(existing metav1.Object, body []byte) string {
	labels := existing.GetLabels()
	if value, _ := robotPowerIn(body); value != labels[RobotPowerLabel] {
		return robotPowerRefused
	}
	annotations := existing.GetAnnotations()
	if value, _ := robotLabelIn(body, RobotClaimLabel); value != labels[RobotClaimLabel] ||
		nodeAnnotations(body)[RobotClaimSetAnnotation] != annotations[RobotClaimSetAnnotation] {
		return robotClaimRefused
	}
	if nodeAnnotations(body)[RobotRestartAnnotation] != annotations[RobotRestartAnnotation] {
		return robotRestartRefused
	}
	if value, _ := robotLabelIn(body, StationMarkLabel); value != labels[StationMarkLabel] ||
		nodeAnnotations(body)[StationMarkAnnotation] != annotations[StationMarkAnnotation] {
		return stationMarkRefused
	}
	if value, _ := robotLabelIn(body, StationLabel); value != labels[StationLabel] {
		return stationLabelRefused
	}
	return ""
}

// ControlPatchConflict reports station fields named by a generic Node patch.
func ControlPatchConflict(body []byte) string {
	if _, named := robotPowerIn(body); named {
		return robotPowerRefused
	}
	_, claimed := robotLabelIn(body, RobotClaimLabel)
	if _, annotated := nodeAnnotations(body)[RobotClaimSetAnnotation]; claimed || annotated {
		return robotClaimRefused
	}
	if _, annotated := nodeAnnotations(body)[RobotRestartAnnotation]; annotated {
		return robotRestartRefused
	}
	_, marked := robotLabelIn(body, StationMarkLabel)
	if _, annotated := nodeAnnotations(body)[StationMarkAnnotation]; marked || annotated {
		return stationMarkRefused
	}
	if _, named := robotLabelIn(body, StationLabel); named {
		return stationLabelRefused
	}
	return ""
}

func nodeAnnotations(body []byte) map[string]string {
	var object struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &object)
	return object.Metadata.Annotations
}
