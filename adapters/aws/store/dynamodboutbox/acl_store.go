package dynamodboutbox

import (
	"context"
	"encoding/json"
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

	skPrefix = "OUTBOX#"
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
	client         *dynamodb.Client
	table          string
	compactGrace   time.Duration
	staleClaim     time.Duration
	maxReplayCount int
	clk            clock.Clock
	logger         *slog.Logger
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

// NewStore creates a DynamoDB-backed OutboxStore.
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:         client,
		table:          defaultTableName,
		compactGrace:   defaultCompactionGrace,
		staleClaim:     defaultStaleClaimDuration,
		maxReplayCount: routing.DefaultMaxReplayAttempts,
		clk:            clock.System,
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
				With("envelopeID", r.EnvelopeID).
				With("bindingID", r.BindingID)
		}
		return wrapErr(err, "outbox persist failed", "envelopeID", r.EnvelopeID, "bindingID", r.BindingID)
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
func (s *Store) Claim(ctx context.Context, partitionKey string, ownerID string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: claim", "partition_key", partitionKey, "limit", limit)
	}

	now := s.clk.Now()
	staleThreshold := now.Add(-s.staleClaim)

	filterExpr := "(#st = :pending) OR (#st = :claimed AND claimed_at < :stale)"
	exprNames := map[string]string{"#st": "status"}
	exprValues := map[string]ddbtypes.AttributeValue{
		":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
		":prefix":  &ddbtypes.AttributeValueMemberS{Value: skPrefix},
		":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
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

			condExpr := "(#st = :pending) OR (#st = :cur_claimed AND claimed_at < :stale)"
			condValues := map[string]ddbtypes.AttributeValue{
				":claimed":     &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":pending":     &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
				":cur_claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":owner":       &ddbtypes.AttributeValueMemberS{Value: ownerID},
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
				return nil, wrapErr(err, "outbox claim update failed", "partitionKey", pk, "ownerID", ownerID)
			}

			rec, err := unmarshalRecord(updateOut.Attributes)
			if err != nil {
				return nil, err
			}
			claimed = append(claimed, rec)
		}

		if queryOut.LastEvaluatedKey == nil {
			break
		}
		startKey = queryOut.LastEvaluatedKey
	}

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
			return shared.ErrNotFound.
				WithMessage("outbox record not found").
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
				"claimed_by = :owner AND claim_version = :ver"),
			ExpressionAttributeNames: map[string]string{
				"#st":  "status",
				"#ttl": "ttl",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":completed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxCompleted)},
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
	}

	return nil
}

// Expire marks pending or claimed records whose ExpiresAt is before the given
// time as expired. Returns the count of expired records.
func (s *Store) Expire(ctx context.Context, before time.Time) (int, error) {
	beforeMs := before.UnixMilli()
	ttlEpoch := before.Add(s.compactGrace).Unix()
	count := 0

	for _, status := range []string{string(persistence.OutboxPending), string(persistence.OutboxClaimed)} {
		n, err := s.expireByStatus(ctx, status, beforeMs, ttlEpoch)
		if err != nil {
			return count, err
		}
		count += n
	}

	return count, nil
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
				ConditionExpression: aws.String(
					"(#st = :pending OR #st = :claimed) AND expires_at > :zero AND expires_at < :before"),
				ExpressionAttributeNames: map[string]string{
					"#st":  "status",
					"#ttl": "ttl",
				},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":expired": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxExpired)},
					":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
					":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
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
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	if len(records) > limit {
		records = records[:limit]
	}

	return records, nil
}

