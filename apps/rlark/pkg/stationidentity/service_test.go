package stationidentity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeReader struct {
	evidence Evidence
	err      error
	calls    int
}

func (f *fakeReader) Authenticate(context.Context, string) (Evidence, error) {
	f.calls++
	return f.evidence, f.err
}

type fakeChallengeStore struct {
	challenge            Challenge
	id                   string
	issueErr, consumeErr error
	issues, consumes     int
}

func (f *fakeChallengeStore) Issue(_ context.Context, ch Challenge) (string, error) {
	f.issues++
	f.challenge = ch
	return f.id, f.issueErr
}
func (f *fakeChallengeStore) Consume(_ context.Context, id string, evidence Evidence, operation, request string, now time.Time) error {
	f.consumes++
	if id != f.id {
		return errors.New("wrong ID")
	}
	if err := f.challenge.Match(evidence, operation, request, now); err != nil {
		return err
	}
	return f.consumeErr
}

func TestServiceRevalidatesBeforeIssueAndConsume(t *testing.T) {
	now := time.Unix(1000, 0)
	reader := &fakeReader{evidence: validEvidence()}
	store := &fakeChallengeStore{id: "opaque-id"}
	service, err := NewService(reader, store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.Issue(context.Background(), "token", "write", "payload", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id != "opaque-id" || reader.calls != 1 || store.issues != 1 || store.challenge.ExpiresAt.Sub(now) != time.Minute {
		t.Fatal("issue contract mismatch")
	}
	if err = service.Consume(context.Background(), "token", id, "write", "payload"); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 2 || store.consumes != 1 {
		t.Fatal("consume did not revalidate current identity")
	}
	if _, err = service.Issue(context.Background(), "token", "write", string(make([]byte, (1<<20)+1)), time.Minute); err == nil {
		t.Fatal("accepted oversized request")
	}
	reader.evidence.BindingInactive = true
	if err = service.Consume(context.Background(), "token", id, "write", "payload"); err == nil {
		t.Fatal("accepted inactive binding")
	}
	if store.consumes != 1 {
		t.Fatal("consumed challenge with invalid evidence")
	}
}

func TestServiceFailsClosedOnReaderAndStoreErrors(t *testing.T) {
	reader := &fakeReader{evidence: validEvidence(), err: errors.New("token review down")}
	store := &fakeChallengeStore{id: "id"}
	service, _ := NewService(reader, store, time.Now)
	if _, err := service.Issue(context.Background(), "token", "write", "body", time.Minute); err == nil {
		t.Fatal("issued without identity")
	}
	reader.err = nil
	store.issueErr = errors.New("store down")
	if _, err := service.Issue(context.Background(), "token", "write", "body", time.Minute); err == nil {
		t.Fatal("ignored issue persistence error")
	}
	store.issueErr = nil
	store.consumeErr = errors.New("consume down")
	if err := service.Consume(context.Background(), "token", "id", "write", "body"); err == nil {
		t.Fatal("ignored consume persistence error")
	}
}
