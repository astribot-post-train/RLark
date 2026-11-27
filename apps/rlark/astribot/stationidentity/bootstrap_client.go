package stationidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BootstrapClient reads installer artifacts through a verified Station API connection.
type BootstrapClient struct {
	address, tunnel, origin string
	http                    *http.Client
}

// NewBootstrapClient configures the machine-facing URL and the trusted origin.
func NewBootstrapClient(bootstrapURL, origin, caFile string) (*BootstrapClient, error) {
	machines, err := url.Parse(bootstrapURL)
	if err != nil || machines.Scheme != "https" || machines.Port() == "" || machines.User != nil || (machines.Path != "" && machines.Path != "/") || machines.RawQuery != "" {
		return nil, errors.New("the Station API bootstrap URL must be https://host:port")
	}
	address := machines.Host
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the Station API HTTPS origin is required to give out join codes")
	}
	client, err := newStationHTTPClient(caFile)
	if err != nil {
		return nil, err
	}
	return &BootstrapClient{address: address, tunnel: u.Host, origin: strings.TrimRight(origin, "/"), http: client}, nil
}

// Address is the host and port advertised to a joining machine.
func (s *BootstrapClient) Address() string { return s.address }

// TunnelAddress is the Station API host and port on the site network.
func (s *BootstrapClient) TunnelAddress() string { return s.tunnel }

// get GETs path from Station API within timeout (0: until ctx ends), and answers its body when Station API answers 200.
func (s *BootstrapClient) get(ctx context.Context, path string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+path, nil)
	if err != nil {
		return nil, err
	}
	client := *s.http
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		// The path may hold a code: it stays out of the error.
		return nil, fmt.Errorf("station API answered %d", resp.StatusCode)
	}
	return resp, nil
}

// Digest returns the hexadecimal SHA-256 of an installer artifact.
func (s *BootstrapClient) Digest(ctx context.Context, path string) (string, error) {
	resp, err := s.get(ctx, path, s.http.Timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Open has no time limit of the client's: the installer is hundreds of MB; ctx ends the stream.
// Open streams a bootstrap artifact and returns its content length.
func (s *BootstrapClient) Open(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	resp, err := s.get(ctx, path, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// BundleDigest waits up to 2 minutes: Station API reads the whole installer (about 2 GB) for its first answer after
// the installer changes.
func (s *BootstrapClient) BundleDigest(ctx context.Context) (string, error) {
	resp, err := s.get(ctx, "/v1/bootstrap/bundle.sha256", 2*time.Minute)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
