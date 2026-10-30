package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const upgradeBodyLimit = 16 << 10

func (h Handler) serveUpgrade(w http.ResponseWriter, r *http.Request) {
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
	var req UpgradeRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, upgradeBodyLimit))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid scoped upgrade request", http.StatusBadRequest)
		return
	}
	result, err := h.Reporter.Upgrade(r.Context(), id, req)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrCapability):
			status = http.StatusBadRequest
		case errors.Is(err, ErrIdentity), errors.Is(err, ErrUpgradeIdentity), errors.Is(err, companion.ErrIdentity), apierrors.IsNotFound(err):
			status = http.StatusForbidden
		case errors.Is(err, ErrUpgradeConflict), apierrors.IsConflict(err):
			status = http.StatusConflict
		}
		http.Error(w, "scoped upgrade operation unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// Upgrade calls only the authenticated node's upgrade operation endpoint.
// Upstream error bodies never enter returned errors; retries retain attempt IDs.
func (c *Client) Upgrade(ctx context.Context, req UpgradeRequest) (*UpgradeResponse, error) {
	if err := validateUpgradeRequest(req); err != nil {
		return nil, err
	}
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return nil, errors.New("configured Agent client required")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(data) > upgradeBodyLimit {
		return nil, ErrCapability
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/upgrade", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send scoped upgrade request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		var cause error
		switch response.StatusCode {
		case http.StatusForbidden:
			cause = ErrUpgradeIdentity
		case http.StatusConflict:
			cause = ErrUpgradeConflict
		default:
			cause = ErrUpgradeUnavailable
		}
		return nil, fmt.Errorf("scoped upgrade returned HTTP %d: %w", response.StatusCode, cause)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, upgradeBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read scoped upgrade response: %w", err)
	}
	if len(body) > upgradeBodyLimit {
		return nil, ErrCapability
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var result *UpgradeResponse
	if err := d.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode scoped upgrade response: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, ErrCapability
	}
	if err := validateUpgradeResponse(req, result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateUpgradeResponse(req UpgradeRequest, result *UpgradeResponse) error {
	if result == nil {
		if req.Action == "assignment" {
			return nil
		}
		return ErrUpgradeIdentity
	}
	if result.Operation.Namespace == "" || result.Operation.Name == "" || result.Operation.UID == "" || result.ResourceVersion == "" {
		return ErrUpgradeIdentity
	}
	if req.Action != "assignment" && req.Operation != result.Operation {
		return ErrUpgradeIdentity
	}
	if req.Allocation.UID != "" && result.Allocation != nil && req.Allocation != *result.Allocation {
		return ErrUpgradeIdentity
	}
	if result.Granted && (req.Action != "claim" || result.Phase != "Applying" || result.Allocation == nil || req.Allocation != *result.Allocation || result.AttemptID != req.AttemptID || result.StartedAt == nil || result.StartedAt.IsZero() || result.Completion != nil) {
		return ErrUpgradeIdentity
	}
	switch result.Phase {
	case "Pending", "Reserved", "Applying", "Succeeded", "RolledBack", "Cancelled", "Failed":
	default:
		return ErrCapability
	}
	release := result.Release
	_, err := manifestSpec(&ReleaseManifest{Namespace: release.Namespace, Name: release.Name, UID: release.UID, Version: release.Version, OS: release.OS, Architecture: release.Architecture, Protocol: release.Protocol, SHA256: release.SHA256, Bytes: 1})
	if err != nil {
		return err
	}
	if result.Release.Namespace != result.Operation.Namespace || result.FromVersion == "" {
		return ErrUpgradeIdentity
	}
	if result.Allocation != nil && (result.Allocation.Namespace != result.Operation.Namespace || result.Allocation.Name == "" || result.Allocation.UID == "") {
		return ErrUpgradeIdentity
	}
	if req.Action == "claim" && !result.Granted {
		return ErrUpgradeUnavailable
	}
	if req.Action == "complete" && (result.Completion == nil || result.Completion.AttemptID != req.AttemptID || result.Completion.Result != req.Result || result.Completion.Version != req.Version) {
		return ErrUpgradeIdentity
	}
	return nil
}
