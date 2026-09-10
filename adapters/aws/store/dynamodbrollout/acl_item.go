package dynamodbrollout

import (
	"fmt"
	"strconv"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	// DefaultTableName is the adapter default used when no table override is set.
	DefaultTableName = "gobridge-rollouts"

	// singletonPK is the fixed partition key of the single rollout row: a rollout
	// store holds exactly one active rollout at a time, so the
	// aggregate is one item, not one-per-generation. A new generation overwrites
	// the row (guarded by the revision counter), so history is not retained —
	// matching the in-memory reference store's single-slot semantics.
	singletonPK = "ROLLOUT#current"

	attrPK           = "PK"
	attrRev          = "rev"
	attrGeneration   = "generation"
	attrState        = "state"
	attrConfigDigest = "config_digest"
	attrConfigVer    = "config_version"
	attrEpoch        = "epoch"
	attrAcks         = "acks"
	attrNacks        = "nacks"
	attrReason       = "reason"
	attrDeadline     = "deadline_ms"
	attrCoordVersion = "coord_version"

	// Confirm window (ADR 0014). All three are absent for a base-protocol
	// rollout; confirm_deadline_ms and converged are also absent pre-commit.
	attrConfirmWindowMs   = "confirm_window_ms"
	attrConfirmDeadlineMs = "confirm_deadline_ms"
	attrConverged         = "converged"

	// ack sub-attributes inside the per-member acks map value.
	ackDigest = "d"
	ackAtMs   = "t"
)

// rolloutItem serializes a snapshot plus its revision counter into a DynamoDB
// item. Empty maps and an empty abort reason are omitted (absent == empty on
// read), which also sidesteps emulator quirks around empty M attributes.
func rolloutItem(snap persistence.RolloutSnapshot, rev uint64) map[string]ddbtypes.AttributeValue {
	item := map[string]ddbtypes.AttributeValue{
		attrPK:           sAttr(singletonPK),
		attrRev:          nUintAttr(rev),
		attrGeneration:   nUintAttr(snap.Generation),
		attrState:        sAttr(string(snap.State)),
		attrConfigDigest: sAttr(snap.ConfigDigest),
		attrConfigVer:    nAttr(int64(snap.ConfigVersion)),
		attrEpoch:        epochAttr(snap.MembershipEpoch),
		attrDeadline:     nAttr(snap.Deadline.UnixMilli()),
		attrCoordVersion: nUintAttr(snap.CoordinatorVersion),
	}
	if len(snap.Acks) > 0 {
		item[attrAcks] = acksAttr(snap.Acks)
	}
	if len(snap.Nacks) > 0 {
		item[attrNacks] = nacksAttr(snap.Nacks)
	}
	if snap.Reason != "" {
		item[attrReason] = sAttr(snap.Reason)
	}
	if snap.ConfirmWindow > 0 {
		item[attrConfirmWindowMs] = nAttr(snap.ConfirmWindow.Milliseconds())
	}
	if !snap.ConfirmDeadline.IsZero() {
		item[attrConfirmDeadlineMs] = nAttr(snap.ConfirmDeadline.UnixMilli())
	}
	if len(snap.Converged) > 0 {
		item[attrConverged] = convergedAttr(snap.Converged)
	}
	return item
}

