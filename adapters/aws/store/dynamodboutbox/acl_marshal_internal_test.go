package dynamodboutbox

// Internal unit tests for the replay-budget first_attempted_at attribute. They
// run without DynamoDB Local (no build tag / no ddblocal client): they exercise
// the pure marshal/unmarshal ACL and the numeric-attribute helper directly, so
// the omit-when-zero and round-trip behaviour is covered even when `make test`
// runs in -short mode. The end-to-end claim stamp is covered by
// TestOutboxFirstAttemptConformance under the integration gate.

import (
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
	item, err := marshalRecord(r, time.Unix(1_700_000_000, 0), 0)
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
