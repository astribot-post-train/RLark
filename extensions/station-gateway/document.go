package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// stationAPI is station-api.json, the file beside the service's own
// configuration that says where this node asks for its document: Station
// API's address, and the file beside it that holds the CA of its certificate.
// The installer lays both on a new machine, the keeper on every machine its
// pod runs on.
type stationAPI struct {
	URL string `json:"url"`
	CA  string `json:"ca"`
}

const stationAPIFile = "station-api.json"

// documentProofLabel begins what the node's proof is an HMAC of
// (apps/rlark/pkg/stationidentity, NetworkDocuments).
const documentProofLabel = "rlark station network document v1"

// ownDocument asks Station API for this node's document with the node's own
// identity, its WireGuard key, and stores it where the agent container
// stores the same document, when it is another than the one there. The
// agent's pod needs the kubelet, and the kubelet the control plane's name,
// which is in the document: a node whose pod was not running when its
// control plane moved would otherwise look for it where it was, for good.
// It returns what there is to say of the round; nothing without the file.
func ownDocument(directory string, key wgtypes.Key, computed string) string {
	data, err := os.ReadFile(filepath.Join(directory, stationAPIFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	var api stationAPI
	if err == nil {
		err = json.Unmarshal(data, &api)
	}
	var ca []byte
	if err == nil {
		ca, err = os.ReadFile(filepath.Join(directory, api.CA))
	}
	if err != nil {
		return fmt.Sprintf("%s is not usable: %v", stationAPIFile, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	private, _ := ecdh.X25519().NewPrivateKey(key[:])
	request := map[string]string{"public_key": key.PublicKey().String()}
	// The first answer is the challenge, the second the document.
	for range 2 {
		body, _ := json.Marshal(request)
		response, err := client.Post(strings.TrimRight(api.URL, "/")+"/v1/network/document", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Sprintf("Station API is not reached: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		var answer struct {
			Challenge  string          `json:"challenge"`
			ServerKey  string          `json:"server_key"`
			Config     json.RawMessage `json:"config"`
			ConfigHash string          `json:"config_hash"`
		}
		if err == nil && response.StatusCode != http.StatusNotFound {
			err = json.Unmarshal(data, &answer)
		}
		switch {
		case err != nil:
			return fmt.Sprintf("Station API at %s answered %d and what cannot be read: %v", api.URL, response.StatusCode, err)
		case response.StatusCode == http.StatusNotFound:
			return "Station API at " + api.URL + " has no document for this node's key; the node goes on with the one it has"
		case response.StatusCode == http.StatusOK:
			if sum := sha256.Sum256(answer.Config); hex.EncodeToString(sum[:]) != answer.ConfigHash {
				return "the document Station API at " + api.URL + " gave does not match its hash"
			}
			if had, _ := os.ReadFile(computed); !bytes.Equal(had, answer.Config) {
				if err := replaceFile(computed, answer.Config, 0644); err != nil {
					return fmt.Sprintf("store the document: %v", err)
				}
				return "took another document from Station API at " + api.URL + ": " + answer.ConfigHash
			}
			return "has the document of Station API at " + api.URL + ": " + answer.ConfigHash
		case response.StatusCode != http.StatusUnauthorized:
			return fmt.Sprintf("Station API at %s answered %d", api.URL, response.StatusCode)
		}
		challenge, _ := base64.StdEncoding.DecodeString(answer.Challenge)
		serverKey, _ := base64.StdEncoding.DecodeString(answer.ServerKey)
		public, err := ecdh.X25519().NewPublicKey(serverKey)
		if err != nil {
			return fmt.Sprintf("Station API at %s gave no key to prove this node's to: %v", api.URL, err)
		}
		shared, err := private.ECDH(public)
		if err != nil {
			return fmt.Sprintf("Station API at %s gave no key to prove this node's to: %v", api.URL, err)
		}
		mac := hmac.New(sha256.New, shared)
		mac.Write([]byte(documentProofLabel))
		mac.Write(challenge)
		request["challenge"], request["proof"] = answer.Challenge, base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	return "Station API at " + api.URL + " did not take the proof of this node's key"
}
