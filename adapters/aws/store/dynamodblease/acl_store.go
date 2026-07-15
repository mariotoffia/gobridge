package dynamodblease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

const (
	// DefaultTableName is the adapter default used when no table override is configured.
	DefaultTableName = "gobridge-leases"

	attrPK         = "PK"
	attrOwner      = "owner"
	attrVersion    = "version"
	attrAcquiredAt = "acquired_at"
	attrExpiresAt  = "expires_at"
	attrRenewedAt  = "renewed_at"
	attrEndpoints  = "endpoints"
	// attrTTL is the legacy DynamoDB TTL attribute. New code never WRITES it,
	// but every mutating operation issues `REMOVE #ttl` to strip any stale
	// value a pre-fix build may have stamped, so a TTL reaper can never delete
	// an actively-held lease row and reset its fencing version (see MF-1/J1).
	attrTTL = "ttl"
)

// Store implements ports.LeaseStore using DynamoDB conditional writes
// for fencing-safe lease management.
//
// Lease rows double as the monotonic fencing-counter store: the version
// attribute must survive forever so that a re-acquire after a release can
// only ever increment it. For this reason lease rows deliberately carry NO
// DynamoDB TTL attribute — enabling TTL on the lease table would delete a
// released row and reset its version to 1, breaking fencing-token
// monotonicity across the cluster.
//
// Table schema (configurable via WithTableName):
//
//	PK (String): "LEASE#<lease_id>" -- partition key, no sort key
//	owner, version, acquired_at, expires_at, renewed_at
type Store struct {
	client    dynamoAPI
	tableName string
	clk       clock.Clock
	logger    *slog.Logger

	// obsMu guards process-local monotonic baselines. Confirmed elapsed time is
	// persisted on the existing lease row and compare-and-set against the exact
	// liveness tuple plus evidence generation. The local map contains no
	// cross-process timestamps and is discarded after every failed read/write.

	obsMu        sync.Mutex
	observations map[string]leaseObservation

	// ttlPreflightAdvisory downgrades the build-time lease-table TTL invariant
	// check (Preflight) from FAIL-CLOSED to advisory: an OBSERVED enabled TTL, or
	// a DescribeTimeToLive that cannot verify the TTL state, becomes a loud WARN
	// instead of a fatal error. Default false = production fail-closed posture;
	// set only via WithTTLPreflightAdvisory (an explicit dev/emulator opt-out).
	ttlPreflightAdvisory bool
}

