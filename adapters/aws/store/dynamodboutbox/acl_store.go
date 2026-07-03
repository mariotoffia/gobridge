package dynamodboutbox

import (
	"context"
	"log/slog"
	"sort"
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
	defaultTableName          = "gobridge-outbox"
	defaultCompactionGrace    = 1 * time.Hour
	defaultStaleClaimDuration = 30 * time.Second

	// defaultResolveMaxAttempts bounds how many times Complete retries a
	// RecordIDIndex GSI lookup that returns not-found. The GSI is
	// eventually consistent, so a lookup immediately after a Claim on a
	// cold key cache can lag; a small bounded retry closes the
	// duplicate-delivery window without unbounded blocking.
	//
	// ponytail: fixed small ceiling; the key cache populated by Claim is
	// the primary (miss-free) path, so this retry is only exercised on a
	// cold cache (process restart with in-flight claimed records).
	defaultResolveMaxAttempts = 5
	defaultResolveBackoff     = 50 * time.Millisecond

	skPrefix = "OUTBOX#"

	// fenceSK is the sort key of the per-partition fence-metadata row that
	// holds the monotonic max_claim_version and the seq_counter used to
	// stamp per-partition persist order. It is deliberately outside the
	// OUTBOX# key space so it is invisible to record queries and, having no
	// GSI key attributes, to every GSI as well.
	fenceSK             = "FENCE"
	attrMaxClaimVersion = "max_claim_version"

	// attrSeqCounter is the fence-row atomic counter that allocates the
	// monotonic per-partition Seq values Persist stamps on records, so
	// Claim can order same-millisecond records by persist order (the
	// ports.OutboxStore claim-ordering contract).
	attrSeqCounter = "seq_counter"

	// attrHasExpiry marks records persisted with a non-zero ExpiresAt. It
	// is the sparse hash key of ExpiryIndex: only expiry-carrying records
	// enter the index, it is written once at Persist and removed once at a
	// terminal transition (Complete/Expire) — never rewritten on Claim —
	// so state transitions cause no index write amplification (unlike the
	// former StatusIndex, which reindexed every record on every
	// transition).
	attrHasExpiry     = "has_expiry"
	hasExpiryFlag     = "1"
	expiryIndexName   = "ExpiryIndex"
	recordIDIndexName = "RecordIDIndex"

	// fenceRetentionFloor is the minimum TTL horizon stamped on FENCE
	// rows: fences live for max(compactGrace, fenceRetentionFloor) past
	// the last write that touches them (Persist seq allocation and Claim
	// fence raise both refresh it). Tradeoff: a partition abandoned longer
	// than this loses its fencing high-water-mark — acceptable, because a
	// fence whose partition has had no writes for 30+ days protects
	// records that were compacted away long ago, while keeping the fence
	// immortal leaks one row per ephemeral session forever.
	fenceRetentionFloor = 30 * 24 * time.Hour
)

// Store implements ports.OutboxStore using DynamoDB with partitioned
// persistence, conditional writes, and TTL-based compaction. It also
// implements ports.OutboxReleaser so a live owner can return a
// transiently-failed record to pending without waiting for stale reclaim.
//
// Key design:
//
//	PK = "<partition_key>" (e.g. "SESSION#<session_id>" or "BINDING#<binding_id>")
//	SK = "OUTBOX#<envelope_id>#<binding_id>"
//
// The SK incorporates both envelope_id and binding_id so that:
//   - Fan-out records (same envelope, different bindings) have distinct keys
//   - Idempotent persist uses attribute_not_exists(SK) to skip records
//     already stored for the same envelope+binding combination across
//     redeliveries (per-record Persist idempotency, ports.OutboxStore)
//
// A RecordIDIndex GSI on the record_id attribute enables O(1) lookup
// by application-level record ID for the Complete operation.
type Store struct {
	client       dynamoAPI
	table        string
	compactGrace time.Duration
	staleClaim   time.Duration
	clk          clock.Clock
	logger       *slog.Logger

	resolveMaxAttempts int
	resolveBackoff     time.Duration

	// keys maps an application record ID to its base-table (PK, SK),
	// populated on Claim so Complete can address the base table directly
	// instead of resolving through the eventually consistent RecordIDIndex
	// GSI (which can lag and report not-found, causing duplicate delivery).
	// It is a bounded LRU so claimed-but-never-completed entries (lease
	// churn) cannot grow it without limit; see keyCache (J-N1).
	keys *keyCache
}

