package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoginForwardsOnlyTypedCredentials(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/auth/login", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-Role"))
		var credentials map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&credentials))
		require.Equal(t, map[string]string{"username": "admin", "password": "only-test"}, credentials)
		w.Header().Set("Set-Cookie", "foreign-cookie=not-forwarded")
		writeJSON(w, 200, map[string]any{"ok": true, "role": "admin", "token": adminToken(t), "expiresAt": "future", "private": "never forward"})
	}))
	defer gateway.Close()
	login, err := NewLogin(gateway.URL, auth)
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"only-test"}`))
	r.Header.Set("Authorization", "must-not-forward")
	r.Header.Set("Cookie", "must-not-forward")
	r.Header.Set("X-Role", "admin")
	w := httptest.NewRecorder()
	login.ServeHTTP(w, r)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "private")
	require.Empty(t, w.Header().Get("Set-Cookie"))
}
func TestLoginRejectsUntrustedOriginsAndRedirects(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	for _, origin := range []string{"http://cloud.example", "https://user:password@cloud.example", "https://cloud.example/api", "https://cloud.example?q=a", "file:///private"} {
		_, err := NewLogin(origin, auth)
		require.Error(t, err)
	}
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unexpected" {
			reached = true
		}
		http.Redirect(w, r, "/unexpected", http.StatusFound)
	}))
	defer server.Close()
	login, err := NewLogin(server.URL, auth)
	require.NoError(t, err)
	w := request(t, login, "POST", "/api/v1/auth/login", `{"username":"admin","password":"test"}`)
	require.Equal(t, 503, w.Code)
	require.False(t, reached)
}
