package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client sends node-scoped observations over the extension's mTLS endpoint.
// The client certificate carries the node identity; observations never carry
// an identity supplied by the process or workload.
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
}

// NewClient validates an HTTPS endpoint and builds a client that requires the
// server certificate to chain to roots. The caller owns the certificate keys.
func NewClient(endpoint string, certificate tls.Certificate, roots *x509.CertPool) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("agent endpoint must be an HTTPS origin")
	}
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil || roots == nil {
		return nil, errors.New("mTLS certificate and server CA are required")
	}
	if _, err := x509.ParseCertificate(certificate.Certificate[0]); err != nil {
		return nil, fmt.Errorf("parse Agent client certificate: %w", err)
	}
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), HTTPClient: &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots.Clone(), Certificates: []tls.Certificate{certificate}}}}}, nil
}

// Heartbeat posts one bounded observation and returns only a safe status error.
// Callers may repeat it after a timeout; the server updates current status.
func (c *Client) Heartbeat(ctx context.Context, observation Observation) error {
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return errors.New("configured Agent client required")
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("encode Agent observation: %w", err)
	}
	if len(body) > 64<<10 {
		return errors.New("agent observation exceeds request limit")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/heartbeat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Agent heartbeat request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send Agent heartbeat: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	// Discard bounded diagnostics: arbitrary upstream bodies must not enter logs.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("agent heartbeat returned HTTP %d", response.StatusCode)
	}
	return nil
}
