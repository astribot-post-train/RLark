package stations

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	StationMarkLabel      = "rlark.io/station-mark"
	StationMarkAnnotation = "rlark.io/station-mark-set"
	stationMarkRefused    = "a station mark is managed through PUT /api/v1/rlinf.io/v1alpha1/nodes/{name}/station-mark"
)

type StationMark struct {
	Mark   string    `json:"mark"`
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

func (r ControlRoutes) setMark(c *gin.Context) {
	var request struct {
		Mark   *string `json:"mark"`
		Reason string  `json:"reason"`
		Holder string  `json:"holder"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Mark == nil {
		r.RespondError(c, http.StatusBadRequest, InvalidBody, "mark must be repair, disabled or an empty string")
		return
	}
	mark, reason := *request.Mark, strings.TrimSpace(request.Reason)
	if (mark != "" && mark != "repair" && mark != "disabled") || (mark != "" && reason == "") || len(reason) > 1000 {
		r.RespondError(c, http.StatusBadRequest, InvalidBody, "repair and disabled require a reason of up to 1000 bytes")
		return
	}
	ctx, name, namespace := c.Request.Context(), c.Param("name"), c.Query("namespace")
	if !r.AuthorizeNode(c, namespace, name) {
		return
	}
	actor := r.Actor(c)
	for attempt := 0; ; attempt++ {
		existing, err := r.GetNode(ctx, namespace, name)
		if err != nil {
			r.RespondKubeError(c, err.Error(), err)
			return
		}
		node := existing
		labels := node.GetLabels()
		if labels["rlark.io/robot-id"] == "" {
			r.RespondError(c, http.StatusBadRequest, InvalidRequest, "the node is not a robot")
			return
		}
		if claim, held := robotClaimOf(node); mark != "" && held && request.Holder != "" && claim.Holder != request.Holder {
			r.RespondErrorWithDetails(c, http.StatusConflict, RobotBusy, "the robot is held by "+claim.Holder, map[string]any{"current": claim})
			return
		}
		var previous StationMark
		_ = json.Unmarshal([]byte(node.GetAnnotations()[StationMarkAnnotation]), &previous)
		if labels[StationMarkLabel] == mark && (mark == "" || previous.Reason == reason) {
			c.JSON(http.StatusOK, existing)
			return
		}
		writeLabels := map[string]any{StationMarkLabel: nil}
		annotations := map[string]any{StationMarkAnnotation: nil}
		if mark != "" {
			record, _ := json.Marshal(StationMark{Mark: mark, Reason: reason, By: actor, At: time.Now().UTC()})
			writeLabels[StationMarkLabel], annotations[StationMarkAnnotation] = mark, string(record)
			// Mark and release are one update, so a racing acquisition cannot leave a marked robot claimed.
			writeLabels[RobotClaimLabel], annotations[RobotClaimSetAnnotation] = nil, nil
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
			"resourceVersion": node.GetResourceVersion(), "labels": writeLabels, "annotations": annotations,
		}})
		result, err := r.PatchNode(ctx, namespace, name, patch)
		if apierrors.IsConflict(err) && attempt == 0 {
			continue
		}
		log.FromContext(ctx).WithName("audit").Info("station mark", "actor", actor,
			"namespace", namespace, "node", name, "mark", mark, "reason", reason, "set", err == nil)
		if err != nil {
			r.RespondWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}
}
