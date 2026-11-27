package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The same numbers stand in apps/rlark/astribot/stationidentity
// (TestNetworkDocumentProofIsTheGatewaysProof): the two modules share no
// code, and the proof this program computes must be one Station API takes.
const (
	vectorNodeKey   = "ERERERERERERERERERERERERERERERERERERERERERE="
	vectorChallenge = "AAAAAGtJ0gABAgMEBQYHCAkKCwwNDg8Q"
	vectorServerKey = "gUE5D2aX/M/z3wvCtPds4qmBUkN1tN4AHT3f576yRQI="
	vectorProof     = "Luhm9kDL1+eJcvRKcPOXv3rnD4va9kmRfHmfozQWXX4="
)

// documentServer is Station API as this program meets it: one key per
// process instead of one per challenge, which a node cannot tell apart.
type documentServer struct {
	t          *testing.T
	key        *ecdh.PrivateKey
	document   string // none: 404
	requests   int
	links      []string // the link records taken
	linkAnswer int      // to a proven link record; none: 204
	*httptest.Server
}

func newDocumentServer(t *testing.T, directory, document string) *documentServer {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &documentServer{t: t, key: key, document: document}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	pointer, _ := json.Marshal(stationAPI{URL: s.URL + "/", CA: "ca.crt"})
	for name, content := range map[string][]byte{"ca.crt": ca, stationAPIFile: pointer} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *documentServer) serve(w http.ResponseWriter, r *http.Request) {
	s.requests++
	var request struct {
		PublicKey string          `json:"public_key"`
		Challenge string          `json:"challenge"`
		Proof     string          `json:"proof"`
		Link      json.RawMessage `json:"link"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || r.URL.Path != "/v1/network/document" && r.URL.Path != "/v1/network/link" || r.Header.Get("Content-Type") != "application/json" {
		s.t.Errorf("request %s %s: %v", r.Method, r.URL.Path, err)
	}
	raw, _ := base64.StdEncoding.DecodeString(request.PublicKey)
	node, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		s.t.Error(err)
	}
	shared, _ := s.key.ECDH(node)
	challenge, _ := base64.StdEncoding.DecodeString(vectorChallenge)
	mac := hmac.New(sha256.New, shared)
	mac.Write([]byte("rlark station network document v1"))
	mac.Write(challenge)
	w.Header().Set("Content-Type", "application/json")
	switch proof, _ := base64.StdEncoding.DecodeString(request.Proof); {
	case request.Challenge != vectorChallenge || !hmac.Equal(proof, mac.Sum(nil)):
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"challenge": vectorChallenge, "server_key": base64.StdEncoding.EncodeToString(s.key.PublicKey().Bytes())})
	case r.URL.Path == "/v1/network/link" && s.linkAnswer != 0:
		w.WriteHeader(s.linkAnswer)
	case r.URL.Path == "/v1/network/link":
		s.links = append(s.links, string(request.Link))
		w.WriteHeader(http.StatusNoContent)
	case s.document == "":
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no document for this key"})
	default:
		sum := sha256.Sum256([]byte(s.document))
		json.NewEncoder(w).Encode(map[string]any{"config": json.RawMessage(s.document), "config_hash": hex.EncodeToString(sum[:])})
	}
}

func TestOwnDocumentIsStoredWhenItIsAnother(t *testing.T) {
	directory := t.TempDir()
	computed := filepath.Join(directory, "computed.json")
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	// Without the file the program asks no one, as before there was one.
	if state := ownDocument(directory, key, computed); state != "" {
		t.Fatalf("without %s: %q", stationAPIFile, state)
	}
	server := newDocumentServer(t, directory, `{"interface":"st-wg0","names":[{"name":"cp.farm.rlark.internal","address":"10.254.240.12"}]}`)
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, "took another document") || server.requests != 2 {
		t.Fatalf("first round: %q after %d requests", state, server.requests)
	}
	stored, err := os.ReadFile(computed)
	if err != nil || string(stored) != server.document {
		t.Fatalf("stored %q, %v", stored, err)
	}
	// The same document again is not written again: the file is the same file.
	before, _ := os.Stat(computed)
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, "has the document") {
		t.Fatalf("second round: %q", state)
	}
	if after, _ := os.Stat(computed); !os.SameFile(before, after) {
		t.Fatal("the same document was written again")
	}
	// What the agent container wrote meanwhile is replaced like any other.
	server.document = `{"interface":"st-wg0","names":[{"name":"cp.farm.rlark.internal","address":"10.254.240.13"}]}`
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, "took another document") {
		t.Fatalf("after the document changed: %q", state)
	}
	if stored, _ := os.ReadFile(computed); string(stored) != server.document {
		t.Fatalf("stored %q", stored)
	}

	// A key Station API has no document for: the node keeps the one it has.
	server.document = ""
	if state := ownDocument(directory, key, computed); !strings.Contains(state, "has no document for this node's key") {
		t.Fatalf("no document: %q", state)
	}
	// A server whose certificate is not of the CA, Station API away, a file
	// that names no CA: said, and no more.
	ca, _ := os.ReadFile(filepath.Join(directory, "ca.crt"))
	os.WriteFile(filepath.Join(directory, "ca.crt"), []byte("no certificate\n"), 0644)
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, "Station API is not reached") || !strings.Contains(state, "certificate") {
		t.Fatalf("a server of another CA: %q", state)
	}
	os.WriteFile(filepath.Join(directory, "ca.crt"), ca, 0644)
	server.Close()
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, "Station API is not reached") {
		t.Fatalf("server away: %q", state)
	}
	os.WriteFile(filepath.Join(directory, stationAPIFile), []byte(`{"url":"https://127.0.0.1:1","ca":"missing.crt"}`), 0644)
	if state := ownDocument(directory, key, computed); !strings.HasPrefix(state, stationAPIFile+" is not usable") {
		t.Fatalf("no CA: %q", state)
	}
	if stored, _ := os.ReadFile(computed); !bytes.Contains(stored, []byte("10.254.240.13")) {
		t.Fatalf("the document the node had is gone: %q", stored)
	}
}

func TestOwnDocumentProof(t *testing.T) {
	directory := t.TempDir()
	server := newDocumentServer(t, directory, "")
	var proof string
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Proof string `json:"proof"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		proof = request.Proof
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"challenge": vectorChallenge, "server_key": vectorServerKey})
	})
	key, _ := wgtypes.ParseKey(vectorNodeKey)
	if state := ownDocument(directory, key, filepath.Join(directory, "computed.json")); !strings.HasSuffix(state, "did not take the proof of this node's key") {
		t.Fatal(state)
	}
	if proof != vectorProof {
		t.Fatalf("proof %s", proof)
	}
}

