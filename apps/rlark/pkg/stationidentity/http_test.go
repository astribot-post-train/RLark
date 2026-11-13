package stationidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testRegistry struct {
	readers map[string]IdentityReader
	err     error
}

func (r testRegistry) Reader(_ context.Context, id string) (IdentityReader, error) {
	if r.err != nil {
		return nil, r.err
	}
	reader := r.readers[id]
	if reader == nil {
		return nil, ErrClusterNotRegistered
	}
	return reader, nil
}

type testObservationSink struct {
	items    []Observation
	evidence []Evidence
	err      error
}

func (s *testObservationSink) Store(_ context.Context, e Evidence, o Observation) error {
	if s.err != nil {
		return s.err
	}
	s.evidence = append(s.evidence, e)
	s.items = append(s.items, o)
	return nil
}

type testSensitiveAction struct {
	calls int
	err   error
}

type testSensitiveAuthorizer struct {
	calls int
	err   error
}

func (a *testSensitiveAuthorizer) Authorize(_ context.Context, _ Evidence, _ string, _ json.RawMessage) error {
	a.calls++
	return a.err
}

func (a *testSensitiveAction) Apply(_ context.Context, _ Evidence, _ string, body json.RawMessage) (any, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return map[string]any{"applied": true, "request": json.RawMessage(body)}, nil
}

func apiRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestHTTPObservationAuthenticatesAndReturnsGenericFailures(t *testing.T) {
	reader := &fakeReader{evidence: validEvidence()}
	sink := &testObservationSink{}
	now := time.Unix(2000, 0)
	api := &API{Registry: testRegistry{readers: map[string]IdentityReader{"c": reader}}, Observations: sink, Now: func() time.Time { return now }}
	body := `{"cluster_id":"c","observation":{"systemUUID":"uuid","roles":["worker"],"wireguard":{"interface":"st-wg0"},"probes":[{"target":"nas:445","ok":true}]}}`
	if w := apiRequest(api, http.MethodPost, "/v1/observations", body, ""); w.Code != 401 {
		t.Fatalf("missing auth status=%d", w.Code)
	}
	if w := apiRequest(api, http.MethodPost, "/v1/observations", body, "token"); w.Code != 202 {
		t.Fatalf("valid status=%d body=%s", w.Code, w.Body.String())
	}
	if sink.evidence[0].NodeUID != "n" || sink.items[0].SystemUUID != "uuid" {
		t.Fatal("stored observation was not attributed to validated subject")
	}
	if w := apiRequest(api, http.MethodPost, "/v1/observations", `{"cluster_id":"c","unexpected":true}`, "token"); w.Code != 400 {
		t.Fatalf("unknown field status=%d", w.Code)
	}
	for _, invalid := range []string{
		`{"cluster_id":"c","observation":{"roles":["worker","worker"]}}`,
		`{"cluster_id":"c","observation":{"wireguard":{}}}`,
		`{"cluster_id":"c","observation":{"wireguard":{"interface":"st-wg0","privateKey":"secret"}}}`,
		`{"cluster_id":"c","observation":{"wireguard":{"interface":"st-wg0","peers":[{"name":"peer","receivedBytes":-1}]}}}`,
		`{"cluster_id":"c","observation":{"wireguard":{"interface":"st-wg0","addresses":["10.0.0.1/32","10.0.0.1/32"]}}}`,
	} {
		if w := apiRequest(api, http.MethodPost, "/v1/observations", invalid, "token"); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid observation accepted: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	for _, malformed := range []string{body + ` {}`, strings.Repeat(" ", 1<<20) + body} {
		if w := apiRequest(api, http.MethodPost, "/v1/observations", malformed, "token"); w.Code != http.StatusBadRequest {
			t.Fatalf("malformed or oversized request status=%d", w.Code)
		}
	}
	badType := httptest.NewRequest(http.MethodPost, "/v1/observations", strings.NewReader(body))
	badType.Header.Set("Content-Type", "text/plain")
	badType.Header.Set("Authorization", "Bearer token")
	badTypeResponse := httptest.NewRecorder()
	api.ServeHTTP(badTypeResponse, badType)
	if badTypeResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status=%d", badTypeResponse.Code)
	}
	api.Registry = testRegistry{err: errors.New("secret cluster configuration")}
	w := apiRequest(api, http.MethodPost, "/v1/observations", body, "token")
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("registry failure leaked or wrong status: %d %s", w.Code, w.Body.String())
	}
	api.Registry = testRegistry{readers: map[string]IdentityReader{"c": reader}}
	reader.err = ErrIdentityUnavailable
	if w := apiRequest(api, http.MethodPost, "/v1/observations", body, "token"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("identity dependency failure status=%d body=%s", w.Code, w.Body.String())
	}
	reader.err = errors.New("invalid bearer token")
	if w := apiRequest(api, http.MethodPost, "/v1/observations", body, "token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid identity status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHTTPReadinessIsSeparateFromLiveness(t *testing.T) {
	api := &API{}
	if got := apiRequest(api, http.MethodGet, "/healthz", "", ""); got.Code != http.StatusOK {
		t.Fatalf("health status=%d", got.Code)
	}
	if got := apiRequest(api, http.MethodGet, "/readyz", "", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured readiness status=%d", got.Code)
	}
	api.Ready = func(context.Context) error { return errors.New("private dependency detail") }
	if got := apiRequest(api, http.MethodGet, "/readyz", "", ""); got.Code != http.StatusServiceUnavailable || strings.Contains(got.Body.String(), "private dependency detail") {
		t.Fatalf("dependency failure leaked or wrong readiness status: %d %s", got.Code, got.Body.String())
	}
	api.Ready = func(context.Context) error { return nil }
	if got := apiRequest(api, http.MethodGet, "/readyz", "", ""); got.Code != http.StatusOK {
		t.Fatalf("ready status=%d", got.Code)
	}
}

