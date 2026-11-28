package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	whepTestNode    = "/api/v1/rlinf.io/v1alpha1/nodes/s1u-01"
	whepTestRoute   = whepTestNode + "/cameras/head/whep"
	whepTestScope   = "?namespace=rlark-robot"
	whepTestSource  = "0123456789abcdef0123456789abcdef"
	whepTestMedia   = "/robot/s1u-01/head/whep"
	whepTestSession = whepTestMedia + "/media-session"
	whepTestOffer   = "v=0\r\ns=offer\r\n"
	whepTestAnswer  = "v=0\r\ns=answer\r\n"
)

func setWHEPTestConfig(t *testing.T, origin string) {
	t.Helper()
	t.Setenv("RLARK_CAMERA_WHEP_ORIGIN", origin)
	t.Setenv("RLARK_CAMERA_WHEP_USER", "reader")
	t.Setenv("RLARK_CAMERA_WHEP_PASSWORD", "reader-password")
	t.Setenv("RLARK_CAMERA_WHEP_CONTROL_ORIGIN", origin)
	t.Setenv("RLARK_CAMERA_WHEP_CONTROL_USER", "control")
	t.Setenv("RLARK_CAMERA_WHEP_CONTROL_PASSWORD", "control-password")
	t.Setenv("RLARK_CAMERA_WHEP_NODES", "s1u-01")
	t.Setenv("RLARK_CAMERA_SOURCE_KEY", whepTestSource)
}

func TestCameraWHEPConfiguration(t *testing.T) {
	setWHEPTestConfig(t, "http://media.internal:8889")
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("RLARK_CAMERA_WHEP_ORIGIN", "")
		if media, err := newCameraWHEP(); err != nil || media != nil {
			t.Fatalf("disabled WHEP: media=%v err=%v", media != nil, err)
		}
	})
	for _, tc := range []struct{ name, variable, value string }{
		{"short source key", "RLARK_CAMERA_SOURCE_KEY", strings.Repeat("k", 31)},
		{"empty node allowlist", "RLARK_CAMERA_WHEP_NODES", ""},
		{"empty node entry", "RLARK_CAMERA_WHEP_NODES", "s1u-01,"},
		{"duplicate node", "RLARK_CAMERA_WHEP_NODES", "s1u-01,s1u-01"},
		{"empty reader password", "RLARK_CAMERA_WHEP_PASSWORD", ""},
		{"empty control origin", "RLARK_CAMERA_WHEP_CONTROL_ORIGIN", ""},
		{"empty control password", "RLARK_CAMERA_WHEP_CONTROL_PASSWORD", ""},
		{"shared reader and control username", "RLARK_CAMERA_WHEP_CONTROL_USER", "reader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.variable, tc.value)
			if media, err := newCameraWHEP(); err == nil || media != nil {
				t.Fatalf("invalid WHEP config: media=%v err=%v", media != nil, err)
			}
		})
	}
	if media, err := newCameraWHEP(); err != nil || media == nil || media.ready.Load() {
		t.Fatalf("configured WHEP should start pending reconciliation: media=%v err=%v", media != nil, err)
	}
}

