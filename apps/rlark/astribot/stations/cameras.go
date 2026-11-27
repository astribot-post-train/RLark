package stations

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"

	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
)

// A robot's cameras are served by its model Addon (s1u-robot's video container, astribot/s1u/device/cameras.py) on
// the node's address, read-only; the Gateway reaches them through the member cluster's API server, as a proxy of that
// Pod, and decides who may look.
const (
	RobotAddonNamespace = "rlark-system"
	RobotAddonSelector  = "app.kubernetes.io/name=s1u-robot"
	RobotCameraPort     = "18891"
)

// CameraRoutes binds the read-only camera and station-log handlers to the Gateway.
type CameraRoutes struct {
	AuthorizeNode func(*gin.Context, string, string) bool
	ReadMember    func(context.Context, string, string) (*http.Response, error)
	RespondError  func(*gin.Context, int, ErrorKind, string)
	// AdmitStream holds a place for one live stream, or answers the request itself and says no.
	AdmitStream func(*gin.Context) (release func(), ok bool)
}

// Register mounts camera routes and applies the operator guard to station logs.
func (r CameraRoutes) Register(nodes gin.IRouter, requireOperator gin.HandlerFunc) {
	nodes.GET("/:name/station-logs", requireOperator, r.stationLogs)
	nodes.GET("/:name/cameras", r.cameras)
	nodes.GET("/:name/cameras/:camera", r.cameras)
	nodes.GET("/:name/cameras/:camera/stream", r.cameras)
}

// robotCameraViews are the views a robot may have, in the order of its camera wall (head, torso / wrist_left,
// wrist_right); a body has some of them.
var robotCameraViews = []string{"head", "torso", "wrist_left", "wrist_right"}

// robotCameraPath is the camera service's path of a request (with the one query it takes, fps), and whether it is
// one of the robot's: /cameras, /cameras/<view>.jpg, /cameras/<view>/stream, /cameras/wall.jpg, /cameras/wall/stream.
func robotCameraPath(c *gin.Context) (string, bool) {
	camera := c.Param("camera")
	if camera == "" {
		return "/cameras", true
	}
	if !strings.HasSuffix(c.FullPath(), "/stream") {
		view, image := strings.CutSuffix(camera, ".jpg")
		return "/cameras/" + camera, image && (view == "wall" || slices.Contains(robotCameraViews, view))
	}
	path := "/cameras/" + camera + "/stream"
	if fps := c.Query("fps"); fps != "" {
		path += "?" + url.Values{"fps": {fps}}.Encode()
	}
	return path, camera == "wall" || slices.Contains(robotCameraViews, camera)
}