type recordKey struct {
	pk string
	sk string
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-outbox").
func WithTableName(name string) Option {
	return func(s *Store) { s.table = name }
}

// WithCompactionGrace sets the duration added to completed/expired timestamps
// before setting the DynamoDB TTL attribute for physical deletion.
func WithCompactionGrace(d time.Duration) Option {
	return func(s *Store) { s.compactGrace = d }
}

// WithStaleClaimDuration sets how old a claimed record must be before it can
// be reclaimed without a fencing-version bump (crash-recovery fallback).
// A value <= 0 disables wall-clock stale reclaim entirely: reclaim then
// happens only via a strictly higher fencing version (version-only), matching
// the memory and SQLite backends.
func WithStaleClaimDuration(d time.Duration) Option {
	return func(s *Store) { s.staleClaim = d }
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

// WithCompleteResolveRetry tunes how Complete tolerates RecordIDIndex GSI
// replication lag when the Claim-populated key cache misses (e.g. after a
// process restart with in-flight claimed records). attempts is the total
// number of GSI lookups (>=1) and backoff is the pause between them. Values
// <= 0 leave the defaults unchanged.
func WithCompleteResolveRetry(attempts int, backoff time.Duration) Option {
	return func(s *Store) {
		if attempts > 0 {
			s.resolveMaxAttempts = attempts
		}
		if backoff > 0 {
			s.resolveBackoff = backoff
		}
	}
}

// NewStore creates a DynamoDB-backed OutboxStore.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL
// constructor exists to wrap; it is injected by the composition root and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:       client,
		table:        defaultTableName,
		compactGrace: defaultCompactionGrace,
		staleClaim:   defaultStaleClaimDuration,
		clk:          clock.System,

		resolveMaxAttempts: defaultResolveMaxAttempts,
		resolveBackoff:     defaultResolveBackoff,
		keys:               newKeyCache(defaultMaxKeyCache),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateTable creates the DynamoDB table with the required schema and GSIs.
// It is idempotent: if the table already exists, it returns nil.
func (s *Store) CreateTable(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "dynamodboutbox: create_table")
	}

	_, err := s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(s.table),
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrHasExpiry), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("expires_at"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			{AttributeName: aws.String("record_id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				// ExpiryIndex is sparse: only records persisted with a
				// non-zero ExpiresAt carry has_expiry, so the index holds
				// exactly the Expire-eligible candidates. It is written once
				// at Persist and deleted once at a terminal transition —
				// state transitions (Claim) never touch it. This replaces
				// the former StatusIndex (a table-wide hot partition keyed
				// on ≤4 status values, rewritten on every transition) and
				// the ClaimedByIndex (created but never queried).
				//
				// ponytail: all expiry-carrying records share one hash value
				// ("1"), bounding the index partition's write throughput; if
				// expiry-heavy workloads ever hit that ceiling, shard the
				// flag value (e.g. "1#<n>") and fan out the Expire query.
				IndexName: aws.String(expiryIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String(attrHasExpiry), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("expires_at"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
			},
			{
				IndexName: aws.String(recordIDIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("record_id"), KeyType: ddbtypes.KeyTypeHash},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
			},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		if isResourceInUse(err) {
			return nil
		}
		return wrapErr(err, "create outbox table failed", "table", s.table)
	}

	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.table),
	}, 2*time.Minute); err != nil {
		return wrapErr(err, "wait for outbox table to exist failed", "table", s.table)
	}

	// Enable DynamoDB TTL on the "ttl" attribute so completed/expired records
	// are compacted physically. A ttl is stamped on completed records and on
	// any record carrying an ExpiresAt (including pending ones) — but always
	// as expiresAt + compactGrace, i.e. strictly at/after the record's own
	// expiry, so TTL never reaps a still-deliverable record. Records with no
	// ExpiresAt carry no ttl. FENCE metadata rows carry a long TTL of
	// max(compactGrace, fenceRetentionFloor) past their last write, so
	// abandoned (ephemeral/rotating session) partitions are eventually
	// cleaned up instead of leaking one immortal row each. Best-effort: TTL
	// is a housekeeping convenience, not a correctness requirement, and is
	// already enabled on re-runs.
	if _, err := s.client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(s.table),
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("ttl"),
		},
	}); err != nil {
		if s.logger != nil {
			s.logger.Warn("dynamodboutbox: enabling DynamoDB TTL failed; completed records not compacted",
				"table", s.table, "error", err.Error())
		}
	}
	return nil
}

