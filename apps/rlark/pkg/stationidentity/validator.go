// Package stationidentity contains pure validation used by the future
// station-api. It does not contact Kubernetes and is not an authentication
// implementation by itself.
package stationidentity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Evidence struct {
	ClusterID, RegisteredClusterID                                                             string
	ServiceAccountNamespace, ServiceAccountName, RegisteredNamespace, RegisteredServiceAccount string
	TokenServiceAccountUID, RegisteredServiceAccountUID, PodServiceAccountName                 string
	TokenPodName, PodName, PodNodeName, NodeName                                               string
	TokenPodUID, PodUID, NodeUID, RegisteredNodeUID, StationUID, RegisteredStationUID          string
	TokenAudiences, RequiredAudiences                                                          []string
	TokenAuthenticated, PodDeleted, NodeDeleted, BindingInactive                               bool
	Generation, RegisteredGeneration                                                           int64
}

func Validate(e Evidence) error {
	if !e.TokenAuthenticated {
		return errors.New("token is not authenticated")
	}
	if e.ClusterID == "" || e.ClusterID != e.RegisteredClusterID {
		return errors.New("cluster identity mismatch")
	}
	if e.ServiceAccountNamespace == "" || e.ServiceAccountNamespace != e.RegisteredNamespace || e.ServiceAccountName == "" || e.ServiceAccountName != e.RegisteredServiceAccount {
		return errors.New("service account identity mismatch")
	}
	if e.TokenServiceAccountUID == "" || e.TokenServiceAccountUID != e.RegisteredServiceAccountUID || e.PodServiceAccountName != e.ServiceAccountName {
		return errors.New("service account instance mismatch")
	}
	if e.TokenPodName == "" || e.TokenPodName != e.PodName || e.TokenPodUID == "" || e.TokenPodUID != e.PodUID || e.PodDeleted || e.NodeDeleted {
		return errors.New("pod identity is not current")
	}
	if e.PodNodeName == "" || e.PodNodeName != e.NodeName || e.NodeUID == "" || e.NodeUID != e.RegisteredNodeUID || e.BindingInactive {
		return errors.New("node binding mismatch")
	}
	stationUnbound := e.StationUID == "" && e.RegisteredStationUID == "" && e.Generation == 0 && e.RegisteredGeneration == 0
	stationBound := e.StationUID != "" && e.StationUID == e.RegisteredStationUID && e.Generation > 0 && e.Generation == e.RegisteredGeneration
	if !stationUnbound && !stationBound {
		return errors.New("station binding mismatch")
	}
	if len(e.RequiredAudiences) == 0 || !audienceContainsAll(e.TokenAudiences, e.RequiredAudiences) {
		return errors.New("token audience is not authorized")
	}
	return nil
}

func audienceContainsAll(got, required []string) bool {
	for _, want := range required {
		if want == "" {
			return false
		}
		found := false
		for _, value := range got {
			if subtle.ConstantTimeCompare([]byte(value), []byte(want)) == 1 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type Challenge struct {
	ClusterID, PodUID, NodeUID, StationUID, Operation, RequestDigest string
	Generation                                                       int64
	ExpiresAt                                                        time.Time
}

// ChallengeStore is the persistence boundary station-api must implement with
// a shared durable store. Consume must be atomic across instances.
type ChallengeStore interface {
	Issue(context.Context, Challenge) (string, error)
	Consume(context.Context, string, Evidence, string, string, time.Time) error
}

// MemoryChallengeStore is a concurrency-safe test implementation. It is not
// suitable for a multi-instance service because state is process-local.
type MemoryChallengeStore struct {
	mu     sync.Mutex
	issued map[string]Challenge
}

func NewMemoryChallengeStore() *MemoryChallengeStore {
	return &MemoryChallengeStore{issued: make(map[string]Challenge)}
}

func (s *MemoryChallengeStore) Issue(_ context.Context, ch Challenge) (string, error) {
	if err := validateChallenge(ch, time.Now()); err != nil {
		return "", err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.issued[id]; exists {
		return "", errors.New("challenge collision")
	}
	s.issued[id] = ch
	return id, nil
}

func (s *MemoryChallengeStore) Consume(_ context.Context, id string, evidence Evidence, operation, request string, now time.Time) error {
	if err := Validate(evidence); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, exists := s.issued[id]
	if !exists {
		return errors.New("challenge not found or consumed")
	}
	if err := ch.Match(evidence, operation, request, now); err != nil {
		return err
	}
	delete(s.issued, id)
	return nil
}

func NewChallenge(clusterID, podUID, nodeUID, stationUID, operation, request string, generation int64, now time.Time, ttl time.Duration) (Challenge, error) {
	if clusterID == "" || podUID == "" || nodeUID == "" || stationUID == "" || operation == "" || request == "" || generation <= 0 || ttl <= 0 || ttl > 5*time.Minute {
		return Challenge{}, errors.New("invalid challenge input")
	}
	digest := sha256.Sum256([]byte(request))
	return Challenge{clusterID, podUID, nodeUID, stationUID, operation, hex.EncodeToString(digest[:]), generation, now.Add(ttl)}, nil
}

func (c Challenge) Match(e Evidence, operation, request string, now time.Time) error {
	if err := Validate(e); err != nil {
		return err
	}
	want, err := NewChallenge(e.ClusterID, e.TokenPodUID, e.NodeUID, e.StationUID, operation, request, e.Generation, now, time.Second)
	if err != nil {
		return err
	}
	if !now.Before(c.ExpiresAt) || c.ClusterID != want.ClusterID || c.PodUID != want.PodUID || c.NodeUID != want.NodeUID || c.StationUID != want.StationUID || c.Operation != want.Operation || c.Generation != want.Generation || c.RequestDigest != want.RequestDigest {
		return fmt.Errorf("challenge mismatch or expired")
	}
	return nil
}
