package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type countUpgradeRequests struct{ count int }

func (c *countUpgradeRequests) RoundTrip(_ *http.Request) (*http.Response, error) {
	c.count++
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func TestStockUpgradeChecksLeaseBeforeEveryWrite(t *testing.T) {
	underlying := &countUpgradeRequests{}
	conflict := errors.New("lease lost")
	var checkErr error
	transport := upgradeWriteTransport{
		next:  underlying,
		check: func(context.Context) error { return checkErr },
	}
	for _, method := range []string{http.MethodPatch, http.MethodPut, http.MethodDelete} {
		request, err := http.NewRequestWithContext(t.Context(), method, "https://member.invalid/api", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatal(err)
		}
	}
	checkErr = conflict
	blocked, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://member.invalid/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(blocked); !errors.Is(err, conflict) || underlying.count != 3 {
		t.Fatalf("write got through the fence: requests=%d, err=%v", underlying.count, err)
	}
}
