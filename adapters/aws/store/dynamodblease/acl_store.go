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
	defaultTableName = "gobridge-leases"

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

	// obsMu guards observations. Takeover of an ACTIVELY-HELD lease uses a
	// local-clock observation window instead of comparing the owner's written
	// expires_at to the taker's clock, which removes the cross-clock skew hazard
	// where a taker whose clock runs fast could seize an unexpired lease
	// (finding M5). observations records, per lease, the liveness tuple
	// (owner, version, renewed_at) this process last saw and WHEN (on this
	// process's own clock) it first saw that exact tuple.
	obsMu        sync.Mutex
	observations map[string]leaseObservation
}

// leaseObservation is one process's local record that a lease appeared held by
// a particular liveness tuple, and the local-clock instant that tuple was first
// observed. A takeover is permitted only once the SAME tuple has persisted for
// at least the lease's own declared TTL, measured entirely on this process's
// clock.
type leaseObservation struct {
	owner     string
	version   uint64
	renewedAt int64 // epoch millis, as written by the owner
	firstSeen time.Time
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
		tableName: defaultTableName,
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
//     the current (owner, version). A node cannot race itself — duplicate
//     ownerIDs across nodes are a fatal misconfiguration, not a supported
//     topology — so the observation window is skipped, sparing the restarted
//     owner from watching its OWN stale tuple for a full TTL (which would leave
//     the partition ownerless for up to ~2×TTL).
//
//   - An ACTIVELY-HELD lease held by a DIFFERENT owner is taken over through a
//     local-clock OBSERVATION window (finding M5): the taker seizes only after
//     it has seen the owner's liveness tuple (owner, version, renewed_at)
//     unchanged for at least the lease's own declared TTL, measured on the
//     TAKER's clock. The seize is fenced on that exact tuple, so a renewal that
//     lands after observation began aborts the takeover. The previous
//     implementation compared the owner-written expires_at to the taker's
//     clock, so a taker with a fast clock could seize an unexpired lease; the
//     observation window removes that cross-clock comparison entirely.
//
// Residual assumption: a standby that starts observing only AFTER the owner has
// already died needs up to ~2×TTL to seize (it must observe the now-final tuple
// for a full TTL first). A standby that polls continuously — the normal HA case
// — begins its observation window at roughly the owner's last successful
// renewal, so it seizes ~TTL after that renewal. The correctness of the window
// relies only on this process's own clock being monotonic within a takeover,
// not on agreement with the owner's clock.
//
// Operational bound (finding F4): because a COLD standby — one that starts AFTER
// the owner died, e.g. the replacement pod brought up by a rolling deploy — can
// take up to ~2×TTL to seize, any strict "fail over within N seconds" objective
// requires lease_ttl ≤ N/2. For the common ≤60s target, configure lease_ttl
// ≤ 30s. The ~2×TTL bound assumes the standby BEGINS observing within ~TTL of
// the owner's death; one brought up even later seizes ~TTL after it first
// observes the final tuple, so a badly delayed rollout can exceed 2×TTL from
// death — budget standby-start latency into the objective. Shrinking the
// cold-standby case to ~TTL would require persisting the first-seen observation
// (kept in per-process memory today — see observationFor/recordObservation).
// That need not touch the safety-critical fencing row — a separate observation
// record would do — but the added write path and its failure modes are out of
// scope, so the 2×TTL cold-standby bound is left as a documented operational
// constraint.
func (s *Store) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
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
		return persistence.LeaseToken{}, wrapErr(err, "", "leaseID", leaseID, "ownerID", ownerID)
	}

	// Item exists. Read it (strongly consistent) to choose the takeover path.
	row, err := s.getRow(ctx, leaseID)
	if err != nil {
		return persistence.LeaseToken{}, wrapErr(err, "lease takeover read failed", "leaseID", leaseID, "ownerID", ownerID)
	}
	if !row.present {
		// The row vanished between PutItem and GetItem (concurrent release +
		// delete is not possible — rows are never deleted — but be defensive).
		// Signal contention so the caller's poll retries with a fresh acquire.
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease row disappeared during takeover read").
			With("leaseID", leaseID)
	}

	if row.owner == "" || row.expiresAt <= 0 {
		// Released (or explicitly zero-expiry) lease: no live owner. A
		// version-fenced takeover is safe without any clock comparison.
		s.clearObservation(leaseID)
		return s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt,
			"#own = :cur_own AND #ver = :cur_ver",
			nil,
			map[string]ddbtypes.AttributeValue{
				":cur_own": &ddbtypes.AttributeValueMemberS{Value: row.owner},
				":cur_ver": &ddbtypes.AttributeValueMemberN{Value: uintStr(row.version)},
			})
	}

	return s.observeOrSeize(ctx, leaseID, ownerID, ttl, endpoints, now, expiresAt, row)
}

