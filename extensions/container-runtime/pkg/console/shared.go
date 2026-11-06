// Package console serves only the private diagnostic listener in the Agent API process.
package console

import (
	"encoding/json"
	management "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/console"
	"net/http"
)

type Authenticator = management.Authenticator

var NewGatewayJWT = management.NewGatewayJWT
var NewLogProxy = management.NewLogProxy
var NewStatsProxy = management.NewStatsProxy
var errUnauthorized = management.ErrUnauthorized
var errForbidden = management.ErrForbidden

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "response unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message})
}
