package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

const NodeImageBrokerPath = "/internal/upgrade/node-image"

type nodeImageRequest struct {
	Cluster  string           `json:"cluster"`
	Executor ExecutorIdentity `json:"executor"`
	Action   NodeImageAction  `json:"action"`
}

// ServeHTTP is intended for an internal HTTPS listener. The only credential
// accepted here is the audience-restricted token of the verified member Pod.
func (b NodeImageBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != NodeImageBrokerPath || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	token, hasToken := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.TLS == nil || !hasToken || token == "" {
		http.Error(w, "upgrade executor authentication required", http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	var request nodeImageRequest
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF ||
		len(validation.IsDNS1123Subdomain(request.Cluster)) != 0 ||
		len(validation.IsDNS1123Subdomain(request.Executor.PodName)) != 0 ||
		len(validation.IsDNS1123Label(request.Executor.ChildUID)) != 0 ||
		(request.Executor.ChildName != "" && len(validation.IsDNS1123Subdomain(request.Executor.ChildName)) != 0) ||
		len(validation.IsDNS1123Label(request.Executor.PodUID)) != 0 {
		http.Error(w, "invalid node image operation", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	result, err := b.Execute(ctx, request.Cluster, request.Executor, token, request.Action)
	if err != nil {
		var conflict *LeaseConflict
		switch {
		case errors.Is(err, ErrBatchAuthority):
			http.Error(w, "member upgrade authority could not be verified", http.StatusPreconditionFailed)
		case errors.Is(err, ErrManagedBaseline):
			http.Error(w, "node Agent baseline has not converged", http.StatusUnprocessableEntity)
		case errors.Is(err, ErrUnverifiedExecutor):
			http.Error(w, "upgrade executor authentication failed", http.StatusUnauthorized)
		case errors.Is(err, ErrNodeImageGrant), apierrors.IsNotFound(err):
			http.Error(w, "no active node image grant", http.StatusForbidden)
		case errors.As(err, &conflict), errors.Is(err, nodeagent.ErrImageDeclarationChanged), apierrors.IsConflict(err):
			http.Error(w, "node image operation conflicted", http.StatusConflict)
		default:
			http.Error(w, "node image operation unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
