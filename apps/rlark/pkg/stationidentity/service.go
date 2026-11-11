package stationidentity

import (
	"context"
	"errors"
	"regexp"
	"time"
)

var operationPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)

// IdentityReader resolves fresh, server-side evidence from a registered cluster.
type IdentityReader interface {
	Authenticate(context.Context, string) (Evidence, error)
}

type Service struct {
	reader     IdentityReader
	challenges ChallengeStore
	now        func() time.Time
}

func NewService(reader IdentityReader, challenges ChallengeStore, now func() time.Time) (*Service, error) {
	if reader == nil || challenges == nil {
		return nil, errors.New("identity reader and challenge store are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{reader: reader, challenges: challenges, now: now}, nil
}

func (s *Service) Issue(ctx context.Context, token, operation, request string, ttl time.Duration) (string, error) {
	if token == "" || !operationPattern.MatchString(operation) || request == "" || len(request) > 1<<20 {
		return "", errors.New("invalid sensitive request")
	}
	evidence, err := s.reader.Authenticate(ctx, token)
	if err != nil {
		return "", err
	}
	if err := Validate(evidence); err != nil {
		return "", err
	}
	now := s.now()
	challenge, err := NewChallenge(evidence.ClusterID, evidence.TokenPodUID, evidence.NodeUID, evidence.StationUID, operation, request, evidence.Generation, now, ttl)
	if err != nil {
		return "", err
	}
	return s.challenges.Issue(ctx, challenge)
}

func (s *Service) Consume(ctx context.Context, token, challengeID, operation, request string) error {
	if token == "" || challengeID == "" || !operationPattern.MatchString(operation) || request == "" || len(request) > 1<<20 {
		return errors.New("invalid sensitive request")
	}
	evidence, err := s.reader.Authenticate(ctx, token)
	if err != nil {
		return err
	}
	if err := Validate(evidence); err != nil {
		return err
	}
	return s.challenges.Consume(ctx, challengeID, evidence, operation, request, s.now())
}