// observeOrSeize implements the local-clock observation-based takeover of an
// actively-held lease (finding M5).
func (s *Store) observeOrSeize(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string, now, expiresAt time.Time, row leaseRow) (persistence.LeaseToken, error) {
	// Same-owner fast path: the lease row still names THIS ownerID as owner.
	// The only writer that ever stamps owner=ownerID is this node — duplicate
	// ownerIDs across distinct nodes are a fatal misconfiguration, not a
	// supported topology (a fencing token cannot protect against two live
	// processes claiming one identity). The store itself cannot defend against
	// this, so the runtime that DERIVES ownerID guards it upstream: it suffixes
	// the human-facing instance_id with a per-process boot nonce
	// (Runtime.leaseOwnerID), so two replicas that share an instance_id still
	// present DISTINCT ownerIDs here and take the observation path against each
	// other instead of instantly counter-seizing via this fast path — the
	// permanent lease ping-pong of finding C3-HIGH. Given distinct-owner
	// invariant, a node cannot race itself, so seize immediately, fenced on the
	// exact (owner, version) just read: a concurrent takeover by a DIFFERENT
	// owner (which increments version) still aborts this claim via the
	// ConditionExpression and falls back to ErrAlreadyExists. This spares a
	// crashed-and-restarted owner (fresh process, empty observation map) from
	// observing its OWN stale tuple for a full TTL before reclaiming — otherwise
	// the partition stays ownerless for up to ~2×TTL.
	//
	// NOTE (finding C3-CRITICAL): the runtime no longer hammers this fast path in
	// a zombie retry loop. A single-use session that cannot re-Start after a
	// step-down Close now RELEASES the lease and escalates to a process restart
	// instead of re-Acquiring here on every supervised retry (which bumped the
	// version and reset every standby's observation window perpetually).
	if row.owner == ownerID {
		s.clearObservation(leaseID)
		return s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt,
			"#own = :cur_own AND #ver = :cur_ver",
			nil,
			map[string]ddbtypes.AttributeValue{
				":cur_own": &ddbtypes.AttributeValueMemberS{Value: row.owner},
				":cur_ver": &ddbtypes.AttributeValueMemberN{Value: uintStr(row.version)},
			})
	}

	obs, ok := s.observationFor(leaseID)
	tupleChanged := !ok || obs.owner != row.owner || obs.version != row.version || obs.renewedAt != row.renewedAt
	if tupleChanged {
		// First sighting of this liveness tuple, or the owner has renewed since
		// we last looked (renewed_at advanced): (re)start the observation window.
		// We cannot seize yet.
		s.recordObservation(leaseID, leaseObservation{
			owner:     row.owner,
			version:   row.version,
			renewedAt: row.renewedAt,
			firstSeen: now,
		})
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; observing for takeover").
			With("leaseID", leaseID).
			With("owner", row.owner)
	}

	// The lease's own declared TTL is expires_at - renewed_at: a difference of
	// two timestamps BOTH written by the owner on the same clock, so it is
	// skew-immune. We measure elapsed time against our OWN clock only.
	observedTTL := time.Duration(row.expiresAt-row.renewedAt) * time.Millisecond
	if observedTTL <= 0 {
		observedTTL = ttl
	}
	if now.Sub(obs.firstSeen) < observedTTL {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease held; observation window not yet elapsed").
			With("leaseID", leaseID)
	}

	// The owner has not renewed for a full TTL of our local time. Seize, fencing
	// on the exact observed tuple so a renewal that landed after observation
	// began (owner recovered) aborts the takeover.
	tok, err := s.runTakeover(ctx, leaseID, ownerID, endpoints, now, expiresAt,
		"#own = :obs_own AND #ver = :obs_ver AND #ren = :obs_ren",
		map[string]string{"#ren": attrRenewedAt},
		map[string]ddbtypes.AttributeValue{
			":obs_own": &ddbtypes.AttributeValueMemberS{Value: obs.owner},
			":obs_ver": &ddbtypes.AttributeValueMemberN{Value: uintStr(obs.version)},
			":obs_ren": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(obs.renewedAt, 10)},
		})
	if err != nil {
		if errors.Is(err, shared.ErrAlreadyExists) {
			// Owner renewed under us; drop the stale window so the next poll
			// starts observing the new tuple rather than hot-retrying a seize.
			s.clearObservation(leaseID)
		}
		return persistence.LeaseToken{}, err
	}
	s.clearObservation(leaseID)
	return tok, nil
}