// decodeRolloutItem is the fail-closed deserialization boundary: any missing,
// mistyped, or malformed attribute yields a corrupt-row error (ErrInvalidConfig)
// rather than a partially-zeroed aggregate a coordinator could act on. Returns
// the snapshot and its revision counter.
func decodeRolloutItem(item map[string]ddbtypes.AttributeValue) (persistence.RolloutSnapshot, uint64, error) {
	pk, err := reqString(item, attrPK)
	if err != nil || pk != singletonPK {
		return persistence.RolloutSnapshot{}, 0, corruptRow("missing or mismatched partition key")
	}
	rev, err := reqUint(item, attrRev)
	if err != nil || rev == 0 {
		return persistence.RolloutSnapshot{}, 0, corruptRow("revision must be present and positive")
	}
	gen, err := reqUint(item, attrGeneration)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("generation: " + err.Error())
	}
	state, err := reqString(item, attrState)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("state: " + err.Error())
	}
	digest, err := reqString(item, attrConfigDigest)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("config_digest: " + err.Error())
	}
	cfgVer, err := reqInt64(item, attrConfigVer)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("config_version: " + err.Error())
	}
	deadlineMs, err := reqInt64(item, attrDeadline)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("deadline_ms: " + err.Error())
	}
	coordVer, err := reqUint(item, attrCoordVersion)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("coord_version: " + err.Error())
	}
	epoch, err := decodeEpoch(item)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, err
	}
	acks, err := decodeAcks(item)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, err
	}
	nacks, err := decodeNacks(item)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, err
	}
	reason, err := optString(item, attrReason)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("reason: " + err.Error())
	}
	confirmWindowMs, err := optInt64(item, attrConfirmWindowMs)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("confirm_window_ms: " + err.Error())
	}
	confirmDeadlineMs, hasConfirmDeadline, err := optInt64Present(item, attrConfirmDeadlineMs)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, corruptRow("confirm_deadline_ms: " + err.Error())
	}
	converged, err := decodeConverged(item)
	if err != nil {
		return persistence.RolloutSnapshot{}, 0, err
	}
	confirmDeadline := time.Time{}
	if hasConfirmDeadline {
		confirmDeadline = time.UnixMilli(confirmDeadlineMs).UTC()
	}

	return persistence.RolloutSnapshot{
		Generation:         gen,
		State:              persistence.RolloutState(state),
		ConfigDigest:       digest,
		ConfigVersion:      int(cfgVer),
		MembershipEpoch:    epoch,
		Acks:               acks,
		Nacks:              nacks,
		Reason:             reason,
		Deadline:           time.UnixMilli(deadlineMs).UTC(),
		CoordinatorVersion: coordVer,
		ConfirmWindow:      time.Duration(confirmWindowMs) * time.Millisecond,
		ConfirmDeadline:    confirmDeadline,
		Converged:          converged,
	}, rev, nil
}

// convergedAttr serializes the convergence set as a member→at-millis map, mirroring
// nacksAttr's shape (a member→value map). Only the timestamp is carried; the member
// id is the key.
func convergedAttr(converged map[string]persistence.RolloutConverged) *ddbtypes.AttributeValueMemberM {
	m := make(map[string]ddbtypes.AttributeValue, len(converged))
	for member, c := range converged {
		m[member] = nAttr(c.At.UnixMilli())
	}
	return &ddbtypes.AttributeValueMemberM{Value: m}
}

func decodeConverged(item map[string]ddbtypes.AttributeValue) (map[string]persistence.RolloutConverged, error) {
	v, ok := item[attrConverged]
	if !ok {
		return map[string]persistence.RolloutConverged{}, nil // absent == none converged yet
	}
	m, ok := v.(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil, corruptRow("converged is not a map")
	}
	converged := make(map[string]persistence.RolloutConverged, len(m.Value))
	for member, av := range m.Value {
		n, ok := av.(*ddbtypes.AttributeValueMemberN)
		if !ok {
			return nil, corruptRow("converged entry is not a number")
		}
		atMs, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil {
			return nil, corruptRow("converged timestamp: " + err.Error())
		}
		converged[member] = persistence.RolloutConverged{MemberID: member, At: time.UnixMilli(atMs).UTC()}
	}
	return converged, nil
}

func epochAttr(members []string) *ddbtypes.AttributeValueMemberL {
	l := make([]ddbtypes.AttributeValue, len(members))
	for i, m := range members {
		l[i] = sAttr(m)
	}
	return &ddbtypes.AttributeValueMemberL{Value: l}
}

func decodeEpoch(item map[string]ddbtypes.AttributeValue) ([]string, error) {
	v, ok := item[attrEpoch]
	if !ok {
		return nil, corruptRow("membership epoch is missing")
	}
	l, ok := v.(*ddbtypes.AttributeValueMemberL)
	if !ok {
		return nil, corruptRow("membership epoch is not a list")
	}
	epoch := make([]string, len(l.Value))
	for i, e := range l.Value {
		s, ok := e.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return nil, corruptRow("membership epoch has a non-string member")
		}
		epoch[i] = s.Value
	}
	return epoch, nil
}

func acksAttr(acks map[string]persistence.RolloutAck) *ddbtypes.AttributeValueMemberM {
	m := make(map[string]ddbtypes.AttributeValue, len(acks))
	for member, ack := range acks {
		m[member] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			ackDigest: sAttr(ack.BuildDigest),
			ackAtMs:   nAttr(ack.At.UnixMilli()),
		}}
	}
	return &ddbtypes.AttributeValueMemberM{Value: m}
}