// The record goes to Station API without when Station API last answered, and
// only when it is another than the one RLark took.
func TestReportLinkSendsTheRecordWhenItChanged(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "link.json")
	key, _ := wgtypes.GeneratePrivateKey()
	server := newDocumentServer(t, directory, "")
	if sent, state := reportLink(directory, key, path, nil); sent != nil || state != "" || server.requests != 0 {
		t.Fatalf("without link.json: %q %q after %d requests", sent, state, server.requests)
	}
	write := func(record link) {
		data, _ := json.Marshal(record)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(link{State: "up", RLark: 1_800_000_100, Since: 1_800_000_000, Actions: []healAction{{At: 1_800_000_000, Step: "recovered", Result: "r"}}})
	sent, state := reportLink(directory, key, path, nil)
	want := `{"state":"up","since":1800000000,"restarts":0,"actions":[{"at":1800000000,"step":"recovered","result":"r"}]}`
	if string(sent) != want || len(server.links) != 1 || server.links[0] != want || !strings.Contains(state, "has the link record") {
		t.Fatalf("first report: %s %q %q", sent, state, server.links)
	}
	// Station API answered again: the record is the same and nothing is sent.
	write(link{State: "up", RLark: 1_800_000_130, Since: 1_800_000_000, Actions: []healAction{{At: 1_800_000_000, Step: "recovered", Result: "r"}}})
	requests := server.requests
	if again, state := reportLink(directory, key, path, sent); string(again) != want || state != "" || server.requests != requests {
		t.Fatalf("the same record: %s %q after %d requests", again, state, server.requests-requests)
	}
	// A record Station API does not take stays to be sent.
	write(link{State: "down", Since: 1_800_000_200})
	server.linkAnswer = http.StatusNotFound
	if kept, state := reportLink(directory, key, path, sent); string(kept) != want || !strings.HasSuffix(state, "answered 404 to the link record") {
		t.Fatalf("not taken: %s %q", kept, state)
	}
}
