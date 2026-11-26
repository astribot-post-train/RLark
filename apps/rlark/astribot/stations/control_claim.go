package stations

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// RobotClaimLabel is, on a robot's management Node, the Run that holds
	// the robot now. The agents carry it to the cluster's Node, where the
	// robot's model Addon puts it on the host for whatever drives the robot.
	RobotClaimLabel = "rlark.io/robot-claim"
	// RobotClaimSetAnnotation is the whole claim beside it: holder, job, by,
	// grantedAt, until. Only the management plane reads it: a claim ends here,
	// and a robot cut off from the management plane keeps the holder it has.
	RobotClaimSetAnnotation = "rlark.io/robot-claim-set"

	robotClaimRefused = "a robot's claim (label " + RobotClaimLabel + ") is set with PUT and DELETE /api/v1/rlinf.io/v1alpha1/nodes/{name}/robot-claim"
)

type RobotClaim struct {
	Holder    string    `json:"holder"`
	Job       string    `json:"job"`
	By        string    `json:"by"`
	GrantedAt time.Time `json:"grantedAt"`
	Until     time.Time `json:"until"`
}

type claimRobotRequest struct {
	Holder       string `json:"holder"`
	Job          string `json:"job"`
	LeaseSeconds int    `json:"leaseSeconds"`
}

// robotClaimOf reads a Node's claim, and whether it has one (over or not).
func robotClaimOf(node metav1.Object) (claim RobotClaim, held bool) {
	if node.GetLabels()[RobotClaimLabel] != "" {
		_ = json.Unmarshal([]byte(node.GetAnnotations()[RobotClaimSetAnnotation]), &claim)
	}
	return claim, claim.Holder != ""
}

// writeRobotClaim puts the claim (nil: none) on the Node at the version it was
// read at: of two writers of one version the API server refuses the second.
func (r ControlRoutes) writeRobotClaim(ctx context.Context, node metav1.Object, claim *RobotClaim) error {
	var label, record any
	if claim != nil {
		text, _ := json.Marshal(claim)
		label, record = claim.Holder, string(text)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": node.GetResourceVersion(),
		"labels":          map[string]any{RobotClaimLabel: label},
		"annotations":     map[string]any{RobotClaimSetAnnotation: record},
	}})
	_, err := r.PatchNode(ctx, node.GetNamespace(), node.GetName(), patch)
	return err
}

// claim answers PUT /api/v1/rlinf.io/v1alpha1/nodes/{name}/robot-claim:
// a free robot is the applicant's for the lease; the holder applying again
// keeps it for another lease; anyone else is told who holds it. A claim lasts
// until it is released or the sweeper ends it, its lease over: one whose lease
// is over is still the robot's until then. Who applies is the authenticated
// caller, never the body. With ?renew=true it only renews: a free robot is
// answered 409 with details.current null, and nothing is granted.
func (r ControlRoutes) claim(c *gin.Context) {
	request := claimRobotRequest{LeaseSeconds: 60}
	if err := c.ShouldBindJSON(&request); err != nil || request.Holder == "" || len(request.Holder) > 40 || len(request.Job) > 253 ||
		len(validation.IsValidLabelValue(request.Holder)) != 0 || request.LeaseSeconds < 5 || request.LeaseSeconds > 600 {
		r.RespondError(c, http.StatusBadRequest, InvalidBody, "holder must be a label value of 1 to 40 characters, job at most 253 and leaseSeconds 5 to 600")
		return
	}
	renew, err := strconv.ParseBool(c.DefaultQuery("renew", "false"))
	if err != nil {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "renew must be a boolean")
		return
	}
	ctx, name, namespace, actor := c.Request.Context(), c.Param("name"), c.Query("namespace"), r.Actor(c)
	for attempt := 0; ; attempt++ {
		existing, err := r.GetNode(ctx, namespace, name)
		if err != nil {
			r.RespondKubeError(c, err.Error(), err)
			return
		}
		if mark := existing.GetLabels()[StationMarkLabel]; mark != "" {
			r.RespondErrorWithDetails(c, http.StatusConflict, RobotBusy, "the station is marked "+mark, map[string]any{"current": nil, "mark": mark})
			return
		}
		current, held := robotClaimOf(existing)
		now := time.Now().UTC().Truncate(time.Second)
		ours := held && current.Holder == request.Holder && current.By == actor
		claim := RobotClaim{Holder: request.Holder, Job: request.Job, By: actor, GrantedAt: now, Until: now.Add(time.Duration(request.LeaseSeconds) * time.Second)}
		if held && !ours || renew && !held {
			var holder any // null: nobody holds the robot
			if held {
				holder = current
			}
			r.RespondErrorWithDetails(c, http.StatusConflict, RobotBusy, "the robot is held by "+cmp.Or(current.Holder, "nobody"), map[string]any{"current": holder})
			return
		}
		if ours {
			claim.GrantedAt = current.GrantedAt
			if claim.Job == "" {
				claim.Job = current.Job // a renewal that names no Job keeps the claim's
			}
		}
		err = r.writeRobotClaim(ctx, existing, &claim)
		if apierrors.IsConflict(err) && attempt == 0 {
			continue // another applicant wrote first: decided again on what it left
		}
		log.FromContext(ctx).WithName("audit").Info("robot claim", "action", "claim", "actor", actor, "namespace", namespace, "node", name,
			"holder", claim.Holder, "job", claim.Job, "until", claim.Until, "renewed", ours, "set", err == nil)
		if err != nil {
			r.RespondWriteError(c, err)
			return
		}
		c.JSON(http.StatusOK, claim)
		return
	}
}