// leaseObservation is one process-local monotonic baseline for evidence read
// successfully from DynamoDB. Only elapsed time since observedAt may be added,
// and only when the next exact-tuple/evidence compare-and-set succeeds.
type leaseObservation struct {
	tuple      leaseTuple
	evidence   observationEvidence
	observedAt time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-leases").
func WithTableName(name string) Option {
	return func(s *Store) { s.tableName = name }
}

// WithGracePeriod is deprecated and now a no-op.
//
// Lease rows are the monotonic fencing-counter store and must never be
// TTL-deleted (see the Store doc comment). The lease store therefore no
// longer writes a TTL attribute and this option is retained only for
// backward compatibility with existing call sites.
//
// Deprecated: lease rows carry no TTL; this option has no effect.
func WithGracePeriod(_ time.Duration) Option {
	return func(*Store) {}
}

// WithClock overrides the clock used for timestamps.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithLogger sets the structured logger for trace/debug diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// WithTTLPreflightAdvisory downgrades the build-time lease-table TTL invariant
// check from FAIL-CLOSED (the production default) to advisory. With it set, an
// OBSERVED enabled (or enabling) DynamoDB TTL on the lease table — and a
// DescribeTimeToLive call that cannot verify the TTL state — is logged as a loud
// WARN and Preflight returns nil instead of a fatal error.
//
// This is an EXPLICIT dev/emulator opt-out ONLY. Leave it unset in production:
// the lease row IS the monotonic fencing counter of record, and a TTL reaper
// deleting a fence row resets its version to 1 while the outbox high-water mark
// sits at v≫1 — every subsequent claim then fails ErrStaleFencingToken and the
// partition splits/stalls. An inability to VERIFY the TTL state is likewise NOT
// proof it is disabled, so it fails closed too (mirroring the base store's
// WithSchemaPreflightAdvisory discipline).
func WithTTLPreflightAdvisory() Option {
	return func(s *Store) { s.ttlPreflightAdvisory = true }
}

// NewStore creates a new DynamoDB-backed LeaseStore.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL
// constructor exists to wrap; it is injected by the composition root and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:    client,
		tableName: DefaultTableName,
		clk:       clock.System,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Acquire attempts to obtain a lease. It first tries a fresh acquire via PutItem
// with attribute_not_exists. If the item already exists it inspects the row:
//
//   - A RELEASED lease (empty owner) or an explicitly zeroed expiry is seized
//     immediately with a version-fenced UpdateItem. This is skew-immune: no
//     wall-clock comparison is involved.
//
//   - An ACTIVELY-HELD lease whose owner is THIS ownerID (a crashed-and-
//     restarted node whose row still names it) is seized immediately, fenced on
//     the current exact tuple and observation evidence. A node cannot race
//     itself; duplicate ownerIDs across nodes are a fatal misconfiguration.
//     The observation window is therefore skipped.
//
//   - An ACTIVELY-HELD lease held by a DIFFERENT owner is observed through
//     persisted unchanged-tuple evidence on this same lease row. Each observer
//     may CAS-add only local monotonic elapsed time since its successful read.
//     The CAS includes PK, owner, version, renewed_at, expires_at, fingerprint,
//     elapsed value, and generation, so competing observers cannot double-count
//     overlapping intervals. A replacement process inherits confirmed elapsed
//     evidence without subtracting timestamps written by another host.
//
// Renewal, release, takeover, and fresh ownership atomically clear evidence.
// Foreign or legacy tuple mutation makes the fingerprint differ and resets the
// evidence to zero through an exact-row CAS. Malformed evidence fails closed.
// DynamoDB TTL remains disabled because this row is also the fencing counter.
func (s *Store) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if ttl <= 0 {
		return persistence.LeaseToken{}, shared.ErrInvalidConfig.WithMessage("dynamodblease: lease TTL must be positive")
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodblease: acquire", "lease_id", leaseID, "owner_id", ownerID)
	}

	now := s.clk.Now()
	expiresAt := now.Add(ttl)
	pk := leaseKey(leaseID)

	item := map[string]ddbtypes.AttributeValue{
		attrPK:         &ddbtypes.AttributeValueMemberS{Value: pk},
		attrOwner:      &ddbtypes.AttributeValueMemberS{Value: ownerID},
		attrVersion:    &ddbtypes.AttributeValueMemberN{Value: "1"},
		attrAcquiredAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
		attrExpiresAt:  &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
		attrRenewedAt:  &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
	}
	if len(endpoints) > 0 {
		item[attrEndpoints] = marshalEndpoints(endpoints)
	}

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &s.tableName,
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{
			"#pk": attrPK,
		},
	})
	if err == nil {
		// We own it now; discard any takeover observation.
		s.clearObservation(leaseID)
		return persistence.LeaseToken{Version: 1, Owner: ownerID}, nil
	}
	if !isConditionFailed(err) {
		s.clearObservation(leaseID)
		return persistence.LeaseToken{}, wrapErr(err, "", "leaseID", leaseID, "ownerID", ownerID)
	}

	// Item exists. Read it (strongly consistent) to choose the takeover path.
	row, err := s.getRow(ctx, leaseID)
	if err != nil {
		return persistence.LeaseToken{}, wrapErr(err, "lease takeover read failed", "leaseID", leaseID, "ownerID", ownerID)
	}
	if !row.present {
		s.clearObservation(leaseID)
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease row disappeared during takeover read").
			With("leaseID", leaseID)
	}

	if row.owner == "" || row.expiresAt <= 0 {
		condition, names, values := takeoverCondition(row, true)
		token, takeoverErr := s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt,
			condition, names, values)
		if takeoverErr != nil {
			s.clearObservation(leaseID)
			return persistence.LeaseToken{}, takeoverErr
		}
		s.clearObservation(leaseID)
		return token, nil
	}

	return s.observeOrSeize(ctx, leaseID, ownerID, ttl, endpoints, now, expiresAt, row)
}