// resolveRecordKeys uses the RecordIDIndex GSI to find the base table PK/SK
// for a given application-level record ID.
func (s *Store) resolveRecordKeys(ctx context.Context, recordID string) (string, string, error) {
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

// --- marshaling ---

func sortKey(envelopeID, bindingID string) string {
	return skPrefix + envelopeID + "#" + bindingID
}

func partitionKey(r *persistence.OutboxRecord) string {
	return persistence.OutboxPartitionKey(r.SessionID, r.BindingID)
}

func marshalRecord(r *persistence.OutboxRecord, now time.Time, compactGrace time.Duration) (map[string]ddbtypes.AttributeValue, error) {
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	envJSON, err := json.Marshal(&r.Envelope)
	if err != nil {
		return nil, fmt.Errorf("dynamodboutbox: marshal envelope: %w", err)
	}

	item := map[string]ddbtypes.AttributeValue{
		"PK":            &ddbtypes.AttributeValueMemberS{Value: partitionKey(r)},
		"SK":            &ddbtypes.AttributeValueMemberS{Value: sortKey(r.EnvelopeID, r.BindingID)},
		"record_id":     &ddbtypes.AttributeValueMemberS{Value: r.ID},
		"route_id":      &ddbtypes.AttributeValueMemberS{Value: r.RouteID},
		"envelope_id":   &ddbtypes.AttributeValueMemberS{Value: r.EnvelopeID},
		"binding_id":    &ddbtypes.AttributeValueMemberS{Value: r.BindingID},
		"session_id":    &ddbtypes.AttributeValueMemberS{Value: r.SessionID},
		"address":       &ddbtypes.AttributeValueMemberS{Value: r.Address},
		"envelope_json": &ddbtypes.AttributeValueMemberS{Value: string(envJSON)},
		"status":        &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		"claim_version": &ddbtypes.AttributeValueMemberN{Value: "0"},
		"replay_count":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		"created_at":    &ddbtypes.AttributeValueMemberN{Value: i64(createdAt.UnixMilli())},
		"expires_at":    &ddbtypes.AttributeValueMemberN{Value: i64(millisOrZero(r.ExpiresAt))},
		"completed_at":  &ddbtypes.AttributeValueMemberN{Value: "0"},
	}
	// Omit claimed_by and claimed_at for unclaimed records — DynamoDB
	// sparse GSI semantics: items without the GSI key attributes are
	// excluded from the ClaimedByIndex, which is exactly what we want.

	if r.DispatchHeaders != nil {
		hdrJSON, err := json.Marshal(r.DispatchHeaders)
		if err != nil {
			return nil, fmt.Errorf("dynamodboutbox: marshal headers: %w", err)
		}
		item["headers_json"] = &ddbtypes.AttributeValueMemberS{Value: string(hdrJSON)}
	}

	if !r.ExpiresAt.IsZero() {
		item["ttl"] = &ddbtypes.AttributeValueMemberN{
			Value: i64(r.ExpiresAt.Add(compactGrace).Unix()),
		}
	}

	return item, nil
}

func unmarshalRecord(item map[string]ddbtypes.AttributeValue) (*persistence.OutboxRecord, error) {
	snap := persistence.OutboxSnapshot{
		ID:           strAttr(item, "record_id"),
		RouteID:      strAttr(item, "route_id"),
		EnvelopeID:   strAttr(item, "envelope_id"),
		BindingID:    strAttr(item, "binding_id"),
		SessionID:    strAttr(item, "session_id"),
		Address:      strAttr(item, "address"),
		Status:       persistence.OutboxStatus(strAttr(item, "status")),
		ClaimedBy:    strAttr(item, "claimed_by"),
		ClaimVersion: numAttrU64(item, "claim_version"),
		ReplayCount:  int(numAttrI64(item, "replay_count")),
		CreatedAt:    timeFromMillis(numAttrI64(item, "created_at")),
		CompletedAt:  timeFromMillis(numAttrI64(item, "completed_at")),
	}

	if expiresMs := numAttrI64(item, "expires_at"); expiresMs > 0 {
		snap.ExpiresAt = timeFromMillis(expiresMs)
	}

	if envJSON := strAttr(item, "envelope_json"); envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &snap.Envelope); err != nil {
			return nil, fmt.Errorf("dynamodboutbox: unmarshal envelope: %w", err)
		}
	}

	if hdrJSON := strAttr(item, "headers_json"); hdrJSON != "" {
		if err := json.Unmarshal([]byte(hdrJSON), &snap.DispatchHeaders); err != nil {
			return nil, fmt.Errorf("dynamodboutbox: unmarshal headers: %w", err)
		}
	}

	return persistence.RehydrateFromSnapshot(snap), nil
}

// --- attribute helpers ---

func strAttr(item map[string]ddbtypes.AttributeValue, key string) string {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func numAttrI64(item map[string]ddbtypes.AttributeValue, key string) int64 {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		return n
	}
	return 0
}

func numAttrU64(item map[string]ddbtypes.AttributeValue, key string) uint64 {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberN); ok {
		n, _ := strconv.ParseUint(v.Value, 10, 64)
		return n
	}
	return 0
}

func i64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func u64(n uint64) string {
	return strconv.FormatUint(n, 10)
}

func millisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeFromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