// release answers DELETE /api/v1/rlinf.io/v1alpha1/nodes/{name}/robot-claim:
// the holder gives the robot back (?holder=), or an administrator or operator
// takes it from whoever holds it (?force=true&reason=). The answer is the claim
// it ended, {} when the robot had none.
func (r ControlRoutes) release(c *gin.Context) {
	ctx, name, namespace, actor := c.Request.Context(), c.Param("name"), c.Query("namespace"), r.Actor(c)
	force, reason := c.Query("force") == "true", c.Query("reason")
	if force && !r.IsOperator(c) {
		r.RespondError(c, http.StatusForbidden, Forbidden, "administrator or operator access required")
		return
	}
	if force && reason == "" {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "a forced release needs a reason")
		return
	}
	for attempt := 0; ; attempt++ {
		existing, err := r.GetNode(ctx, namespace, name)
		if err != nil {
			r.RespondKubeError(c, err.Error(), err)
			return
		}
		current, held := robotClaimOf(existing)
		if held && !force && (current.Holder != c.Query("holder") || current.By != actor) {
			r.RespondErrorWithDetails(c, http.StatusConflict, RobotBusy, "the robot is held by "+current.Holder, map[string]any{"current": current})
			return
		}
		var released any = gin.H{} // nothing was held
		if held {
			released = current
			err = r.writeRobotClaim(ctx, existing, nil)
			if apierrors.IsConflict(err) && attempt == 0 {
				continue
			}
			log.FromContext(ctx).WithName("audit").Info("robot claim", "action", "release", "actor", actor, "namespace", namespace, "node", name,
				"holder", current.Holder, "by", current.By, "job", current.Job, "forced", force, "reason", reason, "set", err == nil)
			if err != nil {
				r.RespondWriteError(c, err)
				return
			}
		}
		c.JSON(http.StatusOK, released)
		return
	}
}

// SweepClaims ends the claims whose lease is over, every 10 s. A claim
// ends on the management plane only: the robot does not count the lease. A
// minute passes after the Gateway starts, and after its upstream comes back (a
// list that failed, then one that did not), before claims are ended again:
// after an outage longer than a lease every claim is over, and their holders,
// which renew more often than that, keep them instead of the whole site being
// let go at once.
func (r ControlRoutes) SweepClaims(ctx context.Context) {
	for quiet, listed := time.Minute, true; ; {
		select {
		case <-ctx.Done():
			return
		case <-time.After(quiet):
		}
		wasListed := listed
		listed = r.ExpireClaims(ctx, wasListed)
		quiet = 10 * time.Second
		if !listed || !wasListed {
			quiet = time.Minute
		}
	}
}

// ExpireClaims ends the claims whose lease is over, when end is true,
// and answers whether the claims were listed.
func (r ControlRoutes) ExpireClaims(ctx context.Context, end bool) bool {
	list, err := r.ListNodes(ctx, metav1.NamespaceAll, metav1.ListOptions{LabelSelector: RobotClaimLabel})
	if err != nil {
		log.FromContext(ctx).Error(err, "robot claims are not listed")
		return false
	}
	if !end {
		return true
	}
	nodes := list.Items
	for i := range nodes {
		claim, _ := robotClaimOf(&nodes[i])
		if time.Now().Before(claim.Until) {
			continue
		}
		err := r.writeRobotClaim(ctx, &nodes[i], nil)
		log.FromContext(ctx).WithName("audit").Info("robot claim", "action", "expire", "namespace", nodes[i].Namespace, "node", nodes[i].Name,
			"holder", claim.Holder, "by", claim.By, "job", claim.Job, "until", claim.Until, "set", err == nil)
	}
	return true
}