// observeOrSeize advances persisted monotonic observation evidence and takes
// over an actively-held lease only after a full unchanged-tuple duration.
func (s *Store) observeOrSeize(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string, now, expiresAt time.Time, row leaseRow) (persistence.LeaseToken, error) {
	row.leaseID = leaseID
	if row.owner == ownerID {
		condition, names, values := takeoverCondition(row, true)
		token, err := s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt, condition, names, values)
		if err != nil {
			s.clearObservation(leaseID)
			return persistence.LeaseToken{}, err
		}
		s.clearObservation(leaseID)
		return token, nil
	}

	required, err := requiredObservationDuration(row, ttl)
	if err != nil {
		s.clearObservation(leaseID)
		return persistence.LeaseToken{}, err
	}
	tuple := row.tuple()
	fingerprint := tuple.fingerprint()

	if !row.observation.present || row.observation.fingerprint != fingerprint {
		generation, generationErr := nextObservationGeneration(row.observation.generation)
		if generationErr != nil {
			s.clearObservation(leaseID)
			return persistence.LeaseToken{}, generationErr
		}
		next := observationEvidence{present: true, fingerprint: fingerprint, generation: generation}
		if err := s.writeObservationCAS(ctx, row, next); err != nil {
			return persistence.LeaseToken{}, err
		}
		s.recordObservation(leaseID, leaseObservation{tuple: tuple, evidence: next, observedAt: now})
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; persisted takeover observation started").With("leaseID", leaseID)
	}

	if row.observation.elapsed >= required {
		condition, names, values := takeoverCondition(row, true)
		token, takeoverErr := s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt, condition, names, values)
		if takeoverErr != nil {
			s.clearObservation(leaseID)
			return persistence.LeaseToken{}, takeoverErr
		}
		s.clearObservation(leaseID)
		return token, nil
	}

	local, ok := s.observationFor(leaseID)
	if !ok || !local.tuple.equal(tuple) || local.evidence != row.observation {
		s.recordObservation(leaseID, leaseObservation{tuple: tuple, evidence: row.observation, observedAt: now})
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; takeover observation baseline refreshed").With("leaseID", leaseID)
	}

	delta := now.Sub(local.observedAt)
	if delta < 0 {
		s.recordObservation(leaseID, leaseObservation{tuple: tuple, evidence: row.observation, observedAt: now})
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; local observation clock regressed").With("leaseID", leaseID)
	}
	if delta == 0 {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; no new takeover observation elapsed").With("leaseID", leaseID)
	}

	generation, generationErr := nextObservationGeneration(row.observation.generation)
	if generationErr != nil {
		s.clearObservation(leaseID)
		return persistence.LeaseToken{}, generationErr
	}
	next := observationEvidence{
		present: true, fingerprint: fingerprint,
		elapsed:    saturatingObservationDuration(row.observation.elapsed, delta),
		generation: generation,
	}
	if err := s.writeObservationCAS(ctx, row, next); err != nil {
		return persistence.LeaseToken{}, err
	}
	s.recordObservation(leaseID, leaseObservation{tuple: tuple, evidence: next, observedAt: now})

	if next.elapsed < required {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; persisted takeover observation incomplete").With("leaseID", leaseID)
	}
	row.observation = next
	condition, names, values := takeoverCondition(row, true)
	token, takeoverErr := s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt, condition, names, values)
	if takeoverErr != nil {
		s.clearObservation(leaseID)
		return persistence.LeaseToken{}, takeoverErr
	}
	s.clearObservation(leaseID)
	return token, nil
}

