package dynamodblease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	attrObservationFingerprint = "takeover_observation_fingerprint"
	attrObservationElapsed     = "takeover_observation_elapsed_ns"
	attrObservationGeneration  = "takeover_observation_generation"
)

type leaseTuple struct {
	owner            string
	ownerPresent     bool
	version          uint64
	versionPresent   bool
	renewedAt        int64
	renewedAtPresent bool
	expiresAt        int64
	expiresAtPresent bool
}

type observationEvidence struct {
	present     bool
	fingerprint string
	elapsed     time.Duration
	generation  uint64
}

func (r leaseRow) tuple() leaseTuple {
	return leaseTuple{
		owner:            r.owner,
		ownerPresent:     r.ownerPresent,
		version:          r.version,
		versionPresent:   r.versionPresent,
		renewedAt:        r.renewedAt,
		renewedAtPresent: r.renewedAtPresent,
		expiresAt:        r.expiresAt,
		expiresAtPresent: r.expiresAtPresent,
	}
}

func (t leaseTuple) equal(other leaseTuple) bool {
	return t == other
}

func (t leaseTuple) fingerprint() string {
	raw := fmt.Sprintf("owner:%t:%q|version:%t:%d|renewed:%t:%d|expires:%t:%d",
		t.ownerPresent, t.owner, t.versionPresent, t.version,
		t.renewedAtPresent, t.renewedAt, t.expiresAtPresent, t.expiresAt)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func requiredObservationDuration(row leaseRow) (time.Duration, error) {
	tuple := row.tuple()
	if !tuple.renewedAtPresent || !tuple.expiresAtPresent || tuple.renewedAt <= 0 || tuple.expiresAt <= tuple.renewedAt {
		return 0, shared.ErrInvalidConfig.WithMessage("dynamodblease: malformed lease liveness duration")
	}
	deltaMillis := tuple.expiresAt - tuple.renewedAt
	if deltaMillis > math.MaxInt64/int64(time.Millisecond) {
		return 0, shared.ErrInvalidConfig.WithMessage("dynamodblease: lease liveness duration overflows time.Duration")
	}
	return time.Duration(deltaMillis) * time.Millisecond, nil
}

func saturatingObservationDuration(current, delta time.Duration) time.Duration {
	if current < 0 || delta < 0 {
		return 0
	}
	if current > time.Duration(math.MaxInt64)-delta {
		return time.Duration(math.MaxInt64)
	}
	return current + delta
}

func decodeObservationEvidence(item map[string]ddbtypes.AttributeValue) (observationEvidence, error) {
	fpValue, fpOK := item[attrObservationFingerprint]
	elapsedValue, elapsedOK := item[attrObservationElapsed]
	generationValue, generationOK := item[attrObservationGeneration]
	if !fpOK && !elapsedOK && !generationOK {
		return observationEvidence{}, nil
	}
	if !fpOK || !elapsedOK || !generationOK {
		return observationEvidence{}, fmt.Errorf("dynamodblease: partial takeover observation evidence")
	}
	fp, ok := fpValue.(*ddbtypes.AttributeValueMemberS)
	if !ok || fp.Value == "" {
		return observationEvidence{}, fmt.Errorf("dynamodblease: invalid takeover observation fingerprint")
	}
	elapsedNumber, ok := elapsedValue.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return observationEvidence{}, fmt.Errorf("dynamodblease: takeover observation elapsed is not numeric")
	}
	elapsed, err := strconv.ParseInt(elapsedNumber.Value, 10, 64)
	if err != nil {
		return observationEvidence{}, fmt.Errorf("dynamodblease: invalid takeover observation elapsed: %w", err)
	}
	if elapsed < 0 {
		return observationEvidence{}, fmt.Errorf("dynamodblease: takeover observation elapsed must be non-negative")
	}
	generationNumber, ok := generationValue.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return observationEvidence{}, fmt.Errorf("dynamodblease: takeover observation generation is not numeric")
	}
	generation, err := strconv.ParseUint(generationNumber.Value, 10, 64)
	if err != nil {
		return observationEvidence{}, fmt.Errorf("dynamodblease: invalid takeover observation generation: %w", err)
	}
	if generation == 0 || generation == math.MaxUint64 {
		return observationEvidence{}, fmt.Errorf("dynamodblease: takeover observation generation must be positive and incrementable")
	}
	return observationEvidence{present: true, fingerprint: fp.Value, elapsed: time.Duration(elapsed), generation: generation}, nil
}

