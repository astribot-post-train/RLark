package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/stretchr/testify/require"
)

const logQuery = "?namespace=agents&allocation=lock&allocationUID=exact-allocation&taskUID=exact-task"

type logReaderFunc func(context.Context, agent.LogTarget) (raw.LogSnapshot, error)

func (f logReaderFunc) Read(ctx context.Context, target agent.LogTarget) (raw.LogSnapshot, error) {
	return f(ctx, target)
}

func TestLogAPIAuthenticatesAndRechecksBeforeReturn(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	reads := 0
	a := LogReadAPI{Auth: auth, Reader: logReaderFunc(func(_ context.Context, target agent.LogTarget) (raw.LogSnapshot, error) {
		reads++
		require.Equal(t, "exact-allocation", target.Allocation.UID)
		require.Equal(t, "exact-task", target.TaskUID)
		return raw.LogSnapshot{Data: []byte("<script>never executed</script>\x1b[31m")}, nil
	})}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, LogPath+logQuery, nil))
	require.Equal(t, 401, w.Code)
	require.Zero(t, reads)
	w = request(t, a, http.MethodGet, LogPath+logQuery, "")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, 1, reads)
	for _, path := range []string{LogPath, LogPath + logQuery + "&path=/etc/passwd", LogPath + logQuery + "&taskUID=other", LogPath + strings.Replace(logQuery, "namespace=agents", "namespace=../other", 1)} {
		require.Equal(t, 400, request(t, a, http.MethodGet, path, "").Code)
	}
	require.Equal(t, 405, request(t, a, http.MethodPost, LogPath+logQuery, "{}").Code)
	// Simulate expiration while a read is waiting without sleeping on a JWT clock.
	check := 0
	a.Auth = authFunc(func(*http.Request) (string, error) {
		check++
		if check > 1 {
			return "", errUnauthorized
		}
		return "admin", nil
	})
	w = request(t, a, http.MethodGet, LogPath+logQuery, "")
	require.Equal(t, 401, w.Code)
	require.NotContains(t, w.Body.String(), "never executed")
}

type authFunc func(*http.Request) (string, error)

func (f authFunc) Administrator(r *http.Request) (string, error) { return f(r) }

func TestLogAPISanitizesFailuresAndBoundsResponses(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	for _, test := range []struct {
		err    error
		status int
	}{{agent.ErrLogBusy, 429}, {agent.ErrLogTimeout, 504}, {agent.ErrIdentity, 409}, {errors.New("private path and credential"), 503}} {
		a := LogReadAPI{Auth: auth, Reader: logReaderFunc(func(context.Context, agent.LogTarget) (raw.LogSnapshot, error) { return raw.LogSnapshot{}, test.err })}
		w := request(t, a, http.MethodGet, LogPath+logQuery, "")
		require.Equal(t, test.status, w.Code)
		require.NotContains(t, w.Body.String(), "private path")
	}
	a := LogReadAPI{Auth: auth, Reader: logReaderFunc(func(context.Context, agent.LogTarget) (raw.LogSnapshot, error) {
		return raw.LogSnapshot{Data: make([]byte, raw.MaxLogBytes+1)}, nil
	})}
	require.Equal(t, 502, request(t, a, http.MethodGet, LogPath+logQuery, "").Code)
}

func TestFixedLogProxyDoesNotForwardCookiesRedirectsOrRawErrors(t *testing.T) {
	for _, bad := range []string{"http://example.com", "http://localhost:9446", "https://127.0.0.1", "http://127.0.0.1/other", "http://user@127.0.0.1", "http://127.0.0.1?q=1"} {
		_, err := NewLogProxy(bad)
		require.Error(t, err)
	}
	for _, status := range []int{200, 302, 401, 503} {
		called := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called++
			require.Equal(t, LogPath, r.URL.Path)
			require.Equal(t, "GET", r.Method)
			require.Empty(t, r.Header.Get("Cookie"))
			require.Empty(t, r.Header.Get("X-Role"))
			require.NotEmpty(t, r.Header.Get("Authorization"))
			w.Header().Set("Set-Cookie", "secret=never-forward")
			w.Header().Set("Location", "http://other.example")
			w.WriteHeader(status)
			if status == 200 {
				_, _ = w.Write([]byte(`{"data":"b2s=","truncated":false,"retentionLimitReached":false}`))
			} else {
				_, _ = w.Write([]byte("private upstream detail"))
			}
		}))
		defer upstream.Close()
		proxy, err := NewLogProxy(upstream.URL)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodGet, LogPath+logQuery, nil)
		r.Header.Set("Authorization", "Bearer admin")
		r.Header.Set("Cookie", "never=forward")
		r.Header.Set("X-Role", "admin")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		want := status
		if status == 302 {
			want = 503
		}
		require.Equal(t, want, w.Code)
		require.Empty(t, w.Header().Get("Set-Cookie"))
		require.Empty(t, w.Header().Get("Location"))
		require.NotContains(t, w.Body.String(), "private upstream")
		require.Equal(t, 1, called)
	}
}

func TestLogAPIAndProxyReturnEmptyOutputAsBase64String(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	api := LogReadAPI{Auth: auth, Reader: logReaderFunc(func(context.Context, agent.LogTarget) (raw.LogSnapshot, error) {
		return raw.LogSnapshot{}, nil // No process output has arrived yet.
	})}
	upstream := httptest.NewServer(api)
	defer upstream.Close()
	proxy, err := NewLogProxy(upstream.URL)
	require.NoError(t, err)
	for _, handler := range []http.Handler{api, proxy} {
		w := request(t, handler, http.MethodGet, LogPath+logQuery, "")
		require.Equal(t, 200, w.Code)
		var snapshot map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snapshot))
		require.Equal(t, "", snapshot["data"])
		require.Equal(t, false, snapshot["truncated"])
	}
}
