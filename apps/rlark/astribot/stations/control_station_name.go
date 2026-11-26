package stations

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// StationLabel is the robot Station a node stands at (S01, S02, …). The
	// station-controller gives a robot the first free one when it admits the
	// machine; its NodeAdmission holds it as one of the machine's own labels
	// (station.rlark.io/machine-labels) and puts it on the RLark Node.
	StationLabel          = "rlark.io/station"
	AdmissionOwnLabels    = "station.rlark.io/machine-labels"
	stationLabelRefused   = "a robot's Station (label " + StationLabel + ") is set with PUT /api/v1/rlinf.io/v1alpha1/nodes/{name}/station"
	stationNameDescriptor = "station must be S followed by two or three digits, as S01"
)

var stationNamePattern = regexp.MustCompile(`^S[0-9]{2,3}$`)

// setName answers PUT /api/v1/rlinf.io/v1alpha1/nodes/{name}/station:
// the Station a robot stands at, unique in its cluster. The node's
// NodeAdmission takes it as the machine's own label, so that its NodeClass
// keeps it and a machine joined again is at the same Station, and the RLark
// Node carries it at once. A robot without a NodeAdmission has no Station (409).
// Every call is in the audit log with its actor.
func (r ControlRoutes) setName(c *gin.Context) {
	var request struct {
		Station string `json:"station"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || !stationNamePattern.MatchString(request.Station) {
		r.RespondError(c, http.StatusBadRequest, InvalidBody, stationNameDescriptor)
		return
	}
	ctx, name, namespace, actor := c.Request.Context(), c.Param("name"), c.Query("namespace"), r.Actor(c)
	existing, err := r.GetNode(ctx, namespace, name)
	if err != nil {
		r.RespondKubeError(c, err.Error(), err)
		return
	}
	was := existing.GetLabels()[StationLabel]
	if existing.GetLabels()["rlark.io/robot-id"] == "" {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "the node is not a robot")
		return
	}
	others, err := r.ListNodes(ctx, namespace, metav1.ListOptions{LabelSelector: StationLabel + "=" + request.Station})
	if err != nil {
		r.RespondKubeError(c, err.Error(), err)
		return
	}
	admissions := r.Admissions(namespace)
	list, err := admissions.List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		return
	}
	for _, other := range others.Items {
		if other.Name != name {
			r.RespondError(c, http.StatusConflict, Conflict, request.Station+" is the Station of "+other.Name)
			return
		}
	}
	var admission *unstructured.Unstructured
	for i := range list.Items {
		labels, _, _ := unstructured.NestedStringMap(list.Items[i].Object, "spec", "labels")
		if list.Items[i].GetName() == name {
			admission = &list.Items[i]
		} else if labels[StationLabel] == request.Station {
			r.RespondError(c, http.StatusConflict, Conflict, request.Station+" is the Station of "+list.Items[i].GetName())
			return
		}
	}
	if admission == nil {
		// The admissions are where the station-controller sees which Stations are taken.
		r.RespondError(c, http.StatusConflict, Conflict, name+" has no NodeAdmission to keep its Station")
		return
	}
	labels, _, _ := unstructured.NestedStringMap(admission.Object, "spec", "labels")
	if labels == nil {
		labels = map[string]string{}
	}
	labels[StationLabel] = request.Station
	_ = unstructured.SetNestedStringMap(admission.Object, labels, "spec", "labels")
	annotations := admission.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	own := strings.FieldsFunc(annotations[AdmissionOwnLabels], func(r rune) bool { return r == ',' })
	if !slices.Contains(own, StationLabel) {
		own = append(own, StationLabel)
		slices.Sort(own)
		annotations[AdmissionOwnLabels] = strings.Join(own, ",")
		admission.SetAnnotations(annotations)
	}
	if _, err := admissions.Update(ctx, admission, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			r.RespondError(c, http.StatusConflict, Conflict, "the admission changed meanwhile; try again")
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		}
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{StationLabel: request.Station}}})
	result, err := r.PatchNode(ctx, namespace, name, patch)
	log.FromContext(ctx).WithName("audit").Info("robot station", "actor", actor, "namespace", namespace, "node", name,
		"was", was, "now", request.Station, "set", err == nil)
	if err != nil {
		r.RespondWriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
