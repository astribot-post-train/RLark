package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func (h Handler) serveTask(w http.ResponseWriter, r *http.Request) {
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
	var req TaskRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid scoped Task request", http.StatusBadRequest)
		return
	}
	result, err := (TaskService{Reporter: h.Reporter}).Handle(r.Context(), id, req)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrIdentity), errors.Is(err, companion.ErrIdentity), apierrors.IsNotFound(err):
			status = http.StatusForbidden
		case errors.Is(err, ErrCapability):
			status = http.StatusBadRequest
		case apierrors.IsConflict(err):
			status = http.StatusConflict
		}
		// Never serialize management errors, workload configuration or credentials.
		http.Error(w, "scoped Task operation unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
