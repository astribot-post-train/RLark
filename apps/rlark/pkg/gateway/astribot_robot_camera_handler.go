package gateway

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

// cameraStreamLimiter bounds the live camera streams the Gateway relays at once, in all and per user: each stream
// holds a member API server proxy connection for as long as someone watches.
type cameraStreamLimiter struct {
	mu      sync.Mutex
	total   int
	perUser map[string]int
}

func (l *cameraStreamLimiter) acquire(user string, globalLimit, userLimit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perUser == nil {
		l.perUser = make(map[string]int)
	}
	if globalLimit > 0 && l.total >= globalLimit || userLimit > 0 && l.perUser[user] >= userLimit {
		return false
	}
	l.total++
	l.perUser[user]++
	return true
}

func (l *cameraStreamLimiter) release(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	l.perUser[user]--
	if l.perUser[user] == 0 {
		delete(l.perUser, user)
	}
}

// cameraStreamOwner is whom a stream counts against: the IAM portal's user when the portal relays, else the caller.
func cameraStreamOwner(c *gin.Context) string {
	subject := c.GetString(authSubjectKey)
	if strings.HasPrefix(subject, serviceAccountSubject+"astribot-iam-") {
		if user := strings.TrimSpace(c.GetHeader("X-IAM-User")); user != "" {
			return user
		}
	}
	if subject != "" {
		return subject
	}
	return c.ClientIP()
}

func (g *Gateway) registerStationCameraRoutes(nodes gin.IRouter) {
	routes := stations.CameraRoutes{
		ReadMember:   g.readStationMember,
		RespondError: respondStationError,
		AuthorizeNode: func(c *gin.Context, namespace, name string) bool {
			if !g.authorizeTenantObjectByName(c, "nodes", namespace, name) {
				return false
			}
			if _, err := g.accessors["nodes"].get(c.Request.Context(), namespace, name, metav1.GetOptions{}); err != nil {
				respondKubeError(c, err.Error(), err)
				return false
			}
			return true
		},
		AdmitStream: func(c *gin.Context) (func(), bool) {
			owner := cameraStreamOwner(c)
			if !g.cameraStreams.acquire(owner, g.config.CameraMaxLiveStreams, g.config.CameraMaxLiveStreamsPerUser) {
				c.Header("Retry-After", "2")
				respondError(c, http.StatusTooManyRequests, "camera_stream_limit_exceeded", "camera live stream limit exceeded")
				return nil, false
			}
			return func() { g.cameraStreams.release(owner) }, true
		},
	}
	routes.Register(nodes, requireOperator())
}
