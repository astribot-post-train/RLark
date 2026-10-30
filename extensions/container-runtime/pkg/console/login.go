package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Login proxies only the typed login operation to a configured trusted Gateway.
// It never forwards browser headers, cookies, arbitrary paths, or redirects.
type Login struct {
	Endpoint string
	Client   *http.Client
	Auth     Authenticator
}

// NewLogin accepts an HTTPS Gateway origin or a loopback HTTP development origin.
func NewLogin(origin string, auth Authenticator) (*Login, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("trusted Gateway origin required")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("gateway requires HTTPS or loopback HTTP")
	}
	if auth == nil {
		return nil, errors.New("gateway authenticator required")
	}
	u.Path = "/api/v1/auth/login"
	return &Login{Endpoint: u.String(), Auth: auth, Client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *Login) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Request-ID", uuid.NewString())
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST required")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if origin != scheme+"://"+r.Host {
			writeError(w, 403, "forbidden", "same-origin request required")
			return
		}
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeStrict(w, r, &credentials) {
		return
	}
	if credentials.Username == "" || credentials.Password == "" {
		writeError(w, 400, "invalid_request", "credentials required")
		return
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		writeError(w, 400, "invalid_request", "invalid credentials")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.Endpoint, bytes.NewReader(data))
	if err != nil {
		writeError(w, 503, "unavailable", "login unavailable")
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.Client.Do(request)
	if err != nil {
		writeError(w, 503, "unavailable", "login unavailable")
		return
	}
	defer func() { _ = response.Body.Close() }() // Read-only response cleanup.
	if response.StatusCode != 200 {
		status := 503
		if response.StatusCode == 401 || response.StatusCode == 403 {
			status = 401
		}
		writeError(w, status, "login_failed", "login failed")
		return
	}
	var result struct {
		OK        bool   `json:"ok"`
		Role      string `json:"role"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if err != nil || json.Unmarshal(body, &result) != nil || !result.OK {
		writeError(w, 503, "unavailable", "login unavailable")
		return
	}
	verify := r.Clone(r.Context())
	verify.Header = make(http.Header)
	verify.Header.Set("Authorization", "Bearer "+result.Token)
	if _, err := p.Auth.Administrator(verify); err != nil {
		writeError(w, 403, "forbidden", "administrator access required")
		return
	}
	writeJSON(w, 200, result)
}
