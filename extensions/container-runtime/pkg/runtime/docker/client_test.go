package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSDKClientNegotiatesModernAndOlderDaemonAPIs(t *testing.T) {
	for _, version := range []string{"1.44", "1.42"} {
		t.Run(version, func(t *testing.T) {
			var called bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", version)
					return
				}
				if r.URL.Path != "/v"+version+"/info" {
					t.Errorf("unexpected path %s", r.URL.Path)
					http.Error(w, "API version rejected", 400)
					return
				}
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"ID":"engine-1","NCPU":8}`)
			}))
			defer server.Close()
			c, err := NewSDKClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			info, err := c.Info(context.Background())
			if err != nil || !called || info.ID != "engine-1" {
				t.Fatalf("info=%#v called=%v err=%v", info, called, err)
			}
		})
	}
}