// runTakeover issues the version-incrementing takeover UpdateItem shared by the
// released-lease and observed-lease paths. The caller supplies the fencing
// condition and any extra names/values it references; runTakeover appends the
// standard SET clause, endpoints, and the legacy-ttl strip.
func (s *Store) runTakeover(ctx context.Context, leaseID, ownerID string, endpoints map[string]string, now, expiresAt time.Time, cond string, extraNames map[string]string, extraValues map[string]ddbtypes.AttributeValue) (persistence.LeaseToken, error) {
	pk := leaseKey(leaseID)

	updateExpr := "SET #own = :owner, #ver = if_not_exists(#ver, :zero) + :one, #acq = :now_ms, #exp = :exp_ms, #ren = :now_ms"
	exprNames := map[string]string{
		"#own": attrOwner,
		"#ver": attrVersion,
		"#acq": attrAcquiredAt,
		"#exp": attrExpiresAt,
		"#ren": attrRenewedAt,
	}
	exprValues := map[string]ddbtypes.AttributeValue{
		":owner":  &ddbtypes.AttributeValueMemberS{Value: ownerID},
		":one":    &ddbtypes.AttributeValueMemberN{Value: "1"},
		":zero":   &ddbtypes.AttributeValueMemberN{Value: "0"},
		":now_ms": &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
		":exp_ms": &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
	}
	for k, v := range extraNames {
		exprNames[k] = v
	}
	for k, v := range extraValues {
		exprValues[k] = v
	}
	if len(endpoints) > 0 {
		updateExpr += ", #ep = :ep"
		exprNames["#ep"] = attrEndpoints
		exprValues[":ep"] = marshalEndpoints(endpoints)
	}
	// Takeover starts a new ownership tuple and clears all observation evidence.
	updateExpr += " REMOVE #ttl, #obs_fp, #obs_elapsed, #obs_gen"
	exprNames["#ttl"] = attrTTL
	exprNames["#obs_fp"] = attrObservationFingerprint
	exprNames["#obs_elapsed"] = attrObservationElapsed
	exprNames["#obs_gen"] = attrObservationGeneration

	result, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConditionExpression:       aws.String(cond),
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
		ReturnValues:              ddbtypes.ReturnValueAllNew,
	})
	if err != nil {
		if isConditionFailed(err) {
			return persistence.LeaseToken{}, shared.ErrAlreadyExists.
				WithMessage("lease already held").
				With("leaseID", leaseID)
		}
		return persistence.LeaseToken{}, wrapErr(err, "lease takeover update failed", "leaseID", leaseID, "ownerID", ownerID)
	}

	ver, err := numAttr(result.Attributes, attrVersion)
	if err != nil {
		return persistence.LeaseToken{}, fmt.Errorf("dynamodblease: parse version from takeover result: %w", err)
	}
	return persistence.LeaseToken{Version: ver, Owner: ownerID}, nil
}

// leaseRow is a decoded snapshot of the stored lease item used by the takeover
// decision. expiresAt/renewedAt are epoch millis.
type leaseRow struct {
	leaseID          string
	present          bool
	owner            string
	ownerPresent     bool
	version          uint64
	versionPresent   bool
	expiresAt        int64
	expiresAtPresent bool
	renewedAt        int64
	renewedAtPresent bool
	observation      observationEvidence
}

// getRow reads the lease item with a strongly consistent read and decodes the
// exact tuple and persisted takeover observation evidence.
func (s *Store) getRow(ctx context.Context, leaseID string) (leaseRow, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: leaseKey(leaseID)},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		s.clearObservation(leaseID)
		return leaseRow{}, err
	}
	if len(result.Item) == 0 {
		return leaseRow{leaseID: leaseID, present: false}, nil
	}
	owner, ownerPresent, err := optionalStringAttr(result.Item, attrOwner)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	version, versionPresent, err := optionalUintAttr(result.Item, attrVersion)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	expiresAt, expiresAtPresent, err := optionalInt64Attr(result.Item, attrExpiresAt)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	renewedAt, renewedAtPresent, err := optionalInt64Attr(result.Item, attrRenewedAt)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	observation, err := decodeObservationEvidence(result.Item)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	return leaseRow{
		leaseID: leaseID, present: true,
		owner: owner, ownerPresent: ownerPresent,
		version: version, versionPresent: versionPresent,
		expiresAt: expiresAt, expiresAtPresent: expiresAtPresent,
		renewedAt: renewedAt, renewedAtPresent: renewedAtPresent,
		observation: observation,
	}, nil
}