func TestCameraWHEPCredentialsPreferProtectedFile(t *testing.T) {
	path := t.TempDir() + "/camera-credentials.json"
	if err := os.WriteFile(path, []byte(`{"readerUser":"file-reader","readerPassword":"file-reader-password","controlUser":"file-control","controlPassword":"file-control-password","sourceKey":"01234567890123456789012345678901"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RLARK_CAMERA_CREDENTIAL_FILE", path)
	t.Setenv("RLARK_CAMERA_WHEP_USER", "environment-reader")
	credentials, err := cameraWHEPCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.ReaderUser != "file-reader" || credentials.ControlUser != "file-control" {
		t.Fatalf("credential file was not used: reader=%q control=%q", credentials.ReaderUser, credentials.ControlUser)
	}
}

type whepFixture struct {
	*Gateway
	router         *gin.Engine
	sourceStatus   atomic.Int32
	offerStatus    atomic.Int32
	badAnswer      atomic.Bool
	replacement    atomic.Bool
	allowDelete    atomic.Bool
	renewals       atomic.Int32
	deletions      atomic.Int32
	sourceReleases atomic.Int32
	leasePaths     chan string
}

func newWHEPFixture(t *testing.T, control http.HandlerFunc) *whepFixture {
	t.Helper()
	f := &whepFixture{}
	f.sourceStatus.Store(http.StatusNoContent)
	f.offerStatus.Store(http.StatusCreated)
	f.allowDelete.Store(true)
	f.leasePaths = make(chan string, 64)
	const pods = "/api/proxy/robot/api/kubernetes/api/v1/namespaces/rlark-system/pods"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pods && r.Method == http.MethodGet:
			if r.URL.Query().Get("fieldSelector") != "spec.nodeName=s1u-01" ||
				r.URL.Query().Get("labelSelector") != robotAddonSelector {
				t.Errorf("unexpected Pod selectors: %s", r.URL.RawQuery)
			}
			pod := "video"
			if f.replacement.Load() {
				pod = "replacement-video"
			}
			writeWHEPTestBody(t, w, fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"status":{"phase":"Running"}}]}`, pod))
		case r.URL.Path == pods+"/video:18891/proxy/cameras/head.jpg" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "image/jpeg")
			writeWHEPTestBody(t, w, "camera-jpeg")
		case strings.HasPrefix(r.URL.Path, pods+"/") && strings.Contains(r.URL.Path, "/proxy/cameras/") && strings.Contains(r.URL.Path, "/h264-lease/") && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
			if r.Header.Get("X-RLark-Camera-Source-Key") != whepTestSource || r.ContentLength != 0 {
				t.Error("the Addon did not receive the configured key and an empty lease request")
			}
			if r.Header.Get("X-RLark-Camera-Source-Path") != "/robot/s1u-01/head" {
				t.Errorf("unexpected publisher path: %q", r.Header.Get("X-RLark-Camera-Source-Path"))
			}
			f.leasePaths <- r.Method + " " + r.URL.Path
			if r.Method == http.MethodPut {
				f.renewals.Add(1)
				w.WriteHeader(int(f.sourceStatus.Load()))
			} else {
				f.sourceReleases.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}
		case r.URL.Path == whepTestMedia && r.Method == http.MethodPost:
			assertWHEPBasicAuth(t, r, "reader")
			offer, err := io.ReadAll(r.Body)
			if err != nil || string(offer) != whepTestOffer || r.Header.Get("Content-Type") != "application/sdp" {
				t.Errorf("unexpected upstream SDP offer: %q, %v", offer, err)
			}
			w.Header().Set("Content-Type", "application/sdp")
			w.Header().Set("Location", whepTestSession)
			if f.badAnswer.Load() {
				w.Header().Set("Content-Type", "text/plain")
			}
			w.WriteHeader(int(f.offerStatus.Load()))
			writeWHEPTestBody(t, w, whepTestAnswer)
		case r.URL.Path == whepTestSession && r.Method == http.MethodDelete:
			assertWHEPBasicAuth(t, r, "reader")
			f.deletions.Add(1)
			if !f.allowDelete.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNotFound) // MediaMTX already removed the reader.
		case control != nil && strings.HasPrefix(r.URL.Path, "/v3/"):
			assertWHEPBasicAuth(t, r, "control")
			control(w, r)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	setWHEPTestConfig(t, upstream.URL)
	media, err := newCameraWHEP()
	if err != nil {
		t.Fatal(err)
	}
	media.ready.Store(control == nil)
	f.Gateway = &Gateway{
		config: Config{ServerAddress: upstream.URL}, serverTransport: http.DefaultTransport.(*http.Transport),
		jwtSigningKey: []byte(whepTestSource),
		astribotState: astribotState{
			cameraMedia: media,
			stationClient: stationHandlerClient(
				stationHandlerCluster("robot", "rlark-robot"),
				stationHandlerCluster("duplicate-a", "rlark-duplicate"),
				stationHandlerCluster("duplicate-b", "rlark-duplicate"),
			),
		},
		accessors: map[string]*resourceAccessor{"nodes": {
			get: func(_ context.Context, namespace, name string, _ metav1.GetOptions) (any, error) {
				if namespace != "rlark-robot" || name != "s1u-01" {
					t.Errorf("unexpected Node lookup: %s/%s", namespace, name)
				}
				return stationHandlerNode(name, namespace, nil, nil), nil
			},
		}},
	}
	t.Cleanup(func() {
		f.allowDelete.Store(true)
		media.mu.Lock()
		for _, session := range media.sessions {
			session.mu.Lock()
			session.closing = true
			session.mu.Unlock()
		}
		media.mu.Unlock()
		waitForWHEP(t, func() bool {
			media.mu.Lock()
			defer media.mu.Unlock()
			return len(media.sessions) == 0
		})
	})
	gin.SetMode(gin.TestMode)
	f.router = gin.New()
	f.registerRobotRoutes(f.router.Group("/api/v1/rlinf.io/v1alpha1/nodes", f.requireJWT()))
	return f
}