func decodeAcks(item map[string]ddbtypes.AttributeValue) (map[string]persistence.RolloutAck, error) {
	v, ok := item[attrAcks]
	if !ok {
		return map[string]persistence.RolloutAck{}, nil // absent == no acks yet
	}
	m, ok := v.(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil, corruptRow("acks is not a map")
	}
	acks := make(map[string]persistence.RolloutAck, len(m.Value))
	for member, av := range m.Value {
		entry, ok := av.(*ddbtypes.AttributeValueMemberM)
		if !ok {
			return nil, corruptRow("ack entry is not a map")
		}
		digest, err := reqString(entry.Value, ackDigest)
		if err != nil {
			return nil, corruptRow("ack digest: " + err.Error())
		}
		atMs, err := reqInt64(entry.Value, ackAtMs)
		if err != nil {
			return nil, corruptRow("ack timestamp: " + err.Error())
		}
		acks[member] = persistence.RolloutAck{
			MemberID:    member,
			BuildDigest: digest,
			At:          time.UnixMilli(atMs).UTC(),
		}
	}
	return acks, nil
}

func nacksAttr(nacks map[string]string) *ddbtypes.AttributeValueMemberM {
	m := make(map[string]ddbtypes.AttributeValue, len(nacks))
	for member, reason := range nacks {
		m[member] = sAttr(reason)
	}
	return &ddbtypes.AttributeValueMemberM{Value: m}
}

func decodeNacks(item map[string]ddbtypes.AttributeValue) (map[string]string, error) {
	v, ok := item[attrNacks]
	if !ok {
		return map[string]string{}, nil // absent == no nacks
	}
	m, ok := v.(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil, corruptRow("nacks is not a map")
	}
	nacks := make(map[string]string, len(m.Value))
	for member, av := range m.Value {
		s, ok := av.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return nil, corruptRow("nack reason is not a string")
		}
		nacks[member] = s.Value
	}
	return nacks, nil
}

// ── attribute primitives ───────────────────────────────────────────────────

func sAttr(v string) *ddbtypes.AttributeValueMemberS {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

func nAttr(v int64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

// nUintAttr serializes an unsigned counter (rev, generation, coordinator
// version) via FormatUint so a value above 2^63 is not narrowed through int64
// into a negative string that would wedge the row as corrupt on read-back.
func nUintAttr(v uint64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatUint(v, 10)}
}

func reqString(attrs map[string]ddbtypes.AttributeValue, key string) (string, error) {
	v, ok := attrs[key]
	if !ok {
		return "", fmt.Errorf("attribute %q is missing", key)
	}
	s, ok := v.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("attribute %q is not a string", key)
	}
	return s.Value, nil
}

// optString reads a string attribute that MAY be legitimately absent. Absent →
// ("", nil); present and string-typed → (value, nil); present but the WRONG type
// → ("", err), so a schema-drifted or buggy writer cannot silently blank the
// value — every attribute in decodeRolloutItem fails closed on a type mismatch.
func optString(attrs map[string]ddbtypes.AttributeValue, key string) (string, error) {
	v, ok := attrs[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("attribute %q is not a string", key)
	}
	return s.Value, nil
}

// optInt64 reads an int64 attribute that MAY be absent, returning 0 when absent.
// A present-but-wrong-typed attribute fails closed (a schema-drifted writer must
// not be silently read as zero).
func optInt64(attrs map[string]ddbtypes.AttributeValue, key string) (int64, error) {
	v, present, err := optInt64Present(attrs, key)
	if err != nil || !present {
		return 0, err
	}
	return v, nil
}

// optInt64Present is optInt64 that also reports whether the attribute was present,
// so a caller can distinguish an absent optional field from a stored zero (the
// confirm deadline, whose zero value is a meaningful "no window").
func optInt64Present(attrs map[string]ddbtypes.AttributeValue, key string) (int64, bool, error) {
	v, ok := attrs[key]
	if !ok {
		return 0, false, nil
	}
	n, ok := v.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, false, fmt.Errorf("attribute %q is not a number", key)
	}
	parsed, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("attribute %q is not a valid int64: %w", key, err)
	}
	return parsed, true, nil
}

func reqInt64(attrs map[string]ddbtypes.AttributeValue, key string) (int64, error) {
	v, ok := attrs[key]
	if !ok {
		return 0, fmt.Errorf("attribute %q is missing", key)
	}
	n, ok := v.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("attribute %q is not a number", key)
	}
	parsed, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("attribute %q is not a valid int64: %w", key, err)
	}
	return parsed, nil
}

func reqUint(attrs map[string]ddbtypes.AttributeValue, key string) (uint64, error) {
	v, ok := attrs[key]
	if !ok {
		return 0, fmt.Errorf("attribute %q is missing", key)
	}
	n, ok := v.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("attribute %q is not a number", key)
	}
	parsed, err := strconv.ParseUint(n.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("attribute %q is not a valid uint64: %w", key, err)
	}
	return parsed, nil
}

func corruptRow(detail string) error {
	return shared.ErrInvalidConfig.WithMessage("dynamodbrollout: corrupt rollout row: " + detail)
}
