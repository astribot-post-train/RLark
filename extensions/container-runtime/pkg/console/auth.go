package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Authenticator validates browser identity independently of management RBAC.
type Authenticator interface {
	Administrator(*http.Request) (string, error)
}

var (
	errUnauthorized = errors.New("authentication required")
	errForbidden    = errors.New("administrator access required")
)

type gatewayJWT struct{ key []byte }
type claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// NewGatewayJWT copies the trusted Gateway's signing key. Provision the key out
// of band; the console does not read Kubernetes Secrets or issue browser tokens.
func NewGatewayJWT(key []byte) (Authenticator, error) {
	if len(key) < 32 {
		return nil, errors.New("gateway JWT key must contain at least 32 bytes")
	}
	return gatewayJWT{key: append([]byte(nil), key...)}, nil
}
func (a gatewayJWT) Administrator(r *http.Request) (string, error) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 16384 {
		return "", errUnauthorized
	}
	c := &claims{}
	token, err := jwt.ParseWithClaims(fields[1], c, func(*jwt.Token) (any, error) { return a.key, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("rlark-gateway"), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || strings.TrimSpace(c.Subject) == "" {
		return "", errUnauthorized
	}
	if c.Role != "admin" {
		return "", errForbidden
	}
	return c.Subject, nil
}
