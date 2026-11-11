package stationidentity

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMemoryChallengeStoreConsumesExactlyOnce(t *testing.T) {
	e := validEvidence()
	now := time.Now()
	challenge, err := NewChallenge("c", "p", "n", "s", "write", "body", 1, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryChallengeStore()
	id, err := store.Issue(context.Background(), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(context.Background(), "forged", e, "write", "body", now); err == nil {
		t.Fatal("accepted unissued challenge")
	}
	if err := store.Consume(context.Background(), id, e, "write", "changed", now); err == nil {
		t.Fatal("accepted changed request")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if store.Consume(context.Background(), id, e, "write", "body", now.Add(time.Second)) == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successes = %d, want 1", successes)
	}
	if err := store.Consume(context.Background(), id, e, "write", "body", now.Add(2*time.Second)); err == nil {
		t.Fatal("accepted replay")
	}
	second, err := store.Issue(context.Background(), challenge)
	if err != nil || second == id {
		t.Fatal("challenge ID not unique", err)
	}
	if err := store.Consume(context.Background(), second, e, "write", "body", now.Add(time.Minute)); err == nil {
		t.Fatal("accepted at expiration")
	}
}
