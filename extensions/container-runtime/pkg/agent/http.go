package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// CertificateRole grants only the extension's node-scoped Agent API. The
// listener must trust a dedicated extension Agent CA, not native admin certs.
const CertificateRole = "containers.rlark.io/node-agent"

// Metadata is signed by the operator's enrollment issuer. It grants no native
// Kubernetes impersonation, proxy, signing, tunnel or SSH permissions.
func (i Identity) Metadata() map[string]string {
	return map[string]string{"cert-role": CertificateRole, "container-namespace": i.Namespace,
		"container-node": i.Name, "container-node-uid": i.UID,
		"container-cluster-uid": i.ClusterUID, "container-instance": i.InstanceID,
		"container-tenant": i.TenantID, "container-credential": i.CredentialID}
}

func requestIdentity(r *http.Request, now time.Time) (Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return Identity{}, ErrIdentity
	}
	leaf := r.TLS.PeerCertificates[0]
	verified := false
	for _, chain := range r.TLS.VerifiedChains {
		if len(chain) > 0 && bytes.Equal(chain[0].Raw, leaf.Raw) {
			verified = true
		}
	}
	if !verified || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return Identity{}, ErrIdentity
	}
	m, ok := cert.GetX509CertMeta(leaf)
	if !ok || m["cert-role"] != CertificateRole {
		return Identity{}, ErrIdentity
	}
	i := Identity{Namespace: m["container-namespace"], Name: m["container-node"], UID: m["container-node-uid"], ClusterUID: m["container-cluster-uid"], InstanceID: m["container-instance"], TenantID: m["container-tenant"], CredentialID: m["container-credential"]}
	for _, value := range i.Metadata() {
		if value == "" {
			return Identity{}, ErrIdentity
		}
	}
	return i, nil
}

// Handler exposes heartbeat and opt-in node-scoped Task transport. Identity comes from a verified
// mTLS peer, not headers or JSON. Construction performs no external effects.
type Handler struct {
	Reporter       Reporter
	EnableTasks    bool
	EnableUpgrades bool
	Logs           *LogBroker
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/logs" && h.EnableTasks && h.Logs != nil {
		h.serveLogs(w, r)
		return
	}
	if r.URL.Path == "/v1/upgrade" && h.EnableUpgrades {
		h.serveUpgrade(w, r)
		return
	}
	if r.URL.Path == "/v1/release" && h.Reporter.ReleaseStore != nil {
		h.serveRelease(w, r)
		return
	}
	if r.URL.Path == "/v1/task" && h.EnableTasks {
		h.serveTask(w, r)
		return
	}
	if r.URL.Path != "/v1/heartbeat" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	id, err := requestIdentity(r, h.Reporter.now())
	if err != nil {
		http.Error(w, "authenticated container Agent required", http.StatusForbidden)
		return
	}
	var body struct {
		Observation
		Busy *bool `json:"busy"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Busy == nil {
		http.Error(w, "invalid Agent observation", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "exactly one Agent observation required", http.StatusBadRequest)
		return
	}
	body.Observation.Busy = *body.Busy
	if err := h.Reporter.Report(r.Context(), id, body.Observation); err != nil {
		status, message := http.StatusServiceUnavailable, "Agent observation unavailable"
		switch {
		case errors.Is(err, ErrIdentity), apierrors.IsNotFound(err):
			status, message = http.StatusForbidden, "container Agent identity is not current"
		case errors.Is(err, ErrCapability):
			status, message = http.StatusBadRequest, "Agent capability is not authorized"
		case apierrors.IsConflict(err):
			status, message = http.StatusConflict, "Agent observation conflicted; retry from current state"
		}
		http.Error(w, message, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) serveRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	id, err := requestIdentity(r, h.Reporter.now())
	if err != nil {
		http.Error(w, "authenticated container Agent required", http.StatusForbidden)
		return
	}
	var request ReleaseRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid release request", http.StatusBadRequest)
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "exactly one release request required", http.StatusBadRequest)
		return
	}
	manifest, data, err := h.Reporter.Release(r.Context(), id, request, h.Reporter.ReleaseStore)
	if err != nil {
		status, message := http.StatusServiceUnavailable, "Agent release unavailable"
		switch {
		case errors.Is(err, ErrIdentity), errors.Is(err, ErrReleaseIdentity):
			status, message = http.StatusForbidden, "container Agent release identity is not current"
		case errors.Is(err, ErrNoRelease):
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(err, artifact.ErrInvalid), errors.Is(err, artifact.ErrDigest):
			status, message = http.StatusBadRequest, "Agent release artifact is invalid"
		case apierrors.IsNotFound(err), errors.Is(err, ErrReleaseUnavailable):
			status, message = http.StatusNotFound, "target Agent release is unavailable"
		}
		http.Error(w, message, status)
		return
	}
	if request.Action == "manifest" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(manifest); err != nil {
			return
		}
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
	_, _ = w.Write(data)
}