// runTakeover issues the version-incrementing takeover UpdateItem shared by the
// released-lease and observed-lease paths. The caller supplies the fencing
// condition and any extra names/values it references; runTakeover appends the
// standard SET clause, endpoints, and the legacy-ttl strip.
func (s *Store) runTakeover(ctx context.Context, leaseID, ownerID string, endpoints map[string]string, now, expiresAt time.Time, cond string, extraNames map[string]string, extraValues map[string]ddbtypes.AttributeValue) (persistence.LeaseToken, error) {
	pk := leaseKey(leaseID)

	updateExpr := "SET #own = :owner, #ver = #ver + :one, #acq = :now_ms, #exp = :exp_ms, #ren = :now_ms"
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
	// Strip any legacy ttl a pre-fix build may have stamped (MF-1/J1).
	updateExpr += " REMOVE #ttl"
	exprNames["#ttl"] = attrTTL

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
	present   bool
	owner     string
	version   uint64
	expiresAt int64
	renewedAt int64
}

// getRow reads the lease item with a strongly consistent read and decodes the
// fields the takeover logic needs.
func (s *Store) getRow(ctx context.Context, leaseID string) (leaseRow, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: leaseKey(leaseID)},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return leaseRow{}, err
	}
	if len(result.Item) == 0 {
		return leaseRow{present: false}, nil
	}
	// A lease row that exists MUST carry a parseable fencing version; a
	// present-but-corrupt version is surfaced rather than silently read as 0
	// (which would reset the fence). A genuinely absent attribute (legacy row)
	// is tolerated as 0.
	version, err := optionalNumAttr(result.Item, attrVersion)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	expiresAt, err := optionalNumAttr(result.Item, attrExpiresAt)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	renewedAt, err := optionalNumAttr(result.Item, attrRenewedAt)
	if err != nil {
		return leaseRow{}, wrapErr(err, "lease row read failed", "leaseID", leaseID)
	}
	return leaseRow{
		present:   true,
		owner:     strAttr(result.Item, attrOwner),
		version:   version,
		expiresAt: int64(expiresAt),
		renewedAt: int64(renewedAt),
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
	// Strip any legacy ttl so a renewed (actively-held) row is never reaped
	// (MF-1/J1). This sheds a stale ttl within one renew interval of upgrade.
	updateExpr += " REMOVE #ttl"
	exprNames["#ttl"] = attrTTL

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
			"SET #own = :empty, #exp = :zero REMOVE #ttl"),
		ExpressionAttributeNames: map[string]string{
			"#own": attrOwner,
			"#ver": attrVersion,
			"#exp": attrExpiresAt,
			"#ttl": attrTTL,
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
