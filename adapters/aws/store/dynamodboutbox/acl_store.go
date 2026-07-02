package dynamodboutbox

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
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
	// holds the monotonic max_claim_version. It is deliberately outside the
	// OUTBOX# key space so it is invisible to record queries and, having no
	// GSI key attributes, to every GSI as well.
	fenceSK             = "FENCE"
	attrMaxClaimVersion = "max_claim_version"
)

// Store implements ports.OutboxStore using DynamoDB with partitioned
// persistence, conditional writes, and TTL-based compaction.
//
// Key design:
//
//	PK = "<partition_key>" (e.g. "SESSION#<session_id>" or "BINDING#<binding_id>")
//	SK = "OUTBOX#<envelope_id>#<binding_id>"
//
// The SK incorporates both envelope_id and binding_id so that:
//   - Fan-out records (same envelope, different bindings) have distinct keys
//   - Idempotent persist uses attribute_not_exists(SK) to reject duplicates
//     for the same envelope+binding combination across redeliveries
//
// A RecordIDIndex GSI on the record_id attribute enables O(1) lookup
// by application-level record ID for the Complete operation.
type Store struct {
	client         dynamoAPI
	table          string
	compactGrace   time.Duration
	staleClaim     time.Duration
	maxReplayCount int
	clk            clock.Clock
	logger         *slog.Logger

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
// be reclaimed by a new owner.
func WithStaleClaimDuration(d time.Duration) Option {
	return func(s *Store) { s.staleClaim = d }
}

// WithMaxReplayCount sets the maximum number of times a record may be
// claimed before it is excluded from future claims (poison message
// protection). A value of 0 means no limit. Default: routing.DefaultMaxReplayAttempts.
func WithMaxReplayCount(n int) Option {
	return func(s *Store) { s.maxReplayCount = n }
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
		client:         client,
		table:          defaultTableName,
		compactGrace:   defaultCompactionGrace,
		staleClaim:     defaultStaleClaimDuration,
		maxReplayCount: routing.DefaultMaxReplayAttempts,
		clk:            clock.System,

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
			{AttributeName: aws.String("status"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("created_at"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			{AttributeName: aws.String("claimed_by"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("claimed_at"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			{AttributeName: aws.String("record_id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String("StatusIndex"),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("status"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("created_at"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{
					ProjectionType:   ddbtypes.ProjectionTypeInclude,
					NonKeyAttributes: []string{"expires_at"},
				},
			},
			{
				IndexName: aws.String("ClaimedByIndex"),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("claimed_by"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("claimed_at"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
			},
			{
				IndexName: aws.String("RecordIDIndex"),
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
	// ExpiresAt and the per-partition FENCE metadata rows carry no ttl and are
	// never reaped. Best-effort: TTL is a housekeeping convenience, not a
	// correctness requirement, and is already enabled on re-runs.
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

// Persist writes outbox records atomically. For a single record, it uses
// PutItem with a condition to reject duplicates. For multiple records
// (fan-out), it uses TransactWriteItems to ensure atomicity.
func (s *Store) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: persist", "count", len(records))
	}

	if len(records) == 0 {
		return nil
	}

	now := s.clk.Now()

	if len(records) == 1 {
		return s.persistSingle(ctx, records[0], now)
	}
	return s.persistFanOut(ctx, records, now)
}

func (s *Store) persistSingle(ctx context.Context, r *persistence.OutboxRecord, now time.Time) error {
	item, err := marshalRecord(r, now, s.compactGrace)
	if err != nil {
		return err
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(SK)"),
	})
	if err != nil {
		if isConditionFailed(err) {
			return shared.ErrDuplicateRecord.
				WithMessage("duplicate outbox record").
				With("envelopeID", r.EnvelopeID()).
				With("bindingID", r.BindingID())
		}
		return wrapErr(err, "outbox persist failed", "envelopeID", r.EnvelopeID(), "bindingID", r.BindingID())
	}
	return nil
}

func (s *Store) persistFanOut(ctx context.Context, records []*persistence.OutboxRecord, now time.Time) error {
	if len(records) > 100 {
		return fmt.Errorf("dynamodboutbox: fan-out exceeds DynamoDB transaction limit of 100 items (%d)", len(records))
	}

	items := make([]ddbtypes.TransactWriteItem, 0, len(records))
	for i := range records {
		item, err := marshalRecord(records[i], now, s.compactGrace)
		if err != nil {
			return err
		}
		items = append(items, ddbtypes.TransactWriteItem{
			Put: &ddbtypes.Put{
				TableName:           aws.String(s.table),
				Item:                item,
				ConditionExpression: aws.String("attribute_not_exists(SK)"),
			},
		})
	}

	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	})
	if err != nil {
		if isTransactionCanceled(err) {
			return shared.ErrDuplicateRecord.
				WithMessage("duplicate outbox record in fan-out batch")
		}
		return wrapErr(err, "outbox persist fan-out failed", "recordCount", len(records))
	}
	return nil
}

// Claim finds pending or stale-claimed records for the given partition and
// claims them for the given owner with the provided fencing token.
// Uses strongly consistent reads with pagination to handle the DynamoDB
// Limit+Filter interaction (Limit caps evaluated items, not filtered results).
// Records that have exceeded the max replay count are skipped.
func (s *Store) Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: claim", "partition_key", partitionKey, "limit", limit)
	}

	now := s.clk.Now()
	staleThreshold := now.Add(-s.staleClaim)

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
	if err := s.raiseFence(ctx, partitionKey, token.Version); err != nil {
		return nil, err
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
		":stale":   &ddbtypes.AttributeValueMemberN{Value: i64(staleThreshold.UnixMilli())},
	}

	if s.maxReplayCount > 0 {
		filterExpr += " AND (attribute_not_exists(replay_count) OR replay_count < :maxReplay)"
		exprValues[":maxReplay"] = &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(s.maxReplayCount)}
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

			condExpr := "(#st = :pending) OR (#st = :cur_claimed AND (claim_version < :ver OR claimed_at < :stale))"
			condValues := map[string]ddbtypes.AttributeValue{
				":claimed":     &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":pending":     &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
				":cur_claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":owner":       &ddbtypes.AttributeValueMemberS{Value: token.Owner},
				":ver":         &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
				":now":         &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
				":stale":       &ddbtypes.AttributeValueMemberN{Value: i64(staleThreshold.UnixMilli())},
				":zero":        &ddbtypes.AttributeValueMemberN{Value: "0"},
				":one":         &ddbtypes.AttributeValueMemberN{Value: "1"},
			}

			if s.maxReplayCount > 0 {
				condExpr += " AND (attribute_not_exists(replay_count) OR replay_count < :maxReplay)"
				condValues[":maxReplay"] = &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(s.maxReplayCount)}
			}

			updateOut, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName: aws.String(s.table),
				Key: map[string]ddbtypes.AttributeValue{
					"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
					"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
				},
				UpdateExpression: aws.String(
					"SET #st = :claimed, claimed_by = :owner, claim_version = :ver, " +
						"claimed_at = :now, replay_count = if_not_exists(replay_count, :zero) + :one"),
				ConditionExpression:       aws.String(condExpr),
				ExpressionAttributeNames:  map[string]string{"#st": "status"},
				ExpressionAttributeValues: condValues,
				ReturnValues:              ddbtypes.ReturnValueAllNew,
			})
			if err != nil {
				if isConditionFailed(err) {
					continue
				}
				return nil, wrapErr(err, "outbox claim update failed", "partitionKey", pk, "ownerID", token.Owner)
			}

			rec, err := unmarshalRecord(updateOut.Attributes)
			if err != nil {
				return nil, err
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

	// Return claimed records in persisted created_at order with envelopeID
	// as a stable tiebreaker, so equal-millisecond timestamps are
	// deterministic and ordering matches the memory/sqlite backends.
	sort.Slice(claimed, func(i, j int) bool {
		ci, cj := claimed[i].CreatedAt(), claimed[j].CreatedAt()
		if ci.Equal(cj) {
			return claimed[i].EnvelopeID() < claimed[j].EnvelopeID()
		}
		return ci.Before(cj)
	})

	return claimed, nil
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
			UpdateExpression: aws.String(
				"SET #st = :completed, completed_at = :now, #ttl = :ttl"),
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

// Expire marks pending records whose ExpiresAt is before the given time as
// expired. Claimed records are never expired here. Returns the count.
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
			IndexName:              aws.String("StatusIndex"),
			KeyConditionExpression: aws.String("#st = :status"),
			FilterExpression:       aws.String("expires_at > :zero AND expires_at < :before"),
			ExpressionAttributeNames: map[string]string{
				"#st": "status",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":status": &ddbtypes.AttributeValueMemberS{Value: status},
				":zero":   &ddbtypes.AttributeValueMemberN{Value: "0"},
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
				UpdateExpression: aws.String("SET #st = :expired, #ttl = :ttl"),
				// Condition gates on the same status the query selected so the
				// guard cannot silently match zero rows if this is reused for
				// another status.
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

	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt().Before(records[j].CreatedAt())
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
// have no fence row. On such a cold partition the fence is seeded once from
// a bounded scan of the existing records' claim_version and persisted, so
// the O(N) scan happens at most once per partition ever (the fence row
// carries no TTL and therefore survives).
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
	if len(out.Item) > 0 {
		return numAttrU64(out.Item, attrMaxClaimVersion), nil
	}

	// Cold partition: seed the fence from existing records once.
	seed, err := s.maxClaimVersionByScan(ctx, partitionKey)
	if err != nil {
		return 0, err
	}
	if seed > 0 {
		if err := s.raiseFence(ctx, partitionKey, seed); err != nil {
			return 0, err
		}
	}
	return seed, nil
}

// raiseFence raises the partition fence row's max_claim_version to version
// using a raise-only conditional write, so concurrent claims with a higher
// version are never clobbered by a lower one.
func (s *Store) raiseFence(ctx context.Context, partitionKey string, version uint64) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		UpdateExpression:    aws.String("SET #mcv = :ver"),
		ConditionExpression: aws.String("attribute_not_exists(#mcv) OR #mcv < :ver"),
		ExpressionAttributeNames: map[string]string{
			"#mcv": attrMaxClaimVersion,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ver": &ddbtypes.AttributeValueMemberN{Value: u64(version)},
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
		IndexName:              aws.String("RecordIDIndex"),
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