func assertWHEPBasicAuth(t *testing.T, r *http.Request, user string) {
	t.Helper()
	gotUser, gotPassword, ok := r.BasicAuth()
	if !ok || gotUser != user || gotPassword != user+"-password" || r.Header.Get("X-RLark-Camera-Source-Key") != "" {
		t.Errorf("unexpected credentials on %s %s", r.Method, r.URL.Path)
	}
}

func writeWHEPTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("writing upstream response: %v", err)
	}
}

func (f *whepFixture) call(t *testing.T, method, path, role, user, kind, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", kind)
	if role != "" {
		claims := authClaims{Role: role, RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "rlark-gateway", Subject: "service-account:astribot-iam-" + role,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(f.jwtSigningKey)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-IAM-User", user)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s as %q/%q: HTTP %d, want %d, body %q", method, path, role, user, response.Code, want, response.Body.String())
	}
	return response
}

func (f *whepFixture) open(t *testing.T, owner string) string {
	t.Helper()
	response := f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, owner, "application/sdp", whepTestOffer, http.StatusCreated)
	location := response.Header().Get("Location")
	if !strings.HasPrefix(location, whepTestRoute+"/") || !strings.HasSuffix(location, whepTestScope) ||
		strings.Contains(location, "media-session") || response.Header().Get("Content-Type") != "application/sdp" ||
		response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != whepTestAnswer {
		t.Fatalf("unexpected SDP response: %v body=%q", response.Header(), response.Body.String())
	}
	return location
}

func (f *whepFixture) update(t *testing.T, method, location, owner string, want int) {
	t.Helper()
	f.call(t, method, location, roleAdmin, owner, "", "", want)
}

func waitForWHEP(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for WHEP cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCameraWHEPRouteAuthorizationAndErrors(t *testing.T) {
	f := newWHEPFixture(t, nil)
	f.cameraMedia.nodes = map[string]struct{}{"another-node": {}}
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusNotFound)
	if f.renewals.Load() != 0 {
		t.Fatal("a node outside the WHEP allowlist renewed an Addon lease")
	}
	f.cameraMedia.nodes = map[string]struct{}{"s1u-01": {}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		path := whepTestRoute
		if method != http.MethodPost {
			path += "/session"
		}
		f.call(t, method, path+whepTestScope, "", "", "application/sdp", whepTestOffer, http.StatusUnauthorized)
		f.call(t, method, path+whepTestScope, "user", "alice", "application/sdp", whepTestOffer, http.StatusForbidden)
	}
	for _, tc := range []struct {
		path, kind, body string
		want             int
	}{
		{whepTestRoute + whepTestScope, "text/plain", whepTestOffer, http.StatusBadRequest},
		{whepTestRoute + whepTestScope, "application/sdp", "", http.StatusBadRequest},
		{whepTestRoute + whepTestScope, "application/sdp", strings.Repeat("x", cameraSDPLimit+1), http.StatusBadRequest},
		{whepTestRoute, "application/sdp", whepTestOffer, http.StatusBadRequest},
		{whepTestNode + "/cameras/invalid/whep" + whepTestScope, "application/sdp", whepTestOffer, http.StatusBadRequest},
		{whepTestRoute + "?namespace=rlark-duplicate", "application/sdp", whepTestOffer, http.StatusConflict},
	} {
		f.call(t, http.MethodPost, tc.path, roleAdmin, "alice", tc.kind, tc.body, tc.want)
	}
	if f.renewals.Load() != 0 {
		t.Fatal("rejected requests renewed an Addon lease")
	}
	f.sourceStatus.Store(http.StatusConflict)
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusServiceUnavailable)
	f.sourceStatus.Store(http.StatusNoContent)
	f.offerStatus.Store(http.StatusBadRequest)
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusBadGateway)
	f.offerStatus.Store(http.StatusCreated)
	f.badAnswer.Store(true)
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusBadGateway)
	if f.sourceReleases.Load() < 2 {
		t.Fatalf("failed opens released %d source leases, want at least 2", f.sourceReleases.Load())
	}
	waitForWHEP(t, func() bool { return f.deletions.Load() > 0 })

	f.cameraMedia = nil
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusNotFound)
	jpeg := f.call(t, http.MethodGet, whepTestNode+"/cameras/head.jpg"+whepTestScope, "user", "alice", "", "", http.StatusOK)
	if jpeg.Header().Get("Content-Type") != "image/jpeg" || jpeg.Body.String() != "camera-jpeg" {
		t.Fatalf("unexpected JPEG response: %v %q", jpeg.Header(), jpeg.Body.String())
	}
}