func (s *Store) observationFor(leaseID string) (leaseObservation, bool) {
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	obs, ok := s.observations[leaseID]
	return obs, ok
}

func (s *Store) recordObservation(leaseID string, obs leaseObservation) {
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	if s.observations == nil {
		s.observations = make(map[string]leaseObservation)
	}
	s.observations[leaseID] = obs
}

func (s *Store) clearObservation(leaseID string) {
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	delete(s.observations, leaseID)
}

// Renew extends the lease TTL. The caller's token must match the stored
// owner and version. The returned token keeps the same version.
func (s *Store) Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodblease: renew", "lease_id", leaseID, "owner_id", token.Owner)
	}

	now := s.clk.Now()
	expiresAt := now.Add(ttl)
	pk := leaseKey(leaseID)

	updateExpr := "SET #exp = :exp_ms, #ren = :now_ms"
	exprNames := map[string]string{
		"#own": attrOwner,
		"#ver": attrVersion,
		"#exp": attrExpiresAt,
		"#ren": attrRenewedAt,
	}
	exprValues := map[string]ddbtypes.AttributeValue{
		":owner":  &ddbtypes.AttributeValueMemberS{Value: token.Owner},
		":ver":    &ddbtypes.AttributeValueMemberN{Value: uintStr(token.Version)},
		":exp_ms": &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
		":now_ms": &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
	}
	if len(endpoints) > 0 {
		updateExpr += ", #ep = :ep"
		exprNames["#ep"] = attrEndpoints
		exprValues[":ep"] = marshalEndpoints(endpoints)
	}
	// Renewal advances the liveness tuple and atomically resets takeover evidence.
	updateExpr += " REMOVE #ttl, #obs_fp, #obs_elapsed, #obs_gen"
	exprNames["#ttl"] = attrTTL
	exprNames["#obs_fp"] = attrObservationFingerprint
	exprNames["#obs_elapsed"] = attrObservationElapsed
	exprNames["#obs_gen"] = attrObservationGeneration

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConditionExpression:       aws.String("#own = :owner AND #ver = :ver AND #exp >= :now_ms"),
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		if isConditionFailed(err) {
			return persistence.LeaseToken{}, s.classifyConditionFailure(ctx, leaseID)
		}
		return persistence.LeaseToken{}, wrapErr(err, "lease renew update failed", "leaseID", leaseID, "ownerID", token.Owner)
	}
	s.clearObservation(leaseID)
	return token, nil
}

// Release marks the lease as released by clearing the owner and setting
// expires_at to zero. The item is preserved so that the version counter
// remains available for monotonic increments on subsequent acquires.
//
// Released lease rows are deliberately NOT given a TTL: they are the
// monotonic fencing-counter of record and must never be deleted, or a
// subsequent fresh acquire would reset the version to 1 and break
// fencing-token monotonicity across the cluster.
func (s *Store) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodblease: release", "lease_id", leaseID)
	}

	pk := leaseKey(leaseID)

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConditionExpression: aws.String("#own = :owner AND #ver = :ver"),
		UpdateExpression: aws.String(
			"SET #own = :empty, #exp = :zero REMOVE #ttl, #obs_fp, #obs_elapsed, #obs_gen"),
		ExpressionAttributeNames: map[string]string{
			"#own":         attrOwner,
			"#ver":         attrVersion,
			"#exp":         attrExpiresAt,
			"#ttl":         attrTTL,
			"#obs_fp":      attrObservationFingerprint,
			"#obs_elapsed": attrObservationElapsed,
			"#obs_gen":     attrObservationGeneration,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: token.Owner},
			":ver":   &ddbtypes.AttributeValueMemberN{Value: uintStr(token.Version)},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		},
	})
	if err != nil {
		if isConditionFailed(err) {
			return s.classifyConditionFailure(ctx, leaseID)
		}
		return wrapErr(err, "lease release update failed", "leaseID", leaseID, "ownerID", token.Owner)
	}
	s.clearObservation(leaseID)
	return nil
}

