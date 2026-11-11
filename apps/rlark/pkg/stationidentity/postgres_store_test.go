package stationidentity

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

func TestPostgresChallengeStoreAtomicConsume(t *testing.T) {
	dsn := os.Getenv("STATION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires a dedicated disposable PostgreSQL database")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithTimeout(3*time.Second)))
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("PostgreSQL test database unavailable")
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA station_api; CREATE TABLE station_api.challenges (
	 challenge_id text PRIMARY KEY, cluster_id text NOT NULL, pod_uid text NOT NULL, node_uid text NOT NULL,
	 station_uid text NOT NULL, operation text NOT NULL, generation bigint NOT NULL,
	 request_digest char(64) NOT NULL, expires_at timestamptz NOT NULL, consumed_at timestamptz,
	 created_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = db.ExecContext(cleanup, `RESET ROLE`)
		if _, err := db.ExecContext(cleanup, `DROP SCHEMA station_api CASCADE`); err != nil {
			t.Errorf("drop temporary challenge schema: %v", err)
		}
	})
	store1, err := NewPostgresChallengeStore(db, "station_api.challenges")
	if err != nil {
		t.Fatal(err)
	}
	store2, err := NewPostgresChallengeStore(db, "station_api.challenges")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	evidence := Evidence{ClusterID: "c", RegisteredClusterID: "c", ServiceAccountNamespace: "agent", RegisteredNamespace: "agent", ServiceAccountName: "sa", RegisteredServiceAccount: "sa", TokenServiceAccountUID: "sauid", RegisteredServiceAccountUID: "sauid", PodServiceAccountName: "sa", TokenPodName: "pod", PodName: "pod", PodNodeName: "node", NodeName: "node", TokenPodUID: "poduid", PodUID: "poduid", NodeUID: "nodeuid", RegisteredNodeUID: "nodeuid", StationUID: "stationuid", RegisteredStationUID: "stationuid", TokenAuthenticated: true, TokenAudiences: []string{"station-api"}, RequiredAudiences: []string{"station-api"}, Generation: 2, RegisteredGeneration: 2}
	challenge, err := NewChallenge("c", "poduid", "nodeuid", "stationuid", "write", `{"desired":1}`, 2, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store1.Issue(ctx, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.Consume(ctx, id, evidence, "write", `{"desired":2}`, now.Add(time.Second)); err == nil {
		t.Fatal("accepted mismatched request")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := store1
			if i%2 == 1 {
				store = store2
			}
			if store.Consume(ctx, id, evidence, "write", `{"desired":1}`, now.Add(2*time.Second)) == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successful consumers=%d, want 1", successes)
	}
	if err := store1.Consume(ctx, id, evidence, "write", `{"desired":1}`, now.Add(3*time.Second)); err == nil {
		t.Fatal("accepted challenge replay")
	}

	expired, err := NewChallenge("c", "poduid", "nodeuid", "stationuid", "write", "expired", 2, now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	expiredID, err := store1.Issue(ctx, expired)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expired.ExpiresAt) + 50*time.Millisecond)
	if err := store2.Consume(ctx, expiredID, evidence, "write", "expired", time.Now()); err == nil {
		t.Fatal("accepted expired challenge")
	}
	if _, err := db.ExecContext(ctx, `UPDATE station_api.challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE challenge_id=$1`, expiredID); err != nil {
		t.Fatal(err)
	}
	if err := store2.Consume(ctx, expiredID, evidence, "write", "expired", now); err == nil {
		t.Fatal("accepted expired row")
	}
	pruned, err := store1.PruneExpired(ctx, 100)
	if err != nil || pruned != 1 {
		t.Fatalf("prune expired challenge count=%d err=%v", pruned, err)
	}
	dbBoundExpiry, err := NewChallenge("c", "poduid", "nodeuid", "stationuid", "write", "db-bound-expiry", 2, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dbBoundExpiry.ExpiresAt = time.Now().Add(300 * time.Millisecond)
	if _, err := store1.Issue(ctx, dbBoundExpiry); err != nil {
		t.Fatalf("issue short-lived challenge: %v", err)
	}
	validChallenge, err := NewChallenge("c", "poduid", "nodeuid", "stationuid", "write", "still-valid", 2, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	validID, err := store1.Issue(ctx, validChallenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE station_api.challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE request_digest=$1`, dbBoundExpiry.RequestDigest); err != nil {
		t.Fatal(err)
	}
	pruned, err = store1.PruneExpired(ctx, 1)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned=%d err=%v, want one expired record", pruned, err)
	}
	if err := store2.Consume(ctx, validID, evidence, "write", "still-valid", time.Now()); err != nil {
		t.Fatalf("pruned unexpired challenge: %v", err)
	}
	pruned, err = store2.PruneExpired(ctx, 1)
	if err != nil || pruned != 0 {
		t.Fatalf("second prune=%d err=%v, want no records", pruned, err)
	}
	if err := store2.Consume(ctx, id, evidence, "write", `{"desired":1}`, time.Now()); err == nil {
		t.Fatal("prune removed or revived wrong challenge state")
	}
	if _, err := db.ExecContext(ctx, `CREATE ROLE station_api_runtime_test NOLOGIN; CREATE ROLE station_api_janitor_test NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = db.ExecContext(cleanup, `RESET ROLE; DROP OWNED BY station_api_runtime_test; DROP OWNED BY station_api_janitor_test; DROP ROLE station_api_runtime_test; DROP ROLE station_api_janitor_test`)
	})
	if _, err := db.ExecContext(ctx, `GRANT USAGE ON SCHEMA station_api TO station_api_runtime_test, station_api_janitor_test; GRANT SELECT, INSERT, UPDATE ON station_api.challenges TO station_api_runtime_test; GRANT SELECT, DELETE ON station_api.challenges TO station_api_janitor_test`); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE station_api_runtime_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM station_api.challenges`); err == nil {
		t.Fatal("runtime role unexpectedly has DELETE")
	}
	if _, err := db.ExecContext(ctx, `SET ROLE station_api_janitor_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := store1.PruneExpired(ctx, 1); err != nil {
		t.Fatalf("janitor role could not prune: %v", err)
	}
	if _, err := db.ExecContext(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePostgresDSNRequiresVerifiedTLS(t *testing.T) {
	valid := "postgres://station:secret@db.example:5432/station?sslmode=verify-full&sslrootcert=%2Fvar%2Frun%2Fsecrets%2Fdb%2Fca.crt"
	if err := ValidatePostgresDSN(valid); err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{
		"postgres://station:secret@db.example:5432/station?sslmode=disable",
		"postgres://station:secret@db.example:5432/station?sslmode=require",
		"postgres://station:secret@db.example:5432/station?sslmode=verify-full",
		"http://station:secret@db.example:5432/station?sslmode=verify-full&sslrootcert=/ca.crt",
		"postgres://db.example:5432/station?sslmode=verify-full&sslrootcert=/ca.crt",
	} {
		if err := ValidatePostgresDSN(dsn); err == nil {
			t.Fatalf("accepted insecure or malformed DSN: %s", dsn)
		}
	}
}