func TestCameraWHEPUsesDistinctReaderLeasesForSameView(t *testing.T) {
	f := newWHEPFixture(t, nil)
	first := f.open(t, "alice")
	second := f.open(t, "alice")
	firstID := strings.TrimSuffix(strings.TrimPrefix(first, whepTestRoute+"/"), whepTestScope)
	secondID := strings.TrimSuffix(strings.TrimPrefix(second, whepTestRoute+"/"), whepTestScope)
	if firstID == secondID || uuid.Validate(firstID) != nil || uuid.Validate(secondID) != nil {
		t.Fatalf("readers did not receive distinct UUIDs: %q and %q", firstID, secondID)
	}
	for _, id := range []string{firstID, secondID} {
		request := <-f.leasePaths
		if !strings.HasSuffix(request, "/h264-lease/"+id) || !strings.HasPrefix(request, "PUT ") {
			t.Fatalf("unexpected reader lease request: %q", request)
		}
	}
	f.update(t, http.MethodDelete, first, "alice", http.StatusAccepted)
	waitForWHEP(t, func() bool { return f.sourceReleases.Load() >= 1 })
	if f.renewals.Load() != 2 {
		t.Fatalf("same-view readers renewed %d leases, want 2", f.renewals.Load())
	}
}

func TestCameraWHEPSessionBindingAndExpiry(t *testing.T) {
	f := newWHEPFixture(t, nil)
	location := f.open(t, "alice")
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		f.update(t, method, location, "bob", http.StatusNotFound)
		f.update(t, method, strings.Replace(location, "s1u-01", "s1u-02", 1), "alice", http.StatusNotFound)
		f.update(t, method, strings.Replace(location, "/head/", "/torso/", 1), "alice", http.StatusNotFound)
		f.update(t, method, strings.Replace(location, "rlark-robot", "rlark-other", 1), "alice", http.StatusNotFound)
	}
	if f.renewals.Load() != 1 {
		t.Fatal("unbound requests renewed the Addon lease")
	}

	id := strings.TrimSuffix(strings.TrimPrefix(location, whepTestRoute+"/"), whepTestScope)
	m := f.cameraMedia
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session == nil {
		t.Fatal("session missing")
	}
	f.update(t, http.MethodPut, location, "alice", http.StatusNoContent)
	session.mu.Lock()
	remaining := time.Until(session.expires)
	session.expires = time.Now().Add(-time.Second)
	session.mu.Unlock()
	if remaining < cameraWHEPLease-time.Second || remaining > cameraWHEPLease {
		t.Fatalf("unexpected renewed lease: %s", remaining)
	}
	f.allowDelete.Store(false)
	f.update(t, http.MethodPut, location, "alice", http.StatusGone)
	if f.renewals.Load() != 2 {
		t.Fatal("expired session renewed the Addon lease")
	}
	waitForWHEP(t, func() bool { return f.deletions.Load() > 0 })
}

func TestCameraWHEPRejectsReplacementPodOnRenewal(t *testing.T) {
	f := newWHEPFixture(t, nil)
	location := f.open(t, "alice")
	f.replacement.Store(true)
	f.update(t, http.MethodPut, location, "alice", http.StatusServiceUnavailable)
	f.update(t, http.MethodPut, location, "alice", http.StatusGone)
	if f.renewals.Load() != 1 {
		t.Fatal("replacement Pod received the existing session's renewal")
	}
}