// Current reads the lease state with a strongly consistent read.
func (s *Store) Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error) {
	pk := leaseKey(leaseID)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return persistence.LeaseInfo{}, wrapErr(err, "lease get failed", "leaseID", leaseID)
	}
	owner := strAttr(result.Item, attrOwner)
	if len(result.Item) == 0 || owner == "" {
		return persistence.LeaseInfo{}, shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	// Surface a corrupt (present-but-unparseable) fencing version instead of
	// silently coercing it to 0 — see optionalNumAttr / getRow.
	version, err := optionalNumAttr(result.Item, attrVersion)
	if err != nil {
		return persistence.LeaseInfo{}, wrapErr(err, "lease get failed", "leaseID", leaseID)
	}
	expiresAtMillis, err := optionalNumAttr(result.Item, attrExpiresAt)
	if err != nil {
		return persistence.LeaseInfo{}, wrapErr(err, "lease get failed", "leaseID", leaseID)
	}

	return persistence.LeaseInfo{
		LeaseID:   leaseID,
		Owner:     owner,
		Version:   version,
		ExpiresAt: time.UnixMilli(int64(expiresAtMillis)),
		Endpoints: unmarshalEndpoints(result.Item),
	}, nil
}

// EnsureTable creates the DynamoDB table if it does not already exist.
// Intended for test setup and local development.
func (s *Store) EnsureTable(ctx context.Context) error {
	_, err := s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: &s.tableName,
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(attrPK), KeyType: ddbtypes.KeyTypeHash},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(attrPK), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		var inUse *ddbtypes.ResourceInUseException
		if errors.As(err, &inUse) {
			// Table already exists (possibly from an older build). It is the
			// fencing counter of record: DynamoDB TTL MUST be disabled on it.
			s.warnIfTTLEnabled(ctx)
			return nil
		}
		return wrapErr(err, "create lease table failed", "table", s.tableName)
	}

	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: &s.tableName,
	}, 30*time.Second); err != nil {
		return wrapErr(err, "wait for lease table to exist failed", "table", s.tableName)
	}
	return nil
}

// warnIfTTLEnabled logs a loud warning when DynamoDB TTL is ENABLED (or
// enabling) on the lease table. The lease table is the monotonic fencing
// counter: a reaper deleting a released — or, with a stale legacy ttl, an
// actively-held — row would reset its version to 1 and break fencing-token
// monotonicity (split-brain / duplicate commits). This is a preflight
// safeguard for operators upgrading from a build that wrote a ttl attribute;
// it never fails EnsureTable (the DescribeTimeToLive call itself may be
// unsupported on some emulators).
//
// INTENTIONAL asymmetry: this EnsureTable (provisioning / dev-and-test setup)
// path stays WARN-only, whereas the build-time Preflight (checkLeaseTableTTL) is
// FATAL by default for the same enabled TTL. EnsureTable is a best-effort
// convenience that may run against an emulator; Preflight is the production
// admission gate that must fail closed on the split-brain hazard.
func (s *Store) warnIfTTLEnabled(ctx context.Context) {
	if s.logger == nil {
		return
	}
	out, err := s.client.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{
		TableName: &s.tableName,
	})
	if err != nil || out.TimeToLiveDescription == nil {
		return
	}
	switch out.TimeToLiveDescription.TimeToLiveStatus {
	case ddbtypes.TimeToLiveStatusEnabled, ddbtypes.TimeToLiveStatusEnabling:
		s.logger.Warn(
			"dynamodblease: DynamoDB TTL is ENABLED on the lease table; it is the "+
				"fencing counter of record and TTL MUST be DISABLED or fencing-token "+
				"monotonicity will break. Disable table TTL immediately.",
			"table", s.tableName,
			"ttl_status", string(out.TimeToLiveDescription.TimeToLiveStatus),
		)
	}
}

