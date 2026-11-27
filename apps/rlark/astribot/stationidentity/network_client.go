package stationidentity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// newStationHTTPClient reaches Station API verified by the CA of caFile, and follows no redirect.
func newStationHTTPClient(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("read Station API CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid Station API CA")
	}
	return &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}, nil
}

// NetworkClient reads node network observations from Station API.
type NetworkClient struct {
	endpoint, credentialFile string
	http                     *http.Client
}

// ErrNetworkCredentialRejected indicates that Station API rejected the credential.
var ErrNetworkCredentialRejected = errors.New("Station network service credential rejected")

// NewNetworkClient creates a Station API client that rereads its credential for each request.
func NewNetworkClient(address, caFile, credentialFile string) (*NetworkClient, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || credentialFile == "" {
		return nil, errors.New("Station API HTTPS origin and credential file are required")
	}
	client, err := newStationHTTPClient(caFile)
	if err != nil {
		return nil, err
	}
	return &NetworkClient{endpoint: strings.TrimRight(address, "/") + "/v1/internal/node-network", credentialFile: credentialFile, http: client}, nil
}

// Read fetches one node’s network observation.
func (s *NetworkClient) Read(ctx context.Context, cluster, node string) (NodeNetworkSummary, error) {
	var result NodeNetworkSummary
	f, err := os.Open(s.credentialFile)
	if err != nil {
		return result, err
	}
	credential, err := io.ReadAll(io.LimitReader(f, 4097))
	_ = f.Close()
	if err != nil {
		return result, err
	}
	token := strings.TrimSpace(string(credential))
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return result, errors.New("invalid Station credential")
	}
	body, err := json.Marshal(map[string]string{"cluster_id": cluster, "node_name": node})
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return result, ErrNetworkCredentialRejected
	}
	if resp.StatusCode != http.StatusOK {
		return result, errors.New("Station network query failed")
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16385))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || (result.State != "current" && result.State != "stale" && result.State != "unreported") {
		return result, errors.New("invalid Station network response")
	}
	return result, nil
}
