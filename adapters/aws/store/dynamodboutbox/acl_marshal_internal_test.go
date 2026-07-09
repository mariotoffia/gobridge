package dynamodboutbox

// Internal unit tests for the replay-budget first_attempted_at attribute. They
// run without DynamoDB Local (no build tag / no ddblocal client): they exercise
// the pure marshal/unmarshal ACL and the numeric-attribute helper directly, so
// the omit-when-zero and round-trip behaviour is covered even when `make test`
// runs in -short mode. The end-to-end claim stamp is covered by
// TestOutboxFirstAttemptConformance under the integration gate.

import (
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

func newMarshalRecord(t *testing.T) *persistence.OutboxRecord {
	t.Helper()
	return persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "fa-1", RouteID: "r", EnvelopeID: "e1", BindingID: "b1", SessionID: "s1", Address: "a",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t"}),
	})
}

func mustMarshalRecord(t *testing.T, r *persistence.OutboxRecord) map[string]ddbtypes.AttributeValue {
	t.Helper()
	item, err := marshalRecord(r, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("marshalRecord: %v", err)
	}
	return item
}

// TestMarshalRecord_OmitsFirstAttemptedAtWhenZero: an unclaimed record (the only
// state Persist ever marshals) must NOT carry a first_attempted_at attribute,
// and it unmarshals back to a zero first attempt. This keeps legacy items —
// which never had the attribute — bit-for-bit compatible: absent → zero.
func TestMarshalRecord_OmitsFirstAttemptedAtWhenZero(t *testing.T) {
	r := newMarshalRecord(t)
	item := mustMarshalRecord(t, r)

	if _, ok := item["first_attempted_at"]; ok {
		t.Fatal("unclaimed record must omit first_attempted_at")
	}

	back, err := unmarshalRecord(item)
	if err != nil {
		t.Fatalf("unmarshalRecord: %v", err)
	}
	if !back.FirstAttemptedAt().IsZero() {
		t.Fatalf("round-trip must keep a zero first attempt, got %v", back.FirstAttemptedAt())
	}
}

// TestMarshalUnmarshal_FirstAttemptedAtRoundTrip: a claimed record's stamped
// first_attempted_at marshals as an N-millis attribute and unmarshals back to
// the same instant.
func TestMarshalUnmarshal_FirstAttemptedAtRoundTrip(t *testing.T) {
	claimAt := time.UnixMilli(1_700_000_500_000) // whole millis, no sub-milli
	r := newMarshalRecord(t)
	if err := r.Claim(claimAt, "owner", 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	item := mustMarshalRecord(t, r)
	if _, ok := item["first_attempted_at"]; !ok {
		t.Fatal("claimed record must emit first_attempted_at")
	}
	if got := numAttrI64(item, "first_attempted_at"); got != claimAt.UnixMilli() {
		t.Fatalf("first_attempted_at millis = %d, want %d", got, claimAt.UnixMilli())
	}

	back, err := unmarshalRecord(item)
	if err != nil {
		t.Fatalf("unmarshalRecord: %v", err)
	}
	if !back.FirstAttemptedAt().Equal(claimAt) {
		t.Fatalf("round-trip first attempt = %v, want %v", back.FirstAttemptedAt(), claimAt)
	}
}

// TestNumAttrI64_MissingAttributeReturnsZero pins the legacy-safety invariant the
// claim/unmarshal path relies on: a missing numeric attribute reads back as 0,
// and timeFromMillis(0) is the zero time.
func TestNumAttrI64_MissingAttributeReturnsZero(t *testing.T) {
	empty := map[string]ddbtypes.AttributeValue{}
	if got := numAttrI64(empty, "first_attempted_at"); got != 0 {
		t.Fatalf("numAttrI64 on a missing attribute = %d, want 0", got)
	}
	if !timeFromMillis(0).IsZero() {
		t.Fatal("timeFromMillis(0) must be the zero time")
	}
}

// TestMarshalRecord_PendingWithExpiry_OmitsReapingTTL pins the durable-delivery
// finding that a still-pending record carrying an ExpiresAt must NOT be stamped
// with a DynamoDB item-ttl. The ttl is a physical-compaction convenience for
// TERMINAL records only (Complete/Expire stamp their own #ttl); stamping it on a
// pending record would let DynamoDB reap undelivered work — e.g. an
// on_expired=dlq record that expired during an egress outage, which the drainer
// defers to the claim path instead of sweeping. The record must still carry
// has_expiry so the sparse ExpiryIndex keeps finding it. memory/sqlite never
// evict pending rows; this keeps DynamoDB consistent and loss-free.
func TestMarshalRecord_PendingWithExpiry_OmitsReapingTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "exp-1", RouteID: "r", EnvelopeID: "e1", BindingID: "b1", SessionID: "s1", Address: "a",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t"}),
		ExpiresAt: now.Add(1 * time.Minute),
	})

	item := mustMarshalRecord(t, r)

	if _, ok := item["ttl"]; ok {
		t.Fatal("pending record with an expiry must NOT carry a reaping ttl; " +
			"DynamoDB would otherwise silently delete undelivered work during an outage")
	}
	if _, ok := item[attrHasExpiry]; !ok {
		t.Fatalf("pending record with an expiry must still carry %q for the ExpiryIndex", attrHasExpiry)
	}
	if got := numAttrI64(item, "expires_at"); got != r.ExpiresAt().UnixMilli() {
		t.Fatalf("expires_at millis = %d, want %d", got, r.ExpiresAt().UnixMilli())
	}
}