// classifyConditionFailure distinguishes between "item not found" and
// "item exists but token doesn't match" after a ConditionalCheckFailedException.
func (s *Store) classifyConditionFailure(ctx context.Context, leaseID string) error {
	pk := leaseKey(leaseID)
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return shared.ErrStaleFencingToken.
			WithMessage("lease token mismatch (follow-up read failed)").
			With("leaseID", leaseID).
			Wrap(err)
	}
	// Treat missing items and released items (empty owner) as not found.
	if len(result.Item) == 0 || strAttr(result.Item, attrOwner) == "" {
		return shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}
	return shared.ErrStaleFencingToken.
		WithMessage("lease token mismatch").
		With("leaseID", leaseID)
}

func leaseKey(leaseID string) string {
	return "LEASE#" + leaseID
}

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func millisStr(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func uintStr(n uint64) string {
	return strconv.FormatUint(n, 10)
}

func numAttr(attrs map[string]ddbtypes.AttributeValue, key string) (uint64, error) {
	v, ok := attrs[key]
	if !ok {
		return 0, fmt.Errorf("attribute %q not found", key)
	}
	n, ok := v.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("attribute %q is not a number", key)
	}
	parsed, err := strconv.ParseUint(n.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dynamodblease: parse number attribute %q: %w", key, err)
	}
	return parsed, nil
}

// optionalNumAttr reads a numeric attribute that MAY be legitimately absent.
//
//   - absent            → (0, nil): tolerated (e.g. renewed_at on a freshly
//     acquired lease, or older rows written before a field existed).
//   - present & corrupt → (0, err): surfaced, NOT coerced to 0.
//
// The distinction is load-bearing for the fencing version: lease rows ARE the
// fencing counter of record, so silently reading a corrupt version as 0 would
// reset the fence below the outbox high-water mark and make every subsequent
// claim fail with ErrStaleFencingToken — a partition-wide stall. A corrupt
// fence must fail loudly at the read, not masquerade as a fresh lease.
func optionalUintAttr(attrs map[string]ddbtypes.AttributeValue, key string) (uint64, bool, error) {
	if _, ok := attrs[key]; !ok {
		return 0, false, nil
	}
	value, err := numAttr(attrs, key)
	return value, true, err
}

func optionalInt64Attr(attrs map[string]ddbtypes.AttributeValue, key string) (int64, bool, error) {
	value, ok := attrs[key]
	if !ok {
		return 0, false, nil
	}
	number, ok := value.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, true, fmt.Errorf("attribute %q is not a number", key)
	}
	parsed, err := strconv.ParseInt(number.Value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, true, fmt.Errorf("dynamodblease: parse non-negative number attribute %q: %w", key, err)
	}
	return parsed, true, nil
}

func optionalStringAttr(attrs map[string]ddbtypes.AttributeValue, key string) (string, bool, error) {
	value, ok := attrs[key]
	if !ok {
		return "", false, nil
	}
	text, ok := value.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", true, fmt.Errorf("attribute %q is not a string", key)
	}
	return text.Value, true, nil
}

func optionalNumAttr(attrs map[string]ddbtypes.AttributeValue, key string) (uint64, error) {
	if _, ok := attrs[key]; !ok {
		return 0, nil
	}
	return numAttr(attrs, key)
}

func strAttr(attrs map[string]ddbtypes.AttributeValue, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	sv, ok := v.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return sv.Value
}

func marshalEndpoints(endpoints map[string]string) *ddbtypes.AttributeValueMemberM {
	m := make(map[string]ddbtypes.AttributeValue, len(endpoints))
	for k, v := range endpoints {
		m[k] = &ddbtypes.AttributeValueMemberS{Value: v}
	}
	return &ddbtypes.AttributeValueMemberM{Value: m}
}

func unmarshalEndpoints(attrs map[string]ddbtypes.AttributeValue) map[string]string {
	v, ok := attrs[attrEndpoints]
	if !ok {
		return nil
	}
	mv, ok := v.(*ddbtypes.AttributeValueMemberM)
	if !ok || len(mv.Value) == 0 {
		return nil
	}
	result := make(map[string]string, len(mv.Value))
	for k, av := range mv.Value {
		if sv, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
			result[k] = sv.Value
		}
	}
	return result
}
