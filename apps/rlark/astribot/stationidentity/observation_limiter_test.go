package stationidentity

import (
	"testing"
	"time"
)

func TestNodeLimiterBoundsEachNodeByItself(t *testing.T) {
	limiter := &NodeLimiter{Rate: 1, Burst: 2}
	start := time.Unix(1000, 0)
	evidence := validEvidence()
	firstAllowed := limiter.Allow(evidence, start)
	secondAllowed := limiter.Allow(evidence, start)
	thirdAllowed := limiter.Allow(evidence, start)
	if !firstAllowed || !secondAllowed || thirdAllowed {
		t.Fatal("burst limit was not enforced")
	}
	other := evidence
	other.NodeUID = "other-node"
	if !limiter.Allow(other, start) {
		t.Fatal("one node used up another's requests")
	}
	if !limiter.Allow(evidence, start.Add(time.Second)) {
		t.Fatal("token bucket did not refill")
	}
}
