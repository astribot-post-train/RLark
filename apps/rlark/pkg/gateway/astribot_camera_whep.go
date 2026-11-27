package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	cameraWHEPLease = 14 * time.Second
	cameraSDPLimit  = 64 << 10
)

var whepLocationSegment = regexp.MustCompile("^[a-zA-Z0-9_-]{1,128}$")

type cameraWHEP struct {
	origin   *url.URL
	user     string
	password string
	key      string
	client   *http.Client
	limiter  cameraStreamLimiter
	mu       sync.Mutex
	sessions map[string]*cameraWHEPSession
}

type cameraWHEPSession struct {
	mu                         sync.Mutex
	owner, namespace, node     string
	agent, pod, view, upstream string
	expires                    time.Time
	closing                    bool
}

func newCameraWHEP() (*cameraWHEP, error) {
	origin := os.Getenv("RLARK_CAMERA_WHEP_ORIGIN")
	if origin == "" {
		return nil, nil
	}
	target, err := url.Parse(origin)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") ||
		target.Hostname() == "" || target.User != nil || (target.Path != "" && target.Path != "/") ||
		target.RawQuery != "" || target.Fragment != "" {
		return nil, fmt.Errorf("RLARK_CAMERA_WHEP_ORIGIN must be a private HTTP(S) origin")
	}
	user, password := os.Getenv("RLARK_CAMERA_WHEP_USER"), os.Getenv("RLARK_CAMERA_WHEP_PASSWORD")
	key := os.Getenv("RLARK_CAMERA_SOURCE_KEY")
	if user == "" || password == "" || len(key) < 32 {
		return nil, fmt.Errorf("camera WHEP requires reader credentials and a source key of at least 32 bytes")
	}
	return &cameraWHEP{
		origin: target, user: user, password: password, key: key,
		client: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sessions: make(map[string]*cameraWHEPSession),
	}, nil
}

func (m *cameraWHEP) request(ctx context.Context, method, path string, sdp []byte) (*http.Response, error) {
	target := *m.origin
	target.Path = path
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(sdp))
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/sdp")
	}
	req.SetBasicAuth(m.user, m.password)
	return m.client.Do(req)
}

// MediaMTX returns a private session URL. Validate it against the stream
// requested by the Gateway and keep it private even on the same origin.
func cameraWHEPLocation(basePath, location string) (string, bool) {
	segment, ok := strings.CutPrefix(location, basePath+"/")
	return location, ok && whepLocationSegment.MatchString(segment)
}

func (g *Gateway) cameraWHEPSource(c *gin.Context) (string, string, bool) {
	namespace := c.Query("namespace")
	if len(validation.IsDNS1123Subdomain(c.Param("name"))) > 0 ||
		len(validation.IsDNS1123Label(namespace)) > 0 || !slices.Contains(robotCameraViews, c.Param("camera")) {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, "invalid node, namespace or camera view")
		return "", "", false
	}
	index, ok := g.stationQueryClusters(c)
	if !ok {
		return "", "", false
	}
	if index.owners[namespace] == "" {
		respondError(c, http.StatusConflict, CodeInvalidRequest, "camera cluster namespace does not have a unique owner")
		return "", "", false
	}
	return g.runningRobotAddon(c)
}

func (g *Gateway) renewCameraSource(ctx context.Context, agent, pod, view string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%s/proxy/cameras/%s/h264-lease",
		robotAddonNamespace, pod, robotCameraPort, view)
	client := &http.Client{
		Transport:     g.serverTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := g.proxyKubeRequestUsing(ctx, client, http.MethodPut, agent, path, nil,
		map[string]string{"X-RLark-Camera-Source-Key": g.cameraMedia.key})
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusNoContent
}

