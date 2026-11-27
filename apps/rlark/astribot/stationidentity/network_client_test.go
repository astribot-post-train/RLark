package stationidentity

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStationNetworkClientTLSAndCredentialRotation(t *testing.T) {
	token := strings.Repeat("a", 32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"current","interface":"st-wg0","addresses":["10.254.240.1/32"]}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	caPath, tokenPath := filepath.Join(dir, "ca"), filepath.Join(dir, "token")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := NewNetworkClient(server.URL, caPath, tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.Read(context.Background(), "cluster", "node"); err != nil || got.State != "current" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	token = strings.Repeat("b", 32)
	if _, err := client.Read(context.Background(), "cluster", "node"); !errors.Is(err, ErrNetworkCredentialRejected) {
		t.Fatalf("expected credential rejection, got %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background(), "cluster", "node"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNetworkClient("http://example.com", caPath, tokenPath); err == nil {
		t.Fatal("accepted plaintext Station API")
	}
}