// Persist writes outbox records with per-record idempotency
// (ports.OutboxStore Persist contract): each record is a conditional
// PutItem keyed on attribute_not_exists(SK), so records whose
// (partition, envelope, binding) identity already exists are skipped
// while new records in the same batch are persisted. It returns
// shared.ErrDuplicateRecord ONLY when every record in the batch already
// existed. Per-record writes (instead of one all-or-nothing transaction)
// are exactly what the idempotency contract makes safe: a batch that
// fails midway is simply re-persisted, and already-written legs become
// no-ops. This also removes the former 100-item transaction ceiling.
//
// Before writing, Persist allocates each record a monotonic per-partition
// Seq from the FENCE row's atomic seq_counter, so Claim can order
// same-millisecond records by persist order.
func (s *Store) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: persist", "count", len(records))
	}

	if len(records) == 0 {
		return nil
	}

	now := s.clk.Now()

	seqs, err := s.allocateSeqs(ctx, records, now)
	if err != nil {
		return err
	}

	duplicates := 0
	for i := range records {
		item, err := marshalRecord(records[i], now, s.compactGrace)
		if err != nil {
			return err
		}
		item["seq"] = &ddbtypes.AttributeValueMemberN{Value: u64(seqs[i])}

		_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String(s.table),
			Item:                item,
			ConditionExpression: aws.String("attribute_not_exists(SK)"),
		})
		if err != nil {
			if isConditionFailed(err) {
				duplicates++
				continue
			}
			return wrapErr(err, "outbox persist failed",
				"envelopeID", records[i].EnvelopeID(), "bindingID", records[i].BindingID())
		}
	}

	if duplicates == len(records) {
		return shared.ErrDuplicateRecord.
			WithMessage("all records in batch already persisted").
			With("recordCount", len(records))
	}
	return nil
}

// allocateSeqs reserves one monotonic per-partition sequence number per
// record by atomically incrementing the partition FENCE row's seq_counter
// (one UpdateItem per distinct partition in the batch). The same write
// refreshes the FENCE row's TTL. Sequences consumed by records that turn
// out to be duplicates leave gaps — the contract requires monotonicity,
// not contiguity.
func (s *Store) allocateSeqs(ctx context.Context, records []*persistence.OutboxRecord, now time.Time) ([]uint64, error) {
	counts := make(map[string]int)
	for _, r := range records {
		counts[partitionKey(r)]++
	}

	next := make(map[string]uint64, len(counts))
	for pk, n := range counts {
		newTop, err := s.addSeqCounter(ctx, pk, n, now)
		if err != nil {
			return nil, err
		}
		if newTop < uint64(n) {
			// Defensive: a backend that did not return the counter would
			// otherwise underflow. Seq 0 sorts first, preserving safety.
			newTop = uint64(n)
		}
		next[pk] = newTop - uint64(n)
	}

	seqs := make([]uint64, len(records))
	for i, r := range records {
		pk := partitionKey(r)
		next[pk]++
		seqs[i] = next[pk]
	}
	return seqs, nil
}

// addSeqCounter atomically advances the partition's seq_counter by n and
// returns the new counter value. The FENCE row is created on first use and
// its TTL horizon refreshed on every allocation.
func (s *Store) addSeqCounter(ctx context.Context, partitionKey string, n int, now time.Time) (uint64, error) {
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		UpdateExpression: aws.String("SET #ttl = :ttl ADD #sc :n"),
		ExpressionAttributeNames: map[string]string{
			"#sc":  attrSeqCounter,
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":n":   &ddbtypes.AttributeValueMemberN{Value: i64(int64(n))},
			":ttl": &ddbtypes.AttributeValueMemberN{Value: i64(s.fenceTTLEpoch(now))},
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, wrapErr(err, "outbox seq allocation failed", "partitionKey", partitionKey)
	}
	return numAttrU64(out.Attributes, attrSeqCounter), nil
}

// fenceTTLEpoch returns the epoch-seconds TTL stamped on FENCE rows:
// now + max(compactGrace, fenceRetentionFloor). See fenceRetentionFloor
// for the abandonment tradeoff.
func (s *Store) fenceTTLEpoch(now time.Time) int64 {
	horizon := s.compactGrace
	if horizon < fenceRetentionFloor {
		horizon = fenceRetentionFloor
	}
	return now.Add(horizon).Unix()
}