func (g *Gateway) openCameraWHEP(c *gin.Context) {
	m := g.cameraMedia
	if m == nil {
		c.Status(http.StatusNotFound)
		return
	}
	contentType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	offer, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, cameraSDPLimit))
	if err != nil || contentType != "application/sdp" || len(offer) == 0 {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, "a bounded SDP offer is required")
		return
	}
	agent, pod, ok := g.cameraWHEPSource(c)
	if !ok {
		return
	}
	owner := cameraStreamOwner(c)
	if !m.limiter.acquire(owner, 20, 4) {
		c.Status(http.StatusTooManyRequests)
		return
	}
	handedOver := false
	defer func() {
		if !handedOver {
			m.limiter.release(owner)
		}
	}()
	ctx := c.Request.Context()
	node, view := c.Param("name"), c.Param("camera")
	if !g.renewCameraSource(ctx, agent, pod, view) {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	path := "/" + agent + "/" + node + "/" + view + "/whep"
	// The lease starts the publisher asynchronously. A missing stream may
	// become ready during the initial GOP without requiring a second offer.
	connect, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result *http.Response
	for {
		result, err = m.request(connect, http.MethodPost, path, offer)
		if err != nil {
			c.Status(http.StatusBadGateway)
			return
		}
		if result.StatusCode != http.StatusNotFound {
			break
		}
		_ = result.Body.Close()
		select {
		case <-connect.Done():
			c.Status(http.StatusServiceUnavailable)
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer func() { _ = result.Body.Close() }()
	if result.StatusCode != http.StatusCreated {
		c.Status(http.StatusBadGateway)
		return
	}
	upstream, ok := cameraWHEPLocation(path, result.Header.Get("Location"))
	if !ok {
		c.Status(http.StatusBadGateway)
		return
	}
	id := uuid.NewString()
	session := &cameraWHEPSession{
		owner: owner, namespace: c.Query("namespace"), node: node, view: view,
		agent: agent, pod: pod, upstream: upstream, expires: time.Now().Add(cameraWHEPLease),
	}
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	handedOver = true
	go m.reap(id, session)

	answer, err := io.ReadAll(io.LimitReader(result.Body, cameraSDPLimit+1))
	answerType, _, _ := mime.ParseMediaType(result.Header.Get("Content-Type"))
	if err != nil || answerType != "application/sdp" || len(answer) == 0 || len(answer) > cameraSDPLimit {
		session.mu.Lock()
		session.closing = true
		session.mu.Unlock()
		c.Status(http.StatusBadGateway)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Location", c.Request.URL.Path+"/"+id+"?"+url.Values{"namespace": {session.namespace}}.Encode())
	c.Data(http.StatusCreated, "application/sdp", answer)
}

func (g *Gateway) updateCameraWHEP(c *gin.Context) {
	m := g.cameraMedia
	if m == nil {
		c.Status(http.StatusNotFound)
		return
	}
	m.mu.Lock()
	session := m.sessions[c.Param("session")]
	m.mu.Unlock()
	if session == nil || session.owner != cameraStreamOwner(c) || session.namespace != c.Query("namespace") ||
		session.node != c.Param("name") || session.view != c.Param("camera") {
		c.Status(http.StatusNotFound)
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if c.Request.Method == http.MethodDelete {
		session.closing = true
		c.Status(http.StatusAccepted)
		return
	}
	if session.closing || !time.Now().Before(session.expires) {
		c.Status(http.StatusGone)
		return
	}
	agent, pod, ok := g.cameraWHEPSource(c)
	if !ok {
		session.closing = true
		return
	}
	if session.agent != agent || session.pod != pod || !g.renewCameraSource(c.Request.Context(), agent, pod, session.view) {
		session.closing = true
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if !time.Now().Before(session.expires) {
		session.closing = true
		c.Status(http.StatusGone)
		return
	}
	session.expires = time.Now().Add(cameraWHEPLease)
	c.Status(http.StatusNoContent)
}

func (m *cameraWHEP) reap(id string, session *cameraWHEPSession) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for range tick.C {
		session.mu.Lock()
		session.closing = session.closing || !time.Now().Before(session.expires)
		closing := session.closing
		session.mu.Unlock()
		if !closing {
			continue
		}
		result, err := m.request(context.Background(), http.MethodDelete, session.upstream, nil)
		if err != nil {
			continue
		}
		_ = result.Body.Close()
		switch result.StatusCode {
		case http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusGone:
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
			m.limiter.release(session.owner)
			return
		}
	}
}