func TestHTTPObservationRateLimitRunsAfterIdentityAndBeforePersistence(t *testing.T) {
	reader := &fakeReader{evidence: validEvidence()}
	sink := &testObservationSink{}
	limiter, err := NewTokenBucketObservationLimiter(1, 1, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(5000, 0)
	api := &API{
		Registry:     testRegistry{readers: map[string]IdentityReader{"c": reader}},
		Observations: sink, ObservationLimiter: limiter, Now: func() time.Time { return now },
	}
	body := `{"cluster_id":"c","observation":{"roles":["worker"]}}`
	if got := apiRequest(api, http.MethodPost, "/v1/observations", body, "token"); got.Code != http.StatusAccepted {
		t.Fatalf("initial observation status=%d %s", got.Code, got.Body.String())
	}
	limited := apiRequest(api, http.MethodPost, "/v1/observations", body, "token")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "1" {
		t.Fatalf("limited response=%d retry-after=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	if len(sink.items) != 1 {
		t.Fatalf("rate-limited observation persisted; count=%d", len(sink.items))
	}
	now = now.Add(time.Second)
	if got := apiRequest(api, http.MethodPost, "/v1/observations", body, "token"); got.Code != http.StatusAccepted {
		t.Fatalf("observation was not allowed after refill: %d %s", got.Code, got.Body.String())
	}
}

func TestHTTPChallengeIsConsumedBeforeSensitiveAction(t *testing.T) {
	reader := &fakeReader{evidence: validEvidence()}
	store := NewMemoryChallengeStore()
	action := &testSensitiveAction{}
	now := time.Now()
	authorizer := &testSensitiveAuthorizer{}
	api := &API{Registry: testRegistry{readers: map[string]IdentityReader{"c": reader}}, Challenges: store, Authorizer: authorizer, Actions: action, TTL: time.Minute, Now: func() time.Time { return now }}
	request := `{"value":"desired"}`
	issue := apiRequest(api, http.MethodPost, "/v1/challenges", `{"cluster_id":"c","operation":"configure","request":`+request+`}`, "token")
	if issue.Code != 201 {
		t.Fatalf("issue status=%d %s", issue.Code, issue.Body.String())
	}
	invalidOperation := apiRequest(api, http.MethodPost, "/v1/challenges", `{"cluster_id":"c","operation":"../write","request":`+request+`}`, "token")
	if invalidOperation.Code != http.StatusBadRequest {
		t.Fatalf("invalid operation status=%d", invalidOperation.Code)
	}
	var issued struct {
		ID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil || issued.ID == "" {
		t.Fatal("missing challenge ID", err)
	}
	consumeBody := `{"cluster_id":"c","challenge_id":"` + issued.ID + `","request":` + request + `}`
	path := "/v1/sensitive/configure"
	if w := apiRequest(api, http.MethodPost, path, consumeBody, "token"); w.Code != 200 {
		t.Fatalf("consume status=%d %s", w.Code, w.Body.String())
	}
	if w := apiRequest(api, http.MethodPost, path, consumeBody, "token"); w.Code != 403 {
		t.Fatalf("replay status=%d", w.Code)
	}
	if action.calls != 1 {
		t.Fatalf("action calls=%d", action.calls)
	}
	if authorizer.calls != 3 {
		t.Fatalf("authorization calls=%d, want issue plus both consume attempts", authorizer.calls)
	}

	second := apiRequest(api, http.MethodPost, "/v1/challenges", `{"cluster_id":"c","operation":"configure","request":`+request+`}`, "token")
	var next struct {
		ID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	action.err = errors.New("apply failed")
	failed := apiRequest(api, http.MethodPost, path, `{"cluster_id":"c","challenge_id":"`+next.ID+`","request":`+request+`}`, "token")
	if failed.Code != 500 || strings.Contains(failed.Body.String(), "apply failed") {
		t.Fatalf("action error was exposed: %d %s", failed.Code, failed.Body.String())
	}
	if replay := apiRequest(api, http.MethodPost, path, `{"cluster_id":"c","challenge_id":"`+next.ID+`","request":`+request+`}`, "token"); replay.Code != 403 {
		t.Fatalf("consumed failed action challenge status=%d", replay.Code)
	}
}

func TestHTTPRequiresStationBindingBeforeChallengeOrSensitiveAction(t *testing.T) {
	evidence := validEvidence()
	evidence.StationUID = ""
	evidence.RegisteredStationUID = ""
	evidence.Generation = 0
	evidence.RegisteredGeneration = 0
	reader := &fakeReader{evidence: evidence}
	store := NewMemoryChallengeStore()
	api := &API{
		Registry:   testRegistry{readers: map[string]IdentityReader{"c": reader}},
		Challenges: store,
		Authorizer: &testSensitiveAuthorizer{},
		Actions:    &testSensitiveAction{},
	}
	request := `{"cluster_id":"c","operation":"configure","request":{"desired":true}}`
	if response := apiRequest(api, http.MethodPost, "/v1/challenges", request, "token"); response.Code != http.StatusForbidden {
		t.Fatalf("unbound challenge status=%d body=%s", response.Code, response.Body.String())
	}
	if response := apiRequest(api, http.MethodPost, "/v1/sensitive/configure", `{"cluster_id":"c","challenge_id":"unused","request":{"desired":true}}`, "token"); response.Code != http.StatusForbidden {
		t.Fatalf("unbound sensitive operation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHTTPSensitiveAuthorizationFailsClosed(t *testing.T) {
	reader := &fakeReader{evidence: validEvidence()}
	store := NewMemoryChallengeStore()
	action := &testSensitiveAction{}
	authorizer := &testSensitiveAuthorizer{}
	base := &API{Registry: testRegistry{readers: map[string]IdentityReader{"c": reader}}, Challenges: store, Authorizer: authorizer, Actions: action}
	body := `{"cluster_id":"c","operation":"configure","request":{"value":"desired"}}`
	issue := apiRequest(base, http.MethodPost, "/v1/challenges", body, "token")
	if issue.Code != http.StatusCreated {
		t.Fatalf("authorized issue status=%d", issue.Code)
	}
	var issued struct {
		ID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil || issued.ID == "" {
		t.Fatalf("missing challenge ID: %v", err)
	}
	consume := `{"cluster_id":"c","challenge_id":"` + issued.ID + `","request":{"value":"desired"}}`
	authorizer.err = ErrSensitiveOperationDenied
	if w := apiRequest(base, http.MethodPost, "/v1/sensitive/configure", consume, "token"); w.Code != http.StatusForbidden {
		t.Fatalf("denied consume status=%d", w.Code)
	}
	authorizer.err = errors.New("authorization backend unavailable")
	if w := apiRequest(base, http.MethodPost, "/v1/sensitive/configure", consume, "token"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorization outage status=%d", w.Code)
	}
	if action.calls != 0 {
		t.Fatalf("unauthorized action executed %d times", action.calls)
	}
	authorizer.err = nil
	if w := apiRequest(base, http.MethodPost, "/v1/sensitive/configure", consume, "token"); w.Code != http.StatusOK {
		t.Fatalf("challenge was consumed before authorization recovered: status=%d body=%s", w.Code, w.Body.String())
	}
	if action.calls != 1 {
		t.Fatalf("authorized action calls=%d", action.calls)
	}
}

func TestWireguardPeerObservationValidation(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	key, other := "bGVhZi1rZXktbGVhZi1rZXktbGVhZi1rZXktbGVhZi0=", "bW92ZWQta2V5LW1vdmVkLWtleS1tb3ZlZC1rZXktbW8="
	many := make([]string, wireguardObservationPeers+1)
	for i := range many {
		many[i] = `{"publicKey":"` + base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%032d", i)) + `"}`
	}
	tests := []struct {
		name, peers string
		valid       bool
	}{
		{"an old agent reports none", ``, true},
		{"key, handshake and counters", `{"publicKey":"` + key + `","latestHandshake":"2026-11-14T12:00:50Z","receivedBytes":148,"sentBytes":92},{"publicKey":"` + other + `"}`, true},
		{"as many as allowed", strings.Join(many[1:], ","), true},
		{"too many", strings.Join(many, ","), false},
		{"a name the node chose", `{"publicKey":"` + key + `","name":"robot-01/s1u-01-x86"}`, false},
		{"no key", `{"receivedBytes":148}`, false},
		{"not a key", `{"publicKey":"c2hvcnQ="}`, false},
		{"the same key twice", `{"publicKey":"` + key + `"},{"publicKey":"` + key + `"}`, false},
		{"negative received", `{"publicKey":"` + key + `","receivedBytes":-1}`, false},
		{"negative sent", `{"publicKey":"` + key + `","sentBytes":-1}`, false},
		{"handshake in the future", `{"publicKey":"` + key + `","latestHandshake":"2026-11-14T12:01:01Z"}`, false},
		{"an endpoint", `{"publicKey":"` + key + `","endpoint":"203.0.113.7:40000"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"interface":"st-wg0"}`
			if tc.peers != "" {
				raw = `{"interface":"st-wg0","peers":[` + tc.peers + `]}`
			}
			if err := validateObservation(Observation{Wireguard: json.RawMessage(raw)}, now); (err == nil) != tc.valid {
				t.Fatalf("error = %v, want valid = %v", err, tc.valid)
			}
		})
	}
}
