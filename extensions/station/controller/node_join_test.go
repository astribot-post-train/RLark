package controller

import (
	"context"
	"errors"
	"testing"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type joinDriverStub struct {
	state                NodeJoinState
	observeErr, startErr error
	starts               int
}

func (s *joinDriverStub) Observe(context.Context, *stationv1alpha1.NodeOperation) (NodeJoinState, error) {
	return s.state, s.observeErr
}
func (s *joinDriverStub) Start(context.Context, *stationv1alpha1.NodeOperation) error {
	s.starts++
	return s.startErr
}

func TestExecuteNodeJoinDoesNotReplayOrAdopt(t *testing.T) {
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{UID: "operation"}, Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", NodeName: "node"}}
	tests := []struct {
		name    string
		state   NodeJoinState
		wantErr error
		done    bool
		starts  int
	}{
		{"fresh", NodeJoinState{}, nil, false, 1},
		{"existing configuration", NodeJoinState{ExistingConfiguration: true}, ErrNodeAlreadyConfigured, false, 0},
		{"unknown previous result", NodeJoinState{Attempted: true}, ErrJoinOutcomeUnknown, false, 0},
		{"await registration", NodeJoinState{Attempted: true, Pending: true}, nil, false, 0},
		{"unowned existing node", NodeJoinState{VerifiedUID: "uid"}, ErrNodeAlreadyConfigured, false, 0},
		{"verified previous attempt", NodeJoinState{Attempted: true, VerifiedUID: "uid"}, nil, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver := &joinDriverStub{state: tc.state}
			done, uid, err := ExecuteNodeJoin(context.Background(), driver, op)
			if !errors.Is(err, tc.wantErr) || done != tc.done || driver.starts != tc.starts || (done && uid != "uid") {
				t.Fatalf("done=%v uid=%q starts=%d err=%v", done, uid, driver.starts, err)
			}
		})
	}
}

func TestExecuteNodeJoinFailureDoesNotRetryWithinAttempt(t *testing.T) {
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{UID: "operation"}, Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", NodeName: "node"}}
	driver := &joinDriverStub{startErr: errors.New("connection lost")}
	if _, _, err := ExecuteNodeJoin(context.Background(), driver, op); err == nil || driver.starts != 1 {
		t.Fatalf("starts=%d err=%v", driver.starts, err)
	}
	driver.state = NodeJoinState{Attempted: true}
	if _, _, err := ExecuteNodeJoin(context.Background(), driver, op); !errors.Is(err, ErrJoinOutcomeUnknown) || driver.starts != 1 {
		t.Fatalf("replayed uncertain join: starts=%d err=%v", driver.starts, err)
	}
}