// Claim finds pending or stale-claimed records for the given partition and
// claims them for the given owner with the provided fencing token.
// Uses strongly consistent reads with pagination to handle the DynamoDB
// Limit+Filter interaction (Limit caps evaluated items, not filtered results).
//
// Per the ports.OutboxStore contract, Claim never filters by replay count —
// poison detection is the drainer's decision, and a store-side filter would
// strand high-replay records (unclaimable forever, then silently TTL-reaped
// with no DLQ entry).
//
// Each per-record claim executes as a TransactWriteItems that pairs the
// record update with a ConditionCheck on the partition FENCE row
// (max_claim_version <= token.Version). This closes the TOCTOU race where
// owner A (v5) passes the fence read, owner B (v6) raises the fence and
// starts claiming, and A keeps claiming other pending records below the
// high-water-mark — split-brain duplicate delivery. With the transactional
// check, A's next claim fails the fence condition and surfaces
// shared.ErrStaleFencingToken.
func (s *Store) Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: claim", "partition_key", partitionKey, "limit", limit)
	}

	now := s.clk.Now()

	// staleMs is the wall-clock threshold (Unix ms) below which a claimed_at
	// marks a stranded claim as reclaimable. staleClaim <= 0 disables
	// wall-clock reclaim: the sentinel 0 can never exceed a real claimed_at,
	// so reclaim is version-only (matching memory/sqlite semantics).
	staleMs := int64(0)
	if s.staleClaim > 0 {
		staleMs = now.Add(-s.staleClaim).UnixMilli()
	}

	// Version-monotonic fence: reject a token whose version is older than the
	// highest claim_version the partition has already observed, so a preempted
	// owner cannot win a freshly pending row (matches memory/sqlite and the
	// ports.OutboxStore contract).
	maxVersion, err := s.maxClaimVersion(ctx, partitionKey)
	if err != nil {
		return nil, err
	}
	if token.Version < maxVersion {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", maxVersion)
	}

	// Raise the per-partition fence to this (accepted) token version so any
	// later claim with a lower version is rejected in O(1), even after the
	// records this owner claims are completed and compacted away. Raise-only
	// via a conditional write, so concurrent higher versions are preserved.
	if err := s.raiseFence(ctx, partitionKey, token.Version, now); err != nil {
		return nil, err
	}

	// limit <= 0 is a fencing no-op (ports.OutboxStore contract): the fence
	// high-water-mark above has been advanced; no records are claimed.
	if limit <= 0 {
		return nil, nil
	}

	// A claimed record is reclaimable when its claim_version is strictly
	// older than the incoming token (version-monotonic preemption, matching
	// the port contract and memory/sqlite) OR when its claim has gone stale
	// past the wall-clock threshold (crash-recovery fallback).
	filterExpr := "(#st = :pending) OR (#st = :claimed AND (claim_version < :ver OR claimed_at < :stale))"
	exprNames := map[string]string{"#st": "status"}
	exprValues := map[string]ddbtypes.AttributeValue{
		":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
		":prefix":  &ddbtypes.AttributeValueMemberS{Value: skPrefix},
		":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
		":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
		":stale":   &ddbtypes.AttributeValueMemberN{Value: i64(staleMs)},
	}

	var claimed []*persistence.OutboxRecord
	var startKey map[string]ddbtypes.AttributeValue

	for len(claimed) < limit {
		input := &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			FilterExpression:          aws.String(filterExpr),
			ExpressionAttributeNames:  exprNames,
			ExpressionAttributeValues: exprValues,
			ConsistentRead:            aws.Bool(true),
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		queryOut, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "outbox claim query failed", "partitionKey", partitionKey)
		}

		for _, item := range queryOut.Items {
			if len(claimed) >= limit {
				break
			}
			if err := ctx.Err(); err != nil {
				return claimed, err
			}

			pk := strAttr(item, "PK")
			sk := strAttr(item, "SK")

			rec, err := s.claimOne(ctx, item, pk, sk, token, now, staleMs)
			if err != nil {
				return nil, err
			}
			if rec == nil {
				continue // lost the per-record race; not an error
			}
			// Cache the base-table keys so Complete can address this record
			// directly instead of resolving through the lagging GSI.
			s.cacheKey(rec.ID(), pk, sk)
			claimed = append(claimed, rec)
		}

		if queryOut.LastEvaluatedKey == nil {
			break
		}
		startKey = queryOut.LastEvaluatedKey
	}

	// Return claimed records in per-partition persist order — ascending
	// (CreatedAt, Seq) per the ports.OutboxStore claim-ordering contract.
	// Records persisted before the sequence existed carry Seq 0 and sort
	// first within their millisecond; envelopeID is the final fallback so
	// legacy ties stay deterministic.
	sort.Slice(claimed, func(i, j int) bool {
		ci, cj := claimed[i].CreatedAt(), claimed[j].CreatedAt()
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		if claimed[i].Seq() != claimed[j].Seq() {
			return claimed[i].Seq() < claimed[j].Seq()
		}
		return claimed[i].EnvelopeID() < claimed[j].EnvelopeID()
	})

	return claimed, nil
}

