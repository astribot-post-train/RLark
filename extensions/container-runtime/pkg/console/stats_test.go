package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/stretchr/testify/require"
)

type statsReaderFunc func(context.Context, agent.LogTarget) (dockerruntime.StatsSnapshot, error)

func (f statsReaderFunc) ReadStats(ctx context.Context, t agent.LogTarget) (dockerruntime.StatsSnapshot, error) {
	return f(ctx, t)
}
func TestStatsAPIAndFixedProxyValidateIdentityAuthAndCounters(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	reads := 0
	api := StatsReadAPI{Auth: auth, Reader: statsReaderFunc(func(_ context.Context, target agent.LogTarget) (dockerruntime.StatsSnapshot, error) {
		reads++
		require.Equal(t, "exact-task", target.TaskUID)
		require.Equal(t, "exact-allocation", target.Allocation.UID)
		return dockerruntime.StatsSnapshot{SampledAt: time.Now().UTC().Format(time.RFC3339Nano), CPUUsageNS: "18446744073709551615", MemoryUsageBytes: "0"}, nil
	})}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, StatsPath+logQuery, nil))
	require.Equal(t, 401, w.Code)
	require.Zero(t, reads)
	upstream := httptest.NewServer(api)
	defer upstream.Close()
	proxy, err := NewStatsProxy(upstream.URL)
	require.NoError(t, err)
	for _, h := range []http.Handler{api, proxy} {
		w := request(t, h, http.MethodGet, StatsPath+logQuery, "")
		require.Equal(t, 200, w.Code)
		require.Contains(t, w.Body.String(), `"cpuUsageNS":"18446744073709551615"`)
		require.NotContains(t, w.Body.String(), `"pids"`)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		require.Equal(t, 400, request(t, h, http.MethodGet, StatsPath+logQuery+"&taskUID=other", "").Code)
		require.Equal(t, 400, request(t, h, http.MethodGet, StatsPath+logQuery+"&path=/etc/passwd", "").Code)
		require.Equal(t, 405, request(t, h, http.MethodPost, StatsPath+logQuery, "").Code)
		require.Equal(t, 404, request(t, h, http.MethodGet, LogPath+logQuery, "").Code)
	}
	checks := 0
	api.Auth = authFunc(func(*http.Request) (string, error) {
		checks++
		if checks > 1 {
			return "", errUnauthorized
		}
		return "admin", nil
	})
	w = request(t, api, http.MethodGet, StatsPath+logQuery, "")
	require.Equal(t, 401, w.Code)
	require.NotContains(t, w.Body.String(), "18446744073709551615")
}
func TestStatsAPISanitizesFailureAndMalformedSamples(t *testing.T) {
	auth, err := NewGatewayJWT(testKey)
	require.NoError(t, err)
	for _, test := range []struct {
		err    error
		status int
	}{{agent.ErrLogBusy, 429}, {agent.ErrLogTimeout, 504}, {agent.ErrIdentity, 409}, {errors.New("private path"), 503}, {nil, 502}} {
		api := StatsReadAPI{Auth: auth, Reader: statsReaderFunc(func(context.Context, agent.LogTarget) (dockerruntime.StatsSnapshot, error) {
			return dockerruntime.StatsSnapshot{}, test.err
		})}
		w := request(t, api, http.MethodGet, StatsPath+logQuery, "")
		require.Equal(t, test.status, w.Code)
		require.NotContains(t, w.Body.String(), "private path")
	}
	for _, origin := range []string{"http://localhost:9446", "https://127.0.0.1", "http://example.test", "http://127.0.0.1/other"} {
		_, err := NewStatsProxy(origin)
		require.Error(t, err)
	}
}
