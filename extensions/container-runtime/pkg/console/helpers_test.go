package console

import (
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type diagnosticClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

var testKey = []byte("only-a-test-key-with-at-least-32-bytes")

func token(t *testing.T, c diagnosticClaims) string {
	t.Helper()
	v, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(testKey)
	require.NoError(t, err)
	return v
}
func adminToken(t *testing.T) string {
	return token(t, diagnosticClaims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
}
func request(t *testing.T, a http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken(t))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