// claimOne transitions a single candidate record to claimed inside a
// TransactWriteItems that also condition-checks the partition FENCE row, so
// a concurrently raised fence (higher-version owner took the partition)
// aborts the claim instead of silently split-braining.
//
// Returns (nil, nil) when only the record-level condition failed — another
// claimer won this record; the caller skips it. Returns
// shared.ErrStaleFencingToken when the fence check failed.
//
// TransactWriteItems cannot return the updated item, so the post-claim
// record is synthesized from the queried item plus the exact values the
// update wrote — the transaction's conditions guarantee they took effect.
func (s *Store) claimOne(
	ctx context.Context,
	item map[string]ddbtypes.AttributeValue,
	pk, sk string,
	token persistence.LeaseToken,
	now time.Time,
	staleMs int64,
) (*persistence.OutboxRecord, error) {
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []ddbtypes.TransactWriteItem{
			{
				ConditionCheck: &ddbtypes.ConditionCheck{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
					},
					ConditionExpression:      aws.String("attribute_not_exists(#mcv) OR #mcv <= :ver"),
					ExpressionAttributeNames: map[string]string{"#mcv": attrMaxClaimVersion},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":ver": &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
					},
				},
			},
			{
				Update: &ddbtypes.Update{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
					},
					UpdateExpression: aws.String(
						"SET #st = :claimed, claimed_by = :owner, claim_version = :ver, " +
							"claimed_at = :now, replay_count = if_not_exists(replay_count, :zero) + :one"),
					ConditionExpression: aws.String(
						"(#st = :pending) OR (#st = :claimed AND (claim_version < :ver OR claimed_at < :stale))"),
					ExpressionAttributeNames: map[string]string{"#st": "status"},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
						":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
						":owner":   &ddbtypes.AttributeValueMemberS{Value: token.Owner},
						":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
						":now":     &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
						":stale":   &ddbtypes.AttributeValueMemberN{Value: i64(staleMs)},
						":zero":    &ddbtypes.AttributeValueMemberN{Value: "0"},
						":one":     &ddbtypes.AttributeValueMemberN{Value: "1"},
					},
				},
			},
		},
	})
	if err != nil {
		if reasons, canceled := transactCancellationCodes(err); canceled {
			// Item 0 is the fence check: its failure means a higher-version
			// owner raised the fence between our read and this claim — the
			// exact TOCTOU split-brain the transaction exists to prevent.
			if len(reasons) > 0 && reasons[0] == "ConditionalCheckFailed" {
				return nil, shared.ErrStaleFencingToken.
					WithMessage("claim aborted: partition fence advanced past token version").
					With("givenVersion", token.Version).
					With("partitionKey", pk)
			}
			// Record-level condition failure or a transient transaction
			// conflict: another claimer won this record. Skip it.
			return nil, nil
		}
		return nil, wrapErr(err, "outbox claim update failed", "partitionKey", pk, "ownerID", token.Owner)
	}

	// Synthesize the post-claim state on a copy of the queried item.
	updated := make(map[string]ddbtypes.AttributeValue, len(item)+4)
	for k, v := range item {
		updated[k] = v
	}
	updated["status"] = &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)}
	updated["claimed_by"] = &ddbtypes.AttributeValueMemberS{Value: token.Owner}
	updated["claim_version"] = &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)}
	updated["claimed_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())}
	updated["replay_count"] = &ddbtypes.AttributeValueMemberN{Value: i64(numAttrI64(item, "replay_count") + 1)}

	return unmarshalRecord(updated)
}

