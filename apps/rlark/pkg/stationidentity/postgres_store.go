package stationidentity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ValidatePostgresDSN requires hostname-verified TLS and a CA bundle before a
// database handle is opened. Credentials remain in the Secret-provided DSN.
func ValidatePostgresDSN(dsn string) error {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "postgres" || parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Path == "" || parsed.Path == "/" {
		return errors.New("invalid Station API PostgreSQL DSN")
	}
	query := parsed.Query()
	if query.Get("sslmode") != "verify-full" {
		return errors.New("Station API PostgreSQL requires sslmode=verify-full")
	}
	if strings.TrimSpace(query.Get("sslrootcert")) == "" {
		return errors.New("Station API PostgreSQL requires sslrootcert")
	}
	return nil
}

// PostgresChallengeStore atomically records and consumes challenges in a
// caller-provisioned Station database. It does not create schemas or roles.
type PostgresChallengeStore struct{ db *sql.DB }

func NewPostgresChallengeStore(db *sql.DB, qualifiedTable string) (*PostgresChallengeStore, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	if qualifiedTable != "station_api.challenges" {
		return nil, errors.New("challenge table must be station_api.challenges")
	}
	return &PostgresChallengeStore{db: db}, nil
}

func validateChallenge(ch Challenge, now time.Time) error {
	if ch.ClusterID == "" || ch.PodUID == "" || ch.NodeUID == "" || ch.StationUID == "" || ch.Operation == "" || ch.Generation <= 0 || !now.Before(ch.ExpiresAt) || ch.ExpiresAt.After(now.Add(5*time.Minute)) {
		return errors.New("invalid or expired challenge")
	}
	digest, err := hex.DecodeString(ch.RequestDigest)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != ch.RequestDigest {
		return errors.New("invalid challenge digest")
	}
	return nil
}

func (s *PostgresChallengeStore) Issue(ctx context.Context, ch Challenge) (string, error) {
	if err := validateChallenge(ch, time.Now()); err != nil {
		return "", err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate challenge ID: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	var inserted string
	err := s.db.QueryRowContext(ctx, `INSERT INTO station_api.challenges
		(challenge_id, cluster_id, pod_uid, node_uid, station_uid, operation, generation, request_digest, expires_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9
		WHERE $9 > clock_timestamp() AND $9 <= clock_timestamp() + interval '5 minutes'
		RETURNING challenge_id`, id, ch.ClusterID, ch.PodUID, ch.NodeUID, ch.StationUID, ch.Operation, ch.Generation, ch.RequestDigest, ch.ExpiresAt).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("challenge expired before persistence")
	}
	if err != nil {
		return "", fmt.Errorf("persist challenge: %w", err)
	}
	return id, nil
}

func (s *PostgresChallengeStore) Consume(ctx context.Context, id string, evidence Evidence, operation, request string, _ time.Time) error {
	if err := Validate(evidence); err != nil {
		return err
	}
	if id == "" || operation == "" || request == "" {
		return errors.New("invalid challenge request")
	}
	digest := sha256.Sum256([]byte(request))
	var consumed string
	err := s.db.QueryRowContext(ctx, `UPDATE station_api.challenges SET consumed_at=clock_timestamp()
		WHERE challenge_id=$1 AND cluster_id=$2 AND pod_uid=$3 AND node_uid=$4 AND station_uid=$5
		AND operation=$6 AND generation=$7 AND request_digest=$8 AND expires_at > clock_timestamp() AND consumed_at IS NULL
		RETURNING challenge_id`, id, evidence.ClusterID, evidence.TokenPodUID, evidence.NodeUID, evidence.StationUID, operation, evidence.Generation, hex.EncodeToString(digest[:])).Scan(&consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("challenge not found, mismatched, expired, or consumed")
	}
	if err != nil {
		return fmt.Errorf("consume challenge: %w", err)
	}
	return nil
}

// PruneExpired removes a bounded batch and requires a separate janitor role
// with SELECT and DELETE privileges. Concurrent janitors may wait on row locks;
// the API runtime role must not receive DELETE.
func (s *PostgresChallengeStore) PruneExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, errors.New("prune limit must be between 1 and 10000")
	}
	rows, err := s.db.QueryContext(ctx, `WITH expired AS (
		SELECT challenge_id FROM station_api.challenges WHERE expires_at <= clock_timestamp()
		ORDER BY expires_at, challenge_id LIMIT $1
	) DELETE FROM station_api.challenges c USING expired e WHERE c.challenge_id=e.challenge_id
	RETURNING c.challenge_id`, limit)
	if err != nil {
		return 0, fmt.Errorf("prune expired challenges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count int64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return count, fmt.Errorf("read pruned challenge: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("read pruned challenges: %w", err)
	}
	return count, nil
}