func TestCameraWHEPRetainsQuotaUntilCleanupSucceeds(t *testing.T) {
	const oldQuotaKey = "service-account:astribot-iam-admin\x00alice"
	f := newWHEPFixture(t, nil)
	first := f.open(t, "alice")
	for range 3 {
		f.open(t, "alice")
	}
	openLimited := func() {
		f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusTooManyRequests)
	}
	openLimited()
	f.allowDelete.Store(false)
	f.update(t, http.MethodDelete, first, "alice", http.StatusAccepted)
	f.update(t, http.MethodPut, first, "alice", http.StatusGone)
	waitForWHEP(t, func() bool { return f.deletions.Load() > 0 })
	openLimited()
	f.allowDelete.Store(true)
	waitForWHEP(t, func() bool {
		limiter := &f.cameraMedia.limiter
		if !limiter.acquire(oldQuotaKey, 20, 4) {
			return false
		}
		limiter.release(oldQuotaKey)
		return true
	})
	f.update(t, http.MethodPut, first, "alice", http.StatusNotFound)
	f.open(t, "alice")
}

func TestCameraWHEPQuotaSeparatesTrustedIAMAdministrators(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := func(subject, iamUser string) string {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, whepTestRoute+whepTestScope, nil)
		ctx.Request.Header.Set("X-IAM-User", iamUser)
		ctx.Set(authSubjectKey, subject)
		return cameraWHEPOwner(ctx)
	}

	adminAlice := owner("service-account:astribot-iam-admin", "alice")
	adminBob := owner("service-account:astribot-iam-admin", "bob")
	if adminAlice == adminBob {
		t.Fatalf("trusted IAM administrators share a WHEP quota key: %q", adminAlice)
	}
	var limiter cameraStreamLimiter
	for range 4 {
		if !limiter.acquire(adminAlice, 20, 4) || !limiter.acquire(adminBob, 20, 4) {
			t.Fatal("trusted IAM administrator quota was exhausted independently")
		}
	}
	if limiter.acquire(adminAlice, 20, 4) || limiter.acquire(adminBob, 20, 4) {
		t.Fatal("trusted IAM administrator quota accepted a fifth session")
	}
}

func TestCameraWHEPQuotaIgnoresUntrustedIAMHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := func(subject, iamUser string) string {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, whepTestRoute+whepTestScope, nil)
		ctx.Request.Header.Set("X-IAM-User", iamUser)
		ctx.Set(authSubjectKey, subject)
		return cameraWHEPOwner(ctx)
	}

	ordinary := owner("alice", "alice")
	spoofed := owner("alice", "bob")
	if ordinary != spoofed {
		t.Fatalf("untrusted IAM headers split WHEP quota keys: %q and %q", ordinary, spoofed)
	}
	var limiter cameraStreamLimiter
	for range 4 {
		if !limiter.acquire(ordinary, 20, 4) {
			t.Fatal("ordinary WHEP quota unexpectedly exhausted")
		}
	}
	if limiter.acquire(spoofed, 20, 4) {
		t.Fatal("untrusted IAM header bypassed the WHEP quota")
	}
}

func TestCameraWHEPWaitsForStartupReconciliation(t *testing.T) {
	const oldSession = "8a7af81d-fb78-4c04-a876-6d1801afa0ff"
	var kicks, lists atomic.Int32
	var allowKick atomic.Bool
	f := newWHEPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v3/webrtc/sessions/list":
			lists.Add(1)
			if kicks.Load() < 2 {
				writeWHEPTestBody(t, w, fmt.Sprintf(`{"itemCount":1,"items":[{"id":%q}]}`, oldSession))
				return
			}
			writeWHEPTestBody(t, w, `{"itemCount":0,"items":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v3/webrtc/sessions/kick/"+oldSession:
			kicks.Add(1)
			if !allowKick.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected control request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
	if err := f.cameraMedia.drainPreviousSessions(context.Background()); err == nil {
		t.Fatal("MediaMTX failure did not block startup")
	}
	f.call(t, http.MethodPost, whepTestRoute+whepTestScope, roleAdmin, "alice", "application/sdp", whepTestOffer, http.StatusServiceUnavailable)
	if f.renewals.Load() != 0 {
		t.Fatal("Gateway renewed a source during startup")
	}
	allowKick.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.cameraMedia.reconcileStartup(ctx)
	if !f.cameraMedia.ready.Load() || lists.Load() != 3 || kicks.Load() != 2 {
		t.Fatalf("expected list/kick/list after recovery, got lists=%d kicks=%d ready=%v",
			lists.Load(), kicks.Load(), f.cameraMedia.ready.Load())
	}
	f.open(t, "alice")
}
