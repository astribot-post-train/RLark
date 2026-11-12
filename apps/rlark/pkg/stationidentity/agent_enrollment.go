package stationidentity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Enroll registers the node's WireGuard public key under the agent's verified
// identity and stores the configuration station-api computed for it where the
// gateway container reads it. A pending or failed enrollment leaves the last
// stored configuration in place: the tunnel never depends on station-api
// being reachable.
func (a *Agent) Enroll(ctx context.Context) (string, error) {
	tokenBytes, err := a.read(a.config.TokenFile)
	if err != nil {
		return "", errors.New("read projected Station token")
	}
	identity, err := a.read(a.config.NetworkIdentityFile)
	if err != nil {
		return "", errors.New("the gateway has not published its public key")
	}
	var published struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(identity, &published); err != nil || published.PublicKey == "" {
		return "", errors.New("gateway identity file is invalid")
	}
	request := enrollmentRequest{ClusterID: a.config.ClusterID, PublicKey: published.PublicKey}
	if address := a.localAddress(); address != "" {
		request.Addresses = []string{address}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestLimit)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(a.config.APIURL, "/")+"/v1/network/enrollments", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("build Station API request")
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send network enrollment: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", errors.New("read network enrollment answer")
	}
	if response.StatusCode == http.StatusConflict {
		return "pending", nil
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Station API rejected network enrollment with HTTP %d", response.StatusCode)
	}
	var answer NetworkEnrollmentAnswer
	if err := json.Unmarshal(data, &answer); err != nil {
		return "", errors.New("decode network enrollment answer")
	}
	if answer.State != "enrolled" || len(answer.Config) == 0 {
		return answer.State, nil
	}
	if digest := sha256.Sum256(answer.Config); hex.EncodeToString(digest[:]) != answer.ConfigHash {
		return "", errors.New("network configuration does not match its hash")
	}
	if current, err := os.ReadFile(a.config.NetworkConfigFile); err == nil && bytes.Equal(current, answer.Config) {
		return answer.State, nil
	}
	file, err := os.CreateTemp(filepath.Dir(a.config.NetworkConfigFile), ".station-network-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0644); err == nil {
		_, err = file.Write(answer.Config)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return answer.State, os.Rename(file.Name(), a.config.NetworkConfigFile)
}

// localAddress is the source address the node uses towards station-api. A UDP
// connect selects it from the routing table without sending a packet.
func (a *Agent) localAddress() string {
	parsed, err := url.Parse(a.config.APIURL)
	if err != nil {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	connection, err := net.Dial("udp4", net.JoinHostPort(parsed.Hostname(), port))
	if err != nil {
		return ""
	}
	defer connection.Close()
	return connection.LocalAddr().(*net.UDPAddr).IP.String()
}
