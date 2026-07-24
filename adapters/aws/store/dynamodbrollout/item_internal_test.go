package dynamodbrollout

import (
	"errors"
	"maps"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func stagingSnapshot() persistence.RolloutSnapshot {
	return persistence.RolloutSnapshot{
		Generation:      7,
		State:           persistence.RolloutStaging,
		ConfigDigest:    "sha256:cafef00d",
		ConfigVersion:   42,
		MembershipEpoch: []string{"node-a", "node-b"},
		Acks: map[string]persistence.RolloutAck{
			"node-a": {MemberID: "node-a", BuildDigest: "build:a", At: time.UnixMilli(1_700_000_000_123)},
		},
		Nacks:              map[string]string{"node-b": "plugin missing"},
		Deadline:           time.UnixMilli(1_700_000_300_000),
		CoordinatorVersion: 0,
	}
}

// TestItemRoundTrip proves the DynamoDB item encoding is self-consistent: a
// snapshot survives rolloutItem -> decodeRolloutItem byte-for-byte, including
// the revision counter, the acks map, and the nacks map. This is the unit-level
// twin of the ddblocal conformance suite -- it pins the serialization without a
// live DynamoDB.
func TestItemRoundTrip(t *testing.T) {
	snap := stagingSnapshot()
	got, rev, err := decodeRolloutItem(rolloutItem(snap, 5))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rev != 5 {
		t.Fatalf("rev = %d, want 5", rev)
	}
	if got.Generation != snap.Generation || got.State != snap.State ||
		got.ConfigDigest != snap.ConfigDigest || got.ConfigVersion != snap.ConfigVersion ||
		got.CoordinatorVersion != snap.CoordinatorVersion {
		t.Fatalf("scalar mismatch: got %+v want %+v", got, snap)
	}
	if !got.Deadline.Equal(snap.Deadline) {
		t.Fatalf("deadline = %v, want %v", got.Deadline, snap.Deadline)
	}
	if len(got.MembershipEpoch) != 2 || got.MembershipEpoch[0] != "node-a" || got.MembershipEpoch[1] != "node-b" {
		t.Fatalf("epoch = %v", got.MembershipEpoch)
	}
	if !maps.Equal(got.Nacks, snap.Nacks) {
		t.Fatalf("nacks = %v, want %v", got.Nacks, snap.Nacks)
	}
	a, ok := got.Acks["node-a"]
	if !ok || a.BuildDigest != "build:a" || !a.At.Equal(snap.Acks["node-a"].At) {
		t.Fatalf("ack round-trip = %+v", a)
	}
}

// TestItemRoundTripEmptyMaps proves a Proposed rollout (no acks/nacks, empty
// reason) round-trips: the omit-empty encoding must decode back to empty maps,
// not a corrupt-row error.
func TestItemRoundTripEmptyMaps(t *testing.T) {
	snap := persistence.RolloutSnapshot{
		Generation:      1,
		State:           persistence.RolloutProposed,
		ConfigDigest:    "sha256:x",
		ConfigVersion:   1,
		MembershipEpoch: []string{"only"},
		Deadline:        time.UnixMilli(1_700_000_000_000),
	}
	got, _, err := decodeRolloutItem(rolloutItem(snap, 1))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Acks) != 0 || len(got.Nacks) != 0 || got.Reason != "" {
		t.Fatalf("empty maps not preserved: %+v", got)
	}
}

// TestItemRoundTripLargeUint64 proves the unsigned fields (generation, rev,
// coordinator version) survive values above 2^63 without narrowing through
// int64 — a signed cast would serialize them negative and wedge the row as
// permanently corrupt on read-back.
func TestItemRoundTripLargeUint64(t *testing.T) {
	const big = uint64(1)<<63 + 7
	snap := stagingSnapshot()
	snap.Generation = big
	got, rev, err := decodeRolloutItem(rolloutItem(snap, big))
	if err != nil {
		t.Fatalf("decode of large uint64 fields: %v", err)
	}
	if got.Generation != big {
		t.Fatalf("generation = %d, want %d", got.Generation, big)
	}
	if rev != big {
		t.Fatalf("rev = %d, want %d", rev, big)
	}
}

// TestDecodeRejectsCorruptItem proves the decode boundary fails closed
// (ErrInvalidConfig) on any malformed item, rather than returning a
// partially-zeroed snapshot a coordinator could act on.
func TestDecodeRejectsCorruptItem(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]ddbtypes.AttributeValue)
	}{
		{"missing_pk", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrPK) }},
		{"wrong_pk", func(it map[string]ddbtypes.AttributeValue) { it[attrPK] = sAttr("ROLLOUT#other") }},
		{"missing_rev", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrRev) }},
		{"zero_rev", func(it map[string]ddbtypes.AttributeValue) { it[attrRev] = nAttr(0) }},
		{"missing_generation", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrGeneration) }},
		{"non_numeric_generation", func(it map[string]ddbtypes.AttributeValue) { it[attrGeneration] = sAttr("seven") }},
		{"missing_state", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrState) }},
		{"missing_digest", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrConfigDigest) }},
		{"missing_deadline", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrDeadline) }},
		{"missing_epoch", func(it map[string]ddbtypes.AttributeValue) { delete(it, attrEpoch) }},
		{"epoch_not_list", func(it map[string]ddbtypes.AttributeValue) { it[attrEpoch] = sAttr("node-a") }},
		{"epoch_member_not_string", func(it map[string]ddbtypes.AttributeValue) {
			it[attrEpoch] = &ddbtypes.AttributeValueMemberL{Value: []ddbtypes.AttributeValue{nAttr(1)}}
		}},
		{"acks_not_map", func(it map[string]ddbtypes.AttributeValue) { it[attrAcks] = sAttr("nope") }},
		{"ack_entry_not_map", func(it map[string]ddbtypes.AttributeValue) {
			it[attrAcks] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"node-a": sAttr("x")}}
		}},
		{"ack_missing_digest", func(it map[string]ddbtypes.AttributeValue) {
			it[attrAcks] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
				"node-a": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{ackAtMs: nAttr(1)}},
			}}
		}},
		{"nack_not_string", func(it map[string]ddbtypes.AttributeValue) {
			it[attrNacks] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"node-b": nAttr(1)}}
		}},
		{"reason_wrong_type", func(it map[string]ddbtypes.AttributeValue) {
			it[attrReason] = nAttr(1) // present but not a string: must fail closed like every other attr
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := rolloutItem(stagingSnapshot(), 3)
			tc.mutate(item)
			if _, _, err := decodeRolloutItem(item); !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("decode of %s: err = %v, want ErrInvalidConfig", tc.name, err)
			}
		})
	}
}