// Complete marks the given records as completed after successful target delivery.
// The caller's fencing token must match the claim_version on each record.
func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: complete", "count", len(recordIDs))
	}

	if len(recordIDs) == 0 {
		return nil
	}

	now := s.clk.Now()
	ttlEpoch := now.Add(s.compactGrace).Unix()

	for _, id := range recordIDs {
		pk, sk, err := s.resolveRecordKeys(ctx, id)
		if err != nil {
			return err
		}
		if pk == "" {
			// The Claim-populated key cache missed (e.g. a cold cache after a
			// process restart) and the eventually consistent RecordIDIndex GSI
			// still had not converged after the bounded resolve retry. The
			// record was just claimed, so it exists in the base table — this is
			// GSI replication lag, not a genuine absence. A permanent
			// ErrNotFound here would tell the caller to give up completing and
			// let the record be re-DELIVERED on the next stale/version reclaim;
			// a transient timeout instead keeps it claimed so the caller
			// retries Complete once the GSI catches up, closing the
			// duplicate-delivery window (at-least-once still holds). See J-N3.
			return shared.ErrTimeout.
				WithMessage("outbox record key resolution timed out: RecordIDIndex GSI lag").
				With("recordID", id)
		}

		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(s.table),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
				"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
			// REMOVE has_expiry drops the terminal record out of the sparse
			// ExpiryIndex so Expire never re-scans it.
			UpdateExpression: aws.String(
				"SET #st = :completed, completed_at = :now, #ttl = :ttl REMOVE " + attrHasExpiry),
			ConditionExpression: aws.String(
				"#st = :claimed AND claimed_by = :owner AND claim_version = :ver"),
			ExpressionAttributeNames: map[string]string{
				"#st":  "status",
				"#ttl": "ttl",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":completed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxCompleted)},
				":claimed":   &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":now":       &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
				":ttl":       &ddbtypes.AttributeValueMemberN{Value: i64(ttlEpoch)},
				":owner":     &ddbtypes.AttributeValueMemberS{Value: token.Owner},
				":ver":       &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
			},
		})
		if err != nil {
			if isConditionFailed(err) {
				return shared.ErrStaleFencingToken.
					WithMessage("claim version mismatch on complete").
					With("recordID", id).
					With("givenVersion", token.Version)
			}
			return wrapErr(err, "outbox complete update failed", "recordID", id, "ownerID", token.Owner)
		}
		// Record is terminal; drop its cached keys.
		s.evictKey(id)
	}

	return nil
}

// Release implements ports.OutboxReleaser: it returns transiently-failed
// claimed records to pending immediately so a still-alive owner can retry
// them on the next drain, without a fencing-version bump or a stale-claim
// timeout. Fencing is owner+version+status, identical to Complete; on any
// mismatch it returns shared.ErrStaleFencingToken.
//
// Releases are applied per record in order and stop at the first mismatch
// (documented in the ports.OutboxReleaser contract); the live drainer
// always passes exactly one record ID. claim_version is retained on the
// released record — the record is pending again, so it is claimable by any
// token at or above the partition fence.
func (s *Store) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: release", "count", len(recordIDs))
	}

	for _, id := range recordIDs {
		pk, sk, err := s.resolveRecordKeys(ctx, id)
		if err != nil {
			return err
		}
		if pk == "" {
			// Same GSI-lag reasoning as Complete: the record was just
			// claimed, so absence is index lag, not a genuine miss. Surface
			// a transient error so the caller can retry the release; the
			// record stays claimed and remains stale-reclaimable.
			return shared.ErrTimeout.
				WithMessage("outbox record key resolution timed out: RecordIDIndex GSI lag").
				With("recordID", id)
		}

		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(s.table),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
				"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
			// claimed_by/claimed_at are removed to match the unclaimed item
			// shape Persist writes; has_expiry is untouched (Claim never
			// removed it), so a released expiry-carrying record remains
			// visible to Expire.
			UpdateExpression: aws.String("SET #st = :pending REMOVE claimed_by, claimed_at"),
			ConditionExpression: aws.String(
				"#st = :claimed AND claimed_by = :owner AND claim_version = :ver"),
			ExpressionAttributeNames: map[string]string{"#st": "status"},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
				":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":owner":   &ddbtypes.AttributeValueMemberS{Value: token.Owner},
				":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
			},
		})
		if err != nil {
			if isConditionFailed(err) {
				return shared.ErrStaleFencingToken.
					WithMessage("claim mismatch on release").
					With("recordID", id).
					With("givenVersion", token.Version)
			}
			return wrapErr(err, "outbox release update failed", "recordID", id, "ownerID", token.Owner)
		}
	}

	return nil
}

// Expire marks pending records whose ExpiresAt is before the given time as
// expired. Claimed records are never expired here. Returns the count.
//
// Candidates come from the sparse ExpiryIndex (only records persisted with
// a non-zero ExpiresAt are in it); the per-record conditional update gates
// on pending status, so claimed/terminal candidates are skipped without
// error.
func (s *Store) Expire(ctx context.Context, before time.Time) (int, error) {
	beforeMs := before.UnixMilli()
	ttlEpoch := before.Add(s.compactGrace).Unix()
	// Pending-only: a claimed record is reclaimed via Claim/IsClaimable,
	// never expired out from under a potentially still-valid owner.
	return s.expireByStatus(ctx, string(persistence.OutboxPending), beforeMs, ttlEpoch)
}

