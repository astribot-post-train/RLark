package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
)

func manifestSpec(m *ReleaseManifest) (ext.AgentReleaseSpec, error) {
	if m == nil || m.Namespace == "" || m.Name == "" || m.UID == "" || m.Bytes <= 0 || m.Bytes > artifact.MaxBytes {
		return ext.AgentReleaseSpec{}, ErrReleaseIdentity
	}
	spec := ext.AgentReleaseSpec{Version: m.Version, OS: m.OS, Architecture: m.Architecture, Protocol: m.Protocol, SHA256: m.SHA256, ArtifactPath: "agent"}
	return spec, artifact.ValidateSpec(spec)
}

func (c *Client) releaseRequest(ctx context.Context, request ReleaseRequest) (*http.Response, error) {
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return nil, errors.New("configured Agent client required")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode release request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/release", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send release request: %w", err)
	}
	return response, nil
}

// TargetRelease reads the authenticated node's current target. Nil means its
// Cluster has no target. No artifact is installed or executed by this method.
func (c *Client) TargetRelease(ctx context.Context, osName, architecture string) (*ReleaseManifest, error) {
	response, err := c.releaseRequest(ctx, ReleaseRequest{Action: "manifest", OS: osName, Architecture: architecture})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }() // Read/status errors remain authoritative.
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent release manifest returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil {
		return nil, fmt.Errorf("read release manifest: %w", err)
	}
	if len(data) > 16<<10 {
		return nil, errors.New("agent release manifest exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest ReleaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("one Agent release manifest required")
	}
	if _, err := manifestSpec(&manifest); err != nil {
		return nil, err
	}
	if manifest.OS != osName || manifest.Architecture != architecture {
		return nil, ErrReleaseIdentity
	}
	return &manifest, nil
}

// DownloadRelease fetches only the exact manifest UID/digest and independently
// verifies byte count, SHA-256 and executable platform. Callers must obtain an
// idle-node upgrade lock before installing or switching the returned bytes.
func (c *Client) DownloadRelease(ctx context.Context, manifest *ReleaseManifest) ([]byte, error) {
	spec, err := manifestSpec(manifest)
	if err != nil {
		return nil, err
	}
	response, err := c.releaseRequest(ctx, ReleaseRequest{Action: "download", OS: manifest.OS, Architecture: manifest.Architecture, ReleaseUID: manifest.UID, SHA256: manifest.SHA256})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }() // Read/status errors remain authoritative.
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent artifact download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != manifest.Bytes {
		return nil, errors.New("agent artifact content length mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, manifest.Bytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Agent artifact: %w", err)
	}
	if int64(len(data)) != manifest.Bytes {
		return nil, errors.New("agent artifact byte count mismatch")
	}
	if err := artifact.Verify(spec, data); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
