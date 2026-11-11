package stationidentity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	ErrClusterNotRegistered              = errors.New("cluster is not registered")
	ErrIdentityUnavailable               = errors.New("identity verification unavailable")
	ErrIdentityDenied                    = errors.New("identity denied")
	ErrObservationUnavailable            = errors.New("observation persistence unavailable")
	ErrChallengeUnavailable              = errors.New("challenge persistence unavailable")
	ErrChallengeDenied                   = errors.New("challenge denied")
	ErrSensitiveOperationDenied          = errors.New("sensitive operation denied")
	ErrSensitiveAuthorizationUnavailable = errors.New("sensitive authorization unavailable")
)

// ClusterRegistry resolves a caller-supplied routing hint only to a trusted,
// server-owned registration. Implementations must not fetch arbitrary URLs.
type ClusterRegistry interface {
	Reader(context.Context, string) (IdentityReader, error)
}

type Observation struct {
	SystemUUID string          `json:"systemUUID,omitempty"`
	BootTime   *time.Time      `json:"bootTime,omitempty"`
	Roles      []string        `json:"roles,omitempty"`
	Wireguard  json.RawMessage `json:"wireguard,omitempty"`
	Probes     []Probe         `json:"probes,omitempty"`
}

type Probe struct {
	Target    string `json:"target"`
	Via       string `json:"via,omitempty"`
	OK        bool   `json:"ok"`
	RTTMillis *int64 `json:"rttMs,omitempty"`
}

type ObservationSink interface {
	Store(context.Context, Evidence, Observation) error
}
type ObservationLimiter interface {
	Allow(Evidence, time.Time) bool
}
type SensitiveAuthorizer interface {
	Authorize(context.Context, Evidence, string, json.RawMessage) error
}
type SensitiveAction interface {
	Apply(context.Context, Evidence, string, json.RawMessage) (any, error)
}

type API struct {
	Registry           ClusterRegistry
	Challenges         ChallengeStore
	Observations       ObservationSink
	ObservationLimiter ObservationLimiter
	Authorizer         SensitiveAuthorizer
	Actions            SensitiveAction
	Ready              func(context.Context) error
	TTL                time.Duration
	Now                func() time.Time
	NetworkSummary     *NodeNetworkSummaryReader
	GatewayCredential  func() ([]byte, error)
}

