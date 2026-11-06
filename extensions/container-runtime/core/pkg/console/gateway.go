package console

import (
	"context"
	"net/http"
)

type gatewayAdministratorKey struct{}

// WithGatewayAdministrator is the trusted embedding boundary. Call it only
// after authenticating a request and verifying the administrator role.
func WithGatewayAdministrator(r *http.Request, subject string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), gatewayAdministratorKey{}, subject))
}

// GatewayIdentity accepts only the in-process identity established by Gateway.
// It never treats client headers or cookies as authorization evidence.
type GatewayIdentity struct{}

func (GatewayIdentity) Administrator(r *http.Request) (string, error) {
	if subject, ok := r.Context().Value(gatewayAdministratorKey{}).(string); ok && subject != "" {
		return subject, nil
	}
	return "", ErrUnauthorized
}
