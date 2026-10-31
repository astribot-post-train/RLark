package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/submission"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

type jobService struct {
	calls              int
	target             submission.ClusterTarget
	request            submission.Request
	uid, version, name string
	err                error
}

func (s *jobService) Submit(_ context.Context, t submission.ClusterTarget, r submission.Request) (submission.Summary, error) {
	s.calls++
	s.target = t
	s.request = r
	return submission.Summary{Name: "job", UID: "exact-job", BindingValid: true}, s.err
}
func (s *jobService) List(_ context.Context, t submission.ClusterTarget) ([]submission.Summary, error) {
	s.calls++
	s.target = t
	return []submission.Summary{}, s.err
}
func (s *jobService) Suspend(_ context.Context, t submission.ClusterTarget, n, u, v string) (submission.Summary, error) {
	s.calls++
	s.target = t
	s.name = n
	s.uid = u
	s.version = v
	return submission.Summary{}, s.err
}

func TestJobRoutesRequireAdministratorAndOptIn(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		a, _, _, _ := fixture(t)
		s := &jobService{}
		a.Jobs = s
		path := prefix + "clusters/cloud-agents/cloud/jobs"
		if method == http.MethodPatch {
			path += "/job/suspend"
		}
		for _, role := range []string{"anonymous", "user"} {
			r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			if role == "user" {
				r.Header.Set("Authorization", "Bearer "+token(t, claims{Role: "user", RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "user", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}))
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			want := 401
			if role == "user" {
				want = 403
			}
			require.Equal(t, want, w.Code)
		}
		require.Zero(t, s.calls)
		a.Jobs = nil
		w := request(t, a, method, path, `{}`)
		require.Equal(t, 404, w.Code)
	}
	a, _, _, _ := fixture(t)
	require.False(t, getDetail(t, a).JobSubmissionEnabled)
	a.Jobs = &jobService{}
	require.True(t, getDetail(t, a).JobSubmissionEnabled)
}
func TestJobRouteIdentityStrictnessAndErrors(t *testing.T) {
	a, cc, _, _ := fixture(t)
	s := &jobService{}
	a.Jobs = s
	path := prefix + "clusters/cloud-agents/cloud/jobs"
	w := request(t, a, "POST", path, `{"clusterUID":"exact-cluster","operationID":"operation","environment":"prepared","process":{"argv":["/usr/bin/python3"],"env":[],"directory":"/work"}}`)
	require.Equal(t, 201, w.Code)
	require.Equal(t, "exact-cluster", s.target.UID)
	require.Equal(t, "cloud-agents", s.target.Namespace)
	require.Equal(t, "prepared", s.request.Environment)
	before := s.calls
	for _, body := range []string{`{"clusterUID":"uid","tenantID":"foreign"}`, `{"clusterUID":"uid","process":{"unknown":true}}`, `{} {}`} {
		w = request(t, a, "POST", path, body)
		require.Equal(t, 400, w.Code)
	}
	require.Equal(t, before, s.calls)
	w = request(t, a, "GET", path, "")
	require.Equal(t, 200, w.Code)
	require.Equal(t, string(cc.UID), s.target.UID)
	w = request(t, a, "PATCH", path+"/job/suspend", `{"uid":"job-uid","resourceVersion":"42"}`)
	require.Equal(t, 200, w.Code)
	require.Equal(t, "job-uid", s.uid)
	require.Equal(t, "42", s.version)
	w = request(t, a, "PATCH", path+"/job/suspend", `{"uid":"job-uid"}`)
	require.Equal(t, 400, w.Code)
	for _, tt := range []struct {
		err  error
		code int
	}{{submission.ErrConflict, 409}, {submission.ErrInvalid, 400}, {errors.New("sensitive upstream credentials"), 503}} {
		s.err = tt.err
		w = request(t, a, "POST", path, `{"clusterUID":"uid"}`)
		require.Equal(t, tt.code, w.Code)
		require.NotContains(t, w.Body.String(), "sensitive")
		require.NotEmpty(t, w.Header().Get("X-Request-ID"))
	}
}
