package workflow

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

func TestNativeJoinUIDRetriesTransientMemberFailure(t *testing.T) {
	var calls int
	service := &Service{hooks: Hooks{ProxyKubeRequest: func(_ context.Context, _ string, _ string, _ string, _ []byte) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"node","uid":"original"}}`))}, nil
	}}}
	node := &rlark.NodeJoin{Spec: rlark.NodeJoinSpec{ClusterID: "member", NodeName: "node"}}
	if err := service.verifyNativeJoinUID(t.Context(), node, "original"); err != nil || calls != 2 {
		t.Fatalf("verification made %d requests: %v", calls, err)
	}
}