// cameras answers GET /api/v1/rlinf.io/v1alpha1/nodes/{name}/cameras[/…]: what the robot's camera
// service answers (its state, a view's newest JPEG or its stream, the wall), passed on as it comes, a stream part by
// part. Anyone signed in may look, as anyone may claim a robot.
func (r CameraRoutes) cameras(c *gin.Context) {
	path, ok := robotCameraPath(c)
	if !ok {
		r.RespondError(c, http.StatusNotFound, NotFound, "a robot's cameras are /cameras, /cameras/<view>.jpg, /cameras/<view>/stream, /cameras/wall.jpg and /cameras/wall/stream; the views are "+strings.Join(robotCameraViews, ", "))
		return
	}
	if r.AdmitStream != nil && strings.HasSuffix(c.FullPath(), "/stream") {
		release, admitted := r.AdmitStream(c)
		if !admitted {
			return
		}
		defer release()
	}
	agent, pod, ok := r.runningRobotAddon(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	answer, err := r.ReadMember(ctx, agent, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%s/proxy%s",
		RobotAddonNamespace, pod, RobotCameraPort, path))
	if err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("the robot's cameras: %v", err))
		return
	}
	defer func() { _ = answer.Body.Close() }()
	if answer.StatusCode != http.StatusOK {
		// The camera service's {"detail"}, or the API server's Status {"message"} when it does not reach the Pod.
		var refused struct{ Detail, Message string }
		body, _ := io.ReadAll(io.LimitReader(answer.Body, 4096))
		_ = json.Unmarshal(body, &refused)
		message := cmp.Or(refused.Detail, refused.Message, strings.TrimSpace(string(body)))
		switch answer.StatusCode {
		case http.StatusBadRequest:
			r.RespondError(c, answer.StatusCode, InvalidRequest, message)
		case http.StatusNotFound:
			r.RespondError(c, answer.StatusCode, NotFound, message)
		case http.StatusServiceUnavailable:
			r.RespondError(c, answer.StatusCode, UpstreamUnavailable, message)
		default:
			r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("the robot's cameras: HTTP %d: %s", answer.StatusCode, message))
		}
		return
	}
	for _, header := range []string{"Content-Type", "Content-Length", "X-Age-Seconds"} {
		if value := answer.Header.Get(header); value != "" {
			c.Header(header, value)
		}
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no") // the console's nginx passes each part of a stream on as it comes
	c.Status(http.StatusOK)
	c.Writer.Flush() // the headers at once: a stream's viewer knows it is there before its first part
	buffer := make([]byte, 64<<10)
	for {
		n, err := answer.Body.Read(buffer)
		if n > 0 {
			if _, err := c.Writer.Write(buffer[:n]); err != nil {
				return // the viewer left
			}
			c.Writer.Flush()
		}
		if err != nil {
			return // the end, or the robot's side gone: the viewer sees the stream end
		}
	}
}

// runningRobotAddon looks up the authorized node and the standing robot Addon Pod on it.
func (r CameraRoutes) runningRobotAddon(c *gin.Context) (string, string, bool) {
	ctx, name, namespace := c.Request.Context(), c.Param("name"), c.Query("namespace")
	if !r.AuthorizeNode(c, namespace, name) {
		return "", "", false
	}
	agent, ok := strings.CutPrefix(namespace, apis.RLarkAgentNamespacePrefix)
	if !ok {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "namespace is the robot's cluster ("+apis.RLarkAgentNamespacePrefix+"<cluster>)")
		return "", "", false
	}

	// The node's camera service: its s1u-robot Pod that runs.
	listed, err := r.ReadMember(ctx, agent, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", RobotAddonNamespace,
		url.Values{"labelSelector": {RobotAddonSelector}, "fieldSelector": {"spec.nodeName=" + name}}.Encode()))
	if err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("the cluster's Pods: %v", err))
		return "", "", false
	}
	var pods corev1.PodList
	err = json.NewDecoder(listed.Body).Decode(&pods)
	_ = listed.Body.Close()
	if listed.StatusCode != http.StatusOK || err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("the cluster's Pods: HTTP %d", listed.StatusCode))
		return "", "", false
	}
	running := slices.IndexFunc(pods.Items, func(pod corev1.Pod) bool {
		return pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning
	})
	if running < 0 {
		r.RespondError(c, http.StatusNotFound, NotFound, "no s1u-robot Pod runs on "+name)
		return "", "", false
	}

	return agent, pods.Items[running].Name, true
}

// stationLogs reads the bounded tail of the standing Station container.
func (r CameraRoutes) stationLogs(c *gin.Context) {
	agent, pod, ok := r.runningRobotAddon(c)
	if !ok {
		return
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?container=station&tailLines=200&limitBytes=131072",
		RobotAddonNamespace, pod)
	answer, err := r.ReadMember(c.Request.Context(), agent, path)
	if err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("Station logs: %v", err))
		return
	}
	defer func() { _ = answer.Body.Close() }()
	if answer.StatusCode != http.StatusOK {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("Station logs: HTTP %d", answer.StatusCode))
		return
	}
	logs, err := io.ReadAll(io.LimitReader(answer.Body, 131072))
	if err != nil {
		r.RespondError(c, http.StatusBadGateway, UpstreamUnavailable, fmt.Sprintf("Station logs: %v", err))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"logs": string(logs)})
}