func (s *Store) expireByStatus(ctx context.Context, status string, beforeMs, ttlEpoch int64) (int, error) {
	count := 0
	var startKey map[string]ddbtypes.AttributeValue

	for {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			IndexName:              aws.String(expiryIndexName),
			KeyConditionExpression: aws.String("#he = :flag AND expires_at < :before"),
			ExpressionAttributeNames: map[string]string{
				"#he": attrHasExpiry,
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":flag":   &ddbtypes.AttributeValueMemberS{Value: hasExpiryFlag},
				":before": &ddbtypes.AttributeValueMemberN{Value: i64(beforeMs)},
			},
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			return count, wrapErr(err, "outbox expire query failed", "status", status)
		}

		for _, item := range out.Items {
			pk := strAttr(item, "PK")
			sk := strAttr(item, "SK")

			_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName: aws.String(s.table),
				Key: map[string]ddbtypes.AttributeValue{
					"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
					"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
				},
				// REMOVE has_expiry drops the now-terminal record out of the
				// sparse ExpiryIndex so later Expire passes never re-scan it.
				UpdateExpression: aws.String("SET #st = :expired, #ttl = :ttl REMOVE " + attrHasExpiry),
				// Condition gates on the status this pass is allowed to
				// expire (pending-only from Expire) plus the expiry window,
				// so a candidate that was claimed or completed between the
				// index read and this write is skipped, not corrupted.
				ConditionExpression: aws.String(
					"#st = :status AND expires_at > :zero AND expires_at < :before"),
				ExpressionAttributeNames: map[string]string{
					"#st":  "status",
					"#ttl": "ttl",
				},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":expired": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxExpired)},
					":status":  &ddbtypes.AttributeValueMemberS{Value: status},
					":zero":    &ddbtypes.AttributeValueMemberN{Value: "0"},
					":before":  &ddbtypes.AttributeValueMemberN{Value: i64(beforeMs)},
					":ttl":     &ddbtypes.AttributeValueMemberN{Value: i64(ttlEpoch)},
				},
			})
			if err != nil {
				if isConditionFailed(err) {
					continue
				}
				return count, wrapErr(err, "outbox expire update failed", "partitionKey", pk)
			}
			count++
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	return count, nil
}

// QueryPending returns pending records for the given partition key, ordered
// by creation time (oldest first). Uses strongly consistent reads and
// paginates past DynamoDB's Limit+Filter interaction.
func (s *Store) QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: query_pending", "partition_key", partitionKey, "limit", limit)
	}

	var records []*persistence.OutboxRecord
	var startKey map[string]ddbtypes.AttributeValue

	for len(records) < limit {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			FilterExpression:       aws.String("#st = :pending"),
			ExpressionAttributeNames: map[string]string{
				"#st": "status",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
				":prefix":  &ddbtypes.AttributeValueMemberS{Value: skPrefix},
				":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
			},
			ConsistentRead: aws.Bool(true),
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "outbox query pending failed", "partitionKey", partitionKey)
		}

		for _, item := range out.Items {
			rec, err := unmarshalRecord(item)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	// Per-partition persist order — ascending (CreatedAt, Seq) with
	// envelopeID as the legacy fallback, matching the Claim ordering
	// contract.
	sort.Slice(records, func(i, j int) bool {
		ci, cj := records[i].CreatedAt(), records[j].CreatedAt()
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		if records[i].Seq() != records[j].Seq() {
			return records[i].Seq() < records[j].Seq()
		}
		return records[i].EnvelopeID() < records[j].EnvelopeID()
	})

	if len(records) > limit {
		records = records[:limit]
	}

	return records, nil
}

// maxClaimVersion returns the monotonic fence for the partition: the
// highest claim_version any owner has ever used to claim in this partition
// (0 if none). It reads a single per-partition fence-metadata row (O(1),
// strongly consistent) rather than scanning the whole partition.
//
// Backward-compatibility: partitions written before the fence row existed
// have no fence row (or, when only Persist has touched the partition, a
// fence row whose max_claim_version attribute is absent). On such a cold
// partition the fence is seeded once from a bounded scan of the existing
// records' claim_version and persisted, so the O(N) scan happens at most
// once per fence lifetime. Fence rows carry a long TTL (see
// fenceRetentionFloor); a partition abandoned past that horizon re-seeds
// from whatever records remain.
func (s *Store) maxClaimVersion(ctx context.Context, partitionKey string) (uint64, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return 0, wrapErr(err, "outbox fence read failed", "partitionKey", partitionKey)
	}
	// Check the ATTRIBUTE, not row presence: a fence row created solely by
	// Persist's seq allocation carries seq_counter but no max_claim_version
	// yet and must still fall through to the cold seed.
	if _, ok := out.Item[attrMaxClaimVersion]; ok {
		return numAttrU64(out.Item, attrMaxClaimVersion), nil
	}

	// Cold partition: seed the fence from existing records once.
	seed, err := s.maxClaimVersionByScan(ctx, partitionKey)
	if err != nil {
		return 0, err
	}
	if seed > 0 {
		if err := s.raiseFence(ctx, partitionKey, seed, s.clk.Now()); err != nil {
			return 0, err
		}
	}
	return seed, nil
}

