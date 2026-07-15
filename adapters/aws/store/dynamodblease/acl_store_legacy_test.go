package dynamodblease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// legacyHeldRow builds an ACTIVELY-HELD lease row written by a build that
// predates the renewed_at attribute: owner and a positive expires_at are set,
// but renewed_at is ABSENT (getRow decodes it to 0). expires_at carries a real
// epoch-millis value so the row routes into the observation-takeover path
// (owner != "" and expires_at > 0), not the released-lease fast path.
func legacyHeldRow(owner string, version uint64, expiresAt time.Time) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK:        &ddbtypes.AttributeValueMemberS{Value: leaseKey("l1")},
		attrOwner:     &ddbtypes.AttributeValueMemberS{Value: owner},
		attrVersion:   &ddbtypes.AttributeValueMemberN{Value: uintStr(version)},
		attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
		// renewed_at intentionally ABSENT — the legacy-row hazard (finding
		// c13-legacy-renewedat): it decodes to 0, and observedTTL computed as
		// expires_at - 0 would be ~decades, so a standby would NEVER take over.
	}
}

// Finding c13-legacy-renewedat: a legacy lease row (renewed_at absent → 0) whose
// owner has died must become seizable within the NORMAL caller-provided TTL
// window, not after a ~decades-long epoch-1970 age. A standby observes the
// unchanged liveness tuple and, once one TTL of its OWN clock has elapsed,
// takes over.
//
// Mutation killed: revert observedTTL to `time.Duration(row.expiresAt-
// row.renewedAt) * time.Millisecond` (the raw renewed_at). With renewed_at=0
// that yields observedTTL ≈ expires_at (~decades), so the post-window Acquire
// below is rejected as "observation window not yet elapsed" (waits decades) and
// this test FAILs at the "takeover must become possible" assertion.
func TestAcquire_LegacyRenewedAtZero_SeizesWithinNormalTTLWindow(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())

	// expires_at is a real (current-epoch) millis value; the raw-renewed_at bug
	// turns expires_at itself into the "observed TTL".
	c := &seizeClient{item: legacyHeldRow("bridge-1", 5, fake.Now())}
	s := &Store{client: c, tableName: "leases-test", clk: fake}

	// First sighting: record the observation, do not seize.
	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting of a legacy lease must observe, got %v", err)
	}
	if c.updateCalls != 0 {
		t.Fatalf("no seize before the observation window elapses, got %d takeovers", c.updateCalls)
	}

	// Advance ONE caller TTL. With the fix observedTTL == ttl, so the window is
	// now satisfied; with the raw-renewed_at bug observedTTL ≈ decades and this
	// seize would never happen.
	fake.Advance(ttl + time.Millisecond)

	tok, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil)
	if err != nil {
		t.Fatalf("legacy-lease takeover must become possible within one TTL window, got %v "+
			"(raw renewed_at=0 would wait ~decades)", err)
	}
	if c.updateCalls != 1 {
		t.Fatalf("expected exactly one takeover after the TTL window, got %d", c.updateCalls)
	}
	if tok.Owner != "bridge-2" || tok.Version != 6 {
		t.Fatalf("post-window token: got owner=%q ver=%d, want owner=bridge-2 ver=6", tok.Owner, tok.Version)
	}
}

// Split-brain-on-foreign-rows [HIGH]: a LIVE legacy owner (renewed_at ABSENT)
// that keeps advancing expires_at — version unchanged, no renewed_at — must RESET
// the observation window and NEVER be seized. A renewed_at-absent row exposes no
// renewed_at liveness signal, so expires_at is the ONLY way to tell a live owner
// from a crashed one. This is the fast (fake-client) teeth; the ddblocal test
// TestAcquire_LegacyRowLiveRenewing_NotSeized proves it end-to-end.
//
// Mutation killed: drop the `obs.expiresAt != row.expiresAt` term from
// tupleChanged (acl_store.go). The frozen (owner, version) tuple then satisfies
// the elapsed window on the first advance, the standby issues a takeover
// UpdateItem, and this test FAILs (the live owner is seized; updateCalls != 0).
func TestAcquire_LegacyRowLiveExpiryAdvancing_ResetsWindowNoSeize(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())

	c := &seizeClient{item: legacyHeldRow("bridge-1", 5, fake.Now().Add(ttl))}
	s := &Store{client: c, tableName: "leases-test", clk: fake}

	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}
	for i := 0; i < 3; i++ {
		fake.Advance(ttl + time.Millisecond)
		// Live owner advances expires_at (version unchanged, renewed_at still absent).
		c.item = legacyHeldRow("bridge-1", 5, fake.Now().Add(ttl))
		if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
			t.Fatalf("iteration %d: a live legacy owner must NOT be seized, got %v", i, err)
		}
	}
	if c.updateCalls != 0 {
		t.Fatalf("a live legacy owner (expires_at advancing) must NEVER be seized; "+
			"got %d takeover UpdateItems (window failed to reset on expires_at)", c.updateCalls)
	}
}