// TestSortKey_HistoricallyCollidingPairsAreDistinct is the c13-sk-collision
// regression. The RAW-concat sort key ("OUTBOX#"+env+"#"+binding) was NOT
// injective: within one partition the DISTINCT pairs (env="order",
// binding="eu#prod") and (env="order#eu", binding="prod") both marshaled to
// "OUTBOX#order#eu#prod", so the second record hit attribute_not_exists(SK),
// was mistaken for an idempotent duplicate, and was acked and DROPPED (silent
// data loss). Envelope IDs are producer-controlled, so a colliding '#'-bearing
// ID can arrive externally on any transport.
//
// Mutation this kills: reverting sortKey to `skPrefix + env + "#" + binding`
// makes the two keys EQUAL again → this test FAILs.
func TestSortKey_HistoricallyCollidingPairsAreDistinct(t *testing.T) {
	a := sortKey("order", "eu#prod")
	b := sortKey("order#eu", "prod")
	if a == b {
		t.Fatalf("distinct (env,binding) pairs must not collide: both marshaled to %q", a)
	}

	// A genuine duplicate — the SAME (env,binding) — must still collapse to the
	// same SK so attribute_not_exists(SK) keeps real redeliveries idempotent.
	if sortKey("order", "eu#prod") != a {
		t.Fatalf("identical (env,binding) must yield a stable SK for idempotency")
	}

	// The SK must still live under the OUTBOX# prefix so begins_with(SK, prefix)
	// record queries keep finding it and skip the FENCE row.
	for _, sk := range []string{a, b} {
		if !strings.HasPrefix(sk, skPrefix) {
			t.Fatalf("sort key %q must retain the %q prefix", sk, skPrefix)
		}
	}
}

// TestSortKey_RoundTripRecoversExactComponents proves the escaped SK is
// injective AND reversible: parseSortKey recovers the exact (env,binding) the
// key was built from, including components that themselves contain the '#'
// separator or the '%' escape marker.
func TestSortKey_RoundTripRecoversExactComponents(t *testing.T) {
	cases := []struct{ env, binding string }{
		{"order", "eu#prod"},
		{"order#eu", "prod"},
		{"plain-env", "plain-bind"},
		{"a#b#c", "d#e"},
		{"pct%25", "hash#and%pct"},
		{"%23", "%25"},
		{"", ""},
		{"#", "#"},
	}
	for _, tc := range cases {
		sk := sortKey(tc.env, tc.binding)
		gotEnv, gotBind, ok := parseSortKey(sk)
		if !ok {
			t.Fatalf("parseSortKey(%q) failed for (env=%q, binding=%q)", sk, tc.env, tc.binding)
		}
		if gotEnv != tc.env || gotBind != tc.binding {
			t.Fatalf("round-trip of %q: got (env=%q, binding=%q), want (env=%q, binding=%q)",
				sk, gotEnv, gotBind, tc.env, tc.binding)
		}
	}

	// The FENCE row's sort key has no record prefix/separator and must not
	// parse as a record key.
	if _, _, ok := parseSortKey(fenceSK); ok {
		t.Fatalf("parseSortKey must reject the non-record FENCE key %q", fenceSK)
	}
}

// TestMarshalRecord_SKIsInjectiveAcrossCollidingPairs pins the injectivity at
// the marshalRecord boundary (not just sortKey): two records that historically
// collided produce items with DISTINCT SK attributes, so a conditional
// attribute_not_exists(SK) Persist no longer drops the second as a duplicate.
func TestMarshalRecord_SKIsInjectiveAcrossCollidingPairs(t *testing.T) {
	mk := func(env, binding string) *persistence.OutboxRecord {
		return persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "c13-" + env + "-" + binding, RouteID: "r",
			EnvelopeID: env, BindingID: binding, SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: env, Subject: "t"}),
		})
	}
	itemA := mustMarshalRecord(t, mk("order", "eu#prod"))
	itemB := mustMarshalRecord(t, mk("order#eu", "prod"))

	if strAttr(itemA, "SK") == strAttr(itemB, "SK") {
		t.Fatalf("marshalRecord produced colliding SKs %q for distinct (env,binding) pairs",
			strAttr(itemA, "SK"))
	}
	// The separate authoritative attributes stay intact per record.
	if strAttr(itemA, "binding_id") != "eu#prod" || strAttr(itemB, "binding_id") != "prod" {
		t.Fatalf("binding_id attribute corrupted: A=%q B=%q",
			strAttr(itemA, "binding_id"), strAttr(itemB, "binding_id"))
	}
}
