package stations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
)

// RobotPowerStateLabel is what a robot's power service says the robot is; a Node with it runs the service.
const RobotPowerStateLabel = "rlark.io/power-state"

type robotPoseRequest struct {
	Pose   string `json:"pose"`
	Action string `json:"action"`
}

// Pose answers POST /api/v1/rlinf.io/v1alpha1/nodes/{name}/robot-pose: the vendor page's hold-to-move
// poses (初始姿势, home), a momentary command and no desired state. The console sends hold while the button is held
// (every 200 ms) and stop when it is let go; the robot's power service moves the robot from the first hold and stops
// it on stop or half a second after the last hold, and refuses (409) a robot not powered, without its driver, held by
// a Run, in use or in maintenance ({reason, state}). The request reaches the service on the robot's Unix socket through the server and
// that node's own Agent. A movement's start and end and every refusal are in the audit log with the actor.
func (r ControlRoutes) Pose(c *gin.Context) {
	var request robotPoseRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.Pose != "home" || request.Action != "hold" && request.Action != "stop" {
		r.RespondError(c, http.StatusBadRequest, InvalidBody, "pose must be home and action hold or stop")
		return
	}
	ctx, name, namespace := c.Request.Context(), c.Param("name"), c.Query("namespace")
	if !r.AuthorizeNode(c, namespace, name) {
		return
	}
	existing, err := r.GetNode(ctx, namespace, name)
	if err != nil {
		r.RespondKubeError(c, err.Error(), err)
		return
	}
	agentID, inAgent := strings.CutPrefix(namespace, apis.RLarkAgentNamespacePrefix)
	if _, robot := existing.GetLabels()[RobotPowerStateLabel]; !robot || !inAgent {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "node "+name+" is not a robot with a power service ("+RobotPowerStateLabel+")")
		return
	}
	body, _ := json.Marshal(request)
	actor := r.Actor(c)
	response, err := r.ForwardPose(ctx, agentID, name, actor, body)
	if err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, "the robot's power service: "+err.Error())
		return
	}
	answer, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	var reply struct {
		Started bool   `json:"started"`
		Reason  string `json:"reason"`
	}
	answered := err == nil && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusConflict) && json.Unmarshal(answer, &reply) == nil
	if request.Action == "stop" || !answered || response.StatusCode != http.StatusOK || reply.Started {
		log.FromContext(ctx).WithName("audit").Info("robot pose", "action", request.Action, "actor", actor, "namespace", namespace, "node", name,
			"pose", request.Pose, "status", response.StatusCode, "started", reply.Started, "refused", reply.Reason)
	}
	if !answered {
		// The vendor's web server failed (503), or the node, its Agent or the service is not reached.
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("the robot's power service: HTTP %d %s", response.StatusCode, bytes.TrimSpace(answer)))
		return
	}
	c.Data(response.StatusCode, "application/json", answer)
}