// (owner, version) AND on the ABSENCE of renewed_at. An equality fence
// `#ren = :tuple_renewed` (with :obs_ren="0") is FALSE against an absent attribute in
// DynamoDB, so it would fail the conditional write forever and the crashed
// legacy owner would never fail over. This pins the ConditionExpression SHAPE
// (the seizeClient fake does not evaluate conditions, so shape is what has
// teeth here; the ddblocal integration test proves the write actually succeeds).
//
// Mutation killed: revert the seize condition to `#ren = :tuple_renewed` (drop the
// attribute_not_exists branch). The fake still lets the seize "succeed", but the
// attribute_not_exists(#ren) assertion below FAILs.
func TestAcquire_LegacyRenewedAtZero_SeizeFencesOnAttributeNotExists(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())

	c := &seizeClient{item: legacyHeldRow("bridge-1", 5, fake.Now())}
	s := &Store{client: c, tableName: "leases-test", clk: fake}

	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}
	fake.Advance(ttl + time.Millisecond)
	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); err != nil {
		t.Fatalf("seize after window: %v", err)
	}

	cond := c.lastUpdateExp
	if !strings.Contains(cond, "attribute_not_exists(#ren)") {
		t.Fatalf("legacy takeover must fence on the ABSENCE of renewed_at "+
			"(attribute_not_exists(#ren)); condition was %q", cond)
	}
	if strings.Contains(cond, ":tuple_renewed") {
		t.Fatalf("legacy takeover must NOT use an equality fence against an absent "+
			"attribute (#ren = :tuple_renewed is FALSE for absent renewed_at); condition was %q", cond)
	}
	// Still fenced on observed owner AND version so two standbys observing the
	// same legacy row cannot both seize (only one version fence wins).
	if !strings.Contains(cond, ":tuple_owner") || !strings.Contains(cond, ":tuple_version") {
		t.Fatalf("legacy takeover must still fence on observed (owner, version); condition was %q", cond)
	}
	// And on expires_at STABILITY (this legacy row has a present expires_at), so a
	// dead owner that revives in the getRow→UpdateItem gap by advancing only
	// expires_at cannot be seized (TOCTOU close).
	if !strings.Contains(cond, "#exp = :tuple_expires") {
		t.Fatalf("legacy takeover must fence on the observed expires_at "+
			"(#exp = :tuple_expires) to close the revive-at-seize TOCTOU; condition was %q", cond)
	}
}

// A MODERN row (renewed_at present & positive) must KEEP the exact-tuple equality
// fence `#ren = :tuple_renewed`; the attribute_not_exists branch is legacy-exclusive.
func TestAcquire_ModernRow_SeizeFencesOnRenewedAtEquality(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())
	base := fake.Now().Add(-2 * ttl)

	c := &seizeClient{item: heldRow("bridge-1", 5, base, ttl)}
	s := &Store{client: c, tableName: "leases-test", clk: fake}

	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}
	fake.Advance(ttl + time.Millisecond)
	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); err != nil {
		t.Fatalf("seize after window: %v", err)
	}

	cond := c.lastUpdateExp
	if !strings.Contains(cond, "#ren = :tuple_renewed") {
		t.Fatalf("modern takeover must fence on the exact observed renewed_at "+
			"(#ren = :tuple_renewed); condition was %q", cond)
	}
	if strings.Contains(cond, "attribute_not_exists(#ren)") {
		t.Fatalf("modern takeover must NOT use attribute_not_exists (that is legacy-only); condition was %q", cond)
	}
}