type observationRequest struct {
	ClusterID   string      `json:"cluster_id"`
	Observation Observation `json:"observation"`
}
type challengeRequest struct {
	ClusterID string          `json:"cluster_id"`
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
}
type sensitiveRequest struct {
	ClusterID   string          `json:"cluster_id"`
	ChallengeID string          `json:"challenge_id"`
	Request     json.RawMessage `json:"request"`
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		if a.Ready == nil || a.Ready(r.Context()) != nil {
			writeError(w, http.StatusServiceUnavailable, "station API dependencies are unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if a.Registry == nil {
		writeError(w, http.StatusServiceUnavailable, "station API is not configured")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return
	}
	if r.URL.Path == "/v1/internal/node-network" {
		a.nodeNetwork(w, r, token)
		return
	}
	switch {
	case r.URL.Path == "/v1/observations":
		a.observe(w, r, token)
	case r.URL.Path == "/v1/challenges":
		a.issue(w, r, token)
	case strings.HasPrefix(r.URL.Path, "/v1/sensitive/"):
		a.sensitive(w, r, token, strings.TrimPrefix(r.URL.Path, "/v1/sensitive/"))
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *API) nodeNetwork(w http.ResponseWriter, r *http.Request, token string) {
	if a.NetworkSummary == nil || a.GatewayCredential == nil {
		writeError(w, http.StatusServiceUnavailable, "network summary is not configured")
		return
	}
	credential, err := a.GatewayCredential()
	if err != nil || len(credential) < 32 || len(credential) > 4096 {
		writeError(w, http.StatusServiceUnavailable, "network summary credential is unavailable")
		return
	}
	expected := sha256.Sum256(credential)
	provided := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(expected[:], provided[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid service credential")
		return
	}
	var request struct {
		ClusterID string `json:"cluster_id"`
		NodeName  string `json:"node_name"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.ClusterID == "" || len(request.ClusterID) > 128 || request.NodeName == "" || len(request.NodeName) > 253 {
		writeError(w, http.StatusBadRequest, "cluster_id and node_name are required")
		return
	}
	summary, err := a.NetworkSummary.Read(r.Context(), request.ClusterID, request.NodeName)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "network summary is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (a *API) authenticate(ctx context.Context, clusterID, token string) (Evidence, error) {
	if clusterID == "" {
		return Evidence{}, ErrIdentityDenied
	}
	reader, err := a.Registry.Reader(ctx, clusterID)
	if err != nil {
		if errors.Is(err, ErrClusterNotRegistered) {
			return Evidence{}, ErrIdentityDenied
		}
		return Evidence{}, ErrIdentityUnavailable
	}
	if reader == nil {
		return Evidence{}, ErrIdentityUnavailable
	}
	e, err := reader.Authenticate(ctx, token)
	if err != nil {
		if errors.Is(err, ErrIdentityUnavailable) {
			return Evidence{}, ErrIdentityUnavailable
		}
		return Evidence{}, ErrIdentityDenied
	}
	if err := Validate(e); err != nil {
		return Evidence{}, ErrIdentityDenied
	}
	if e.ClusterID != clusterID {
		return Evidence{}, ErrIdentityDenied
	}
	return e, nil
}

func (a *API) observe(w http.ResponseWriter, r *http.Request, token string) {
	if a.Observations == nil {
		writeError(w, http.StatusServiceUnavailable, "observation storage unavailable")
		return
	}
	var req observationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	e, err := a.authenticate(r.Context(), req.ClusterID, token)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := validateObservation(req.Observation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.ObservationLimiter != nil && !a.ObservationLimiter.Allow(e, a.now()) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "observation rate limit exceeded")
		return
	}
	if err := a.Observations.Store(r.Context(), e, req.Observation); err != nil {
		writeError(w, http.StatusServiceUnavailable, "observation storage unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "station_uid": e.StationUID, "node_uid": e.NodeUID, "reported_at": a.now().UTC()})
}

func (a *API) issue(w http.ResponseWriter, r *http.Request, token string) {
	if a.Challenges == nil {
		writeError(w, http.StatusServiceUnavailable, "challenge storage unavailable")
		return
	}
	var req challengeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validJSON(req.Request) || len(req.Request) > 1<<20 || !operationPattern.MatchString(req.Operation) {
		writeError(w, http.StatusBadRequest, "invalid challenge request")
		return
	}
	e, err := a.authenticate(r.Context(), req.ClusterID, token)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if e.StationUID == "" || e.Generation <= 0 {
		writeError(w, http.StatusForbidden, "Station binding is required for sensitive operations")
		return
	}
	if err := a.authorize(r.Context(), e, req.Operation, req.Request); err != nil {
		writeSensitiveAuthError(w, err)
		return
	}
	now := a.now()
	ttl := a.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	challenge, err := NewChallenge(e.ClusterID, e.TokenPodUID, e.NodeUID, e.StationUID, req.Operation, string(req.Request), e.Generation, now, ttl)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid challenge request")
		return
	}
	id, err := a.Challenges.Issue(r.Context(), challenge)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "challenge storage unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"challenge_id": id, "expires_at": challenge.ExpiresAt.UTC()})
}

func (a *API) sensitive(w http.ResponseWriter, r *http.Request, token, operation string) {
	if a.Challenges == nil || a.Actions == nil {
		writeError(w, http.StatusServiceUnavailable, "sensitive actions unavailable")
		return
	}
	if !operationPattern.MatchString(operation) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req sensitiveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validJSON(req.Request) || len(req.Request) > 1<<20 || req.ChallengeID == "" {
		writeError(w, http.StatusBadRequest, "invalid sensitive request")
		return
	}
	e, err := a.authenticate(r.Context(), req.ClusterID, token)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if e.StationUID == "" || e.Generation <= 0 {
		writeError(w, http.StatusForbidden, "Station binding is required for sensitive operations")
		return
	}
	if err := a.authorize(r.Context(), e, operation, req.Request); err != nil {
		writeSensitiveAuthError(w, err)
		return
	}
	if err := a.Challenges.Consume(r.Context(), req.ChallengeID, e, operation, string(req.Request), a.now()); err != nil {
		writeError(w, http.StatusForbidden, "challenge rejected")
		return
	}
	result, err := a.Actions.Apply(r.Context(), e, operation, req.Request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sensitive action failed after challenge consumption")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) authorize(ctx context.Context, evidence Evidence, operation string, request json.RawMessage) error {
	if a.Authorizer == nil {
		return ErrSensitiveAuthorizationUnavailable
	}
	if err := a.Authorizer.Authorize(ctx, evidence, operation, request); err != nil {
		if errors.Is(err, ErrSensitiveOperationDenied) {
			return ErrSensitiveOperationDenied
		}
		return ErrSensitiveAuthorizationUnavailable
	}
	return nil
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func validateObservation(o Observation) error {
	if len(o.SystemUUID) > 128 || len(o.Roles) > 32 || len(o.Probes) > 64 || len(o.Wireguard) > 16*1024 {
		return errors.New("observation exceeds limits")
	}
	roles := make(map[string]struct{}, len(o.Roles))
	for _, role := range o.Roles {
		if role == "" || len(role) > 64 {
			return errors.New("invalid observation role")
		}
		if _, exists := roles[role]; exists {
			return errors.New("duplicate observation role")
		}
		roles[role] = struct{}{}
	}
	for _, probe := range o.Probes {
		if probe.Target == "" || len(probe.Target) > 256 || len(probe.Via) > 256 || (probe.RTTMillis != nil && (*probe.RTTMillis < 0 || *probe.RTTMillis > 600000)) {
			return errors.New("invalid probe")
		}
	}
	if len(o.Wireguard) > 0 {
		if err := validateWireguardObservation(o.Wireguard); err != nil {
			return err
		}
	}
	return nil
}

type wireguardObservation struct {
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses,omitempty"`
	Peers     []struct {
		Name            string     `json:"name"`
		LatestHandshake *time.Time `json:"latestHandshake,omitempty"`
		ReceivedBytes   int64      `json:"receivedBytes,omitempty"`
		SentBytes       int64      `json:"sentBytes,omitempty"`
	} `json:"peers,omitempty"`
}

func validateWireguardObservation(raw json.RawMessage) error {
	var observation wireguardObservation
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return errors.New("invalid WireGuard observation")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid WireGuard observation")
	}
	if observation.Interface == "" || len(observation.Interface) > 15 || len(observation.Addresses) > 16 || len(observation.Peers) > 64 {
		return errors.New("invalid WireGuard observation")
	}
	addresses := make(map[string]struct{}, len(observation.Addresses))
	for _, address := range observation.Addresses {
		if _, _, err := net.ParseCIDR(address); err != nil || len(address) > 64 {
			return errors.New("invalid WireGuard address")
		}
		if _, exists := addresses[address]; exists {
			return errors.New("duplicate WireGuard address")
		}
		addresses[address] = struct{}{}
	}
	for _, peer := range observation.Peers {
		if peer.Name == "" || len(peer.Name) > 128 || peer.ReceivedBytes < 0 || peer.SentBytes < 0 {
			return errors.New("invalid WireGuard peer observation")
		}
	}
	return nil
}

func bearer(v string) (string, bool) {
	parts := strings.Split(v, " ")
	returnToken := ""
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		returnToken = parts[1]
	}
	return returnToken, returnToken != ""
}
func validJSON(v json.RawMessage) bool { return len(v) > 0 && json.Valid(v) }
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrIdentityUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "identity verification unavailable")
		return
	}
	writeError(w, http.StatusUnauthorized, "identity rejected")
}
func writeSensitiveAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrSensitiveOperationDenied) {
		writeError(w, http.StatusForbidden, "sensitive operation rejected")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "sensitive authorization unavailable")
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
