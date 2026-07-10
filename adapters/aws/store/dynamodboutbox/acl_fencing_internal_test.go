package dynamodboutbox

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// invalidTokens enumerates every zero-value / partially-populated LeaseToken
// that persistence.LeaseToken.Valid rejects: empty owner, zero version, or
// both. A real lease always names a non-empty owner and a version >= 1.
var invalidTokens = []struct {
	name  string
	token persistence.LeaseToken
}{
	{"zero_value", persistence.LeaseToken{}},
	{"empty_owner", persistence.LeaseToken{Version: 1}},
	{"zero_version", persistence.LeaseToken{Owner: "owner"}},
}

// assertNoDDBCalls fails the test if the fake observed ANY DynamoDB call. It is
// the mutation/read probe behind the fencing guards: a valid guard rejects the
// invalid token as the FIRST statement, BEFORE any fence read, condition
// expression, or write, so the store must not have touched DynamoDB at all.
func assertNoDDBCalls(t *testing.T, f *fakeDDB) {
	t.Helper()
	if f.getItemCalls != 0 {
		t.Errorf("invalid token issued %d GetItem calls; want 0 (no fence read)", f.getItemCalls)
	}
	if f.updateItemCalls != 0 {
		t.Errorf("invalid token issued %d UpdateItem calls; want 0 (no mutation)", f.updateItemCalls)
	}
	if f.transactCalls != 0 {
		t.Errorf("invalid token issued %d TransactWriteItems calls; want 0 (no claim write)", f.transactCalls)
	}
	if f.putItemCalls != 0 {
		t.Errorf("invalid token issued %d PutItem calls; want 0", f.putItemCalls)
	}
	for idx, n := range f.queryCalls {
		if n != 0 {
			t.Errorf("invalid token issued %d Query calls on index %q; want 0 (no scan)", n, idx)
		}
	}
}

// TestClaim_InvalidToken_RejectedNoMutation pins the F1 fencing guard on the
// DynamoDB Claim path: a zero-value / invalid LeaseToken is rejected with
// shared.ErrStaleFencingToken, claims 0 records, and issues NO DynamoDB call
// (not even the O(1) fence read) — the raw-DDB path can never run with a bad
// token. Mutation-verify: delete the guard and Claim reaches s.maxClaimVersion,
// firing a GetItem, so assertNoDDBCalls fails.
func TestClaim_InvalidToken_RejectedNoMutation(t *testing.T) {
	for _, tc := range invalidTokens {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDDB()
			s := newFakeStore(f)

			claimed, err := s.Claim(context.Background(), "SESSION#s1", tc.token, 10)
			if !errors.Is(err, shared.ErrStaleFencingToken) {
				t.Fatalf("Claim(%+v): got err %v; want shared.ErrStaleFencingToken", tc.token, err)
			}
			if len(claimed) != 0 {
				t.Fatalf("Claim(%+v): claimed %d records; want 0", tc.token, len(claimed))
			}
			assertNoDDBCalls(t, f)
		})
	}
}

// TestComplete_InvalidToken_RejectedNoMutation pins the F1 fencing guard on the
// DynamoDB Complete path. The record's base-table keys are pre-cached so the
// ONLY DynamoDB op a guard-less Complete could reach is the terminal
// UpdateItem; the guard rejects the invalid token first, so updateItemCalls
// stays 0. Mutation-verify: delete the guard and Complete issues the UpdateItem,
// failing assertNoDDBCalls.
func TestComplete_InvalidToken_RejectedNoMutation(t *testing.T) {
	for _, tc := range invalidTokens {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDDB()
			s := newFakeStore(f)
			s.cacheKey("rec-1", "SESSION#s1", "OUTBOX#env#bind")

			err := s.Complete(context.Background(), []string{"rec-1"}, tc.token)
			if !errors.Is(err, shared.ErrStaleFencingToken) {
				t.Fatalf("Complete(%+v): got err %v; want shared.ErrStaleFencingToken", tc.token, err)
			}
			assertNoDDBCalls(t, f)
		})
	}
}

// TestRelease_InvalidToken_RejectedNoMutation pins the F1 fencing guard on the
// DynamoDB Release path, identical in shape to Complete.
func TestRelease_InvalidToken_RejectedNoMutation(t *testing.T) {
	for _, tc := range invalidTokens {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDDB()
			s := newFakeStore(f)
			s.cacheKey("rec-1", "SESSION#s1", "OUTBOX#env#bind")

			err := s.Release(context.Background(), []string{"rec-1"}, tc.token)
			if !errors.Is(err, shared.ErrStaleFencingToken) {
				t.Fatalf("Release(%+v): got err %v; want shared.ErrStaleFencingToken", tc.token, err)
			}
			assertNoDDBCalls(t, f)
		})
	}
}

// TestComplete_ValidToken_Mutates is the positive control: with a VALID token
// and a cached key, Complete DOES issue the terminal UpdateItem. It proves the
// mutation counter actually observes the write, so a guard regression that let
// an invalid token through would be caught by
// TestComplete_InvalidToken_RejectedNoMutation (not silently pass on a probe
// that can never fire).
func TestComplete_ValidToken_Mutates(t *testing.T) {
	f := newFakeDDB()
	s := newFakeStore(f)
	s.cacheKey("rec-1", "SESSION#s1", "OUTBOX#env#bind")

	err := s.Complete(context.Background(), []string{"rec-1"}, persistence.LeaseToken{Version: 1, Owner: "owner"})
	if err != nil {
		t.Fatalf("Complete(valid): %v", err)
	}
	if f.updateItemCalls != 1 {
		t.Fatalf("Complete(valid): UpdateItem calls = %d; want 1", f.updateItemCalls)
	}
}

// TestRelease_ValidToken_Mutates is the positive control for the Release path.
func TestRelease_ValidToken_Mutates(t *testing.T) {
	f := newFakeDDB()
	s := newFakeStore(f)
	s.cacheKey("rec-1", "SESSION#s1", "OUTBOX#env#bind")

	err := s.Release(context.Background(), []string{"rec-1"}, persistence.LeaseToken{Version: 1, Owner: "owner"})
	if err != nil {
		t.Fatalf("Release(valid): %v", err)
	}
	if f.updateItemCalls != 1 {
		t.Fatalf("Release(valid): UpdateItem calls = %d; want 1", f.updateItemCalls)
	}
}