func nextObservationGeneration(current uint64, completesObservation bool) (uint64, error) {
	if current >= math.MaxUint64-1 || (current == math.MaxUint64-2 && !completesObservation) {
		return 0, shared.ErrInvalidConfig.WithMessage("dynamodblease: takeover observation generation overflow")
	}
	return current + 1, nil
}

func takeoverCondition(row leaseRow, includeEvidence bool) (string, map[string]string, map[string]ddbtypes.AttributeValue) {
	tuple := row.tuple()
	names := map[string]string{
		"#pk": attrPK, "#own": attrOwner, "#ver": attrVersion,
		"#ren": attrRenewedAt, "#exp": attrExpiresAt,
	}
	values := map[string]ddbtypes.AttributeValue{
		":tuple_pk": &ddbtypes.AttributeValueMemberS{Value: leaseKey(row.leaseID)},
	}
	clauses := []string{"#pk = :tuple_pk"}
	appendStringCondition := func(alias, valueAlias, value string, present bool) {
		if present {
			clauses = append(clauses, alias+" = "+valueAlias)
			values[valueAlias] = &ddbtypes.AttributeValueMemberS{Value: value}
		} else {
			clauses = append(clauses, "attribute_not_exists("+alias+")")
		}
	}
	appendNumberCondition := func(alias, valueAlias, value string, present bool) {
		if present {
			clauses = append(clauses, alias+" = "+valueAlias)
			values[valueAlias] = &ddbtypes.AttributeValueMemberN{Value: value}
		} else {
			clauses = append(clauses, "attribute_not_exists("+alias+")")
		}
	}
	appendStringCondition("#own", ":tuple_owner", tuple.owner, tuple.ownerPresent)
	appendNumberCondition("#ver", ":tuple_version", strconv.FormatUint(tuple.version, 10), tuple.versionPresent)
	appendNumberCondition("#ren", ":tuple_renewed", strconv.FormatInt(tuple.renewedAt, 10), tuple.renewedAtPresent)
	appendNumberCondition("#exp", ":tuple_expires", strconv.FormatInt(tuple.expiresAt, 10), tuple.expiresAtPresent)
	if includeEvidence {
		names["#obs_fp"] = attrObservationFingerprint
		names["#obs_elapsed"] = attrObservationElapsed
		names["#obs_gen"] = attrObservationGeneration
		evidence := row.observation
		appendStringCondition("#obs_fp", ":obs_fp", evidence.fingerprint, evidence.present)
		appendNumberCondition("#obs_elapsed", ":obs_elapsed", strconv.FormatInt(int64(evidence.elapsed), 10), evidence.present)
		appendNumberCondition("#obs_gen", ":obs_gen", strconv.FormatUint(evidence.generation, 10), evidence.present)
	}
	return strings.Join(clauses, " AND "), names, values
}

func (s *Store) writeObservationCAS(ctx context.Context, row leaseRow, next observationEvidence) error {
	condition, names, values := takeoverCondition(row, true)
	names["#obs_fp"] = attrObservationFingerprint
	names["#obs_elapsed"] = attrObservationElapsed
	names["#obs_gen"] = attrObservationGeneration
	values[":next_obs_fp"] = &ddbtypes.AttributeValueMemberS{Value: next.fingerprint}
	values[":next_obs_elapsed"] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(int64(next.elapsed), 10)}
	values[":next_obs_gen"] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatUint(next.generation, 10)}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.tableName,
		Key:                       map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: leaseKey(row.leaseID)}},
		ConditionExpression:       aws.String(condition),
		UpdateExpression:          aws.String("SET #obs_fp = :next_obs_fp, #obs_elapsed = :next_obs_elapsed, #obs_gen = :next_obs_gen"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		s.clearObservation(row.leaseID)
		if isConditionFailed(err) {
			return shared.ErrAlreadyExists.WithMessage("lease observation compare-and-set lost").With("leaseID", row.leaseID)
		}
		return wrapErr(err, "lease observation update failed", "leaseID", row.leaseID)
	}
	return nil
}