// raiseFence raises the partition fence row's max_claim_version to version
// using a raise-only conditional write, so concurrent claims with a higher
// version are never clobbered by a lower one. The same write refreshes the
// fence row's TTL horizon (see fenceRetentionFloor).
func (s *Store) raiseFence(ctx context.Context, partitionKey string, version uint64, now time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		UpdateExpression:    aws.String("SET #mcv = :ver, #ttl = :ttl"),
		ConditionExpression: aws.String("attribute_not_exists(#mcv) OR #mcv < :ver"),
		ExpressionAttributeNames: map[string]string{
			"#mcv": attrMaxClaimVersion,
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ver": &ddbtypes.AttributeValueMemberN{Value: u64(version)},
			":ttl": &ddbtypes.AttributeValueMemberN{Value: i64(s.fenceTTLEpoch(now))},
		},
	})
	if err != nil {
		// Condition failure means a concurrent claim already advanced the
		// fence to >= version — the monotonic invariant still holds.
		if isConditionFailed(err) {
			return nil
		}
		return wrapErr(err, "outbox fence raise failed", "partitionKey", partitionKey)
	}
	return nil
}

// maxClaimVersionByScan computes the fence seed for a cold partition by
// scanning existing record rows (bounded to one full partition read; used
// once per partition to migrate to the fence row).
func (s *Store) maxClaimVersionByScan(ctx context.Context, partitionKey string) (uint64, error) {
	var maxVersion uint64
	var startKey map[string]ddbtypes.AttributeValue

	for {
		out, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			ExpressionAttributeNames: map[string]string{
				"#cv": "claim_version",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk":     &ddbtypes.AttributeValueMemberS{Value: partitionKey},
				":prefix": &ddbtypes.AttributeValueMemberS{Value: skPrefix},
			},
			ProjectionExpression: aws.String("#cv"),
			ConsistentRead:       aws.Bool(true),
			ExclusiveStartKey:    startKey,
		})
		if err != nil {
			return 0, wrapErr(err, "outbox max claim version query failed", "partitionKey", partitionKey)
		}

		for _, item := range out.Items {
			if v := numAttrU64(item, "claim_version"); v > maxVersion {
				maxVersion = v
			}
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	return maxVersion, nil
}

// resolveRecordKeys returns the base-table (PK, SK) for an application record
// ID. It first consults the key cache populated by Claim (the miss-free path),
// then falls back to the eventually consistent RecordIDIndex GSI with a
// bounded retry to absorb GSI replication lag — closing the not-found →
// duplicate-delivery window a single GSI read would leave open.
func (s *Store) resolveRecordKeys(ctx context.Context, recordID string) (string, string, error) {
	if k, ok := s.lookupKey(recordID); ok {
		return k.pk, k.sk, nil
	}

	var lastPK, lastSK string
	for attempt := 0; ; attempt++ {
		pk, sk, err := s.resolveRecordKeysFromGSI(ctx, recordID)
		if err != nil {
			return "", "", err
		}
		if pk != "" {
			return pk, sk, nil
		}
		lastPK, lastSK = pk, sk

		if attempt >= s.resolveMaxAttempts-1 {
			break
		}
		if err := s.sleep(ctx, s.resolveBackoff); err != nil {
			return "", "", err
		}
	}
	return lastPK, lastSK, nil
}

func (s *Store) resolveRecordKeysFromGSI(ctx context.Context, recordID string) (string, string, error) {
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(recordIDIndexName),
		KeyConditionExpression: aws.String("record_id = :id"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":id": &ddbtypes.AttributeValueMemberS{Value: recordID},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return "", "", wrapErr(err, "outbox resolve record keys failed", "recordID", recordID)
	}
	if len(out.Items) == 0 {
		return "", "", nil
	}
	return strAttr(out.Items[0], "PK"), strAttr(out.Items[0], "SK"), nil
}

// sleep blocks for d honoring context cancellation, using the injected
// clock so tests remain deterministic.
func (s *Store) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := s.clk.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

func (s *Store) cacheKey(recordID, pk, sk string) {
	s.keys.put(recordID, recordKey{pk: pk, sk: sk})
}

func (s *Store) lookupKey(recordID string) (recordKey, bool) {
	return s.keys.get(recordID)
}

func (s *Store) evictKey(recordID string) {
	s.keys.remove(recordID)
}
