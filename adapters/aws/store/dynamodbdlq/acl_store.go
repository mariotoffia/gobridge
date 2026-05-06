package dynamodbdlq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

const (
	defaultTableName   = "gobridge-dlq"
	defaultGracePeriod = 1 * time.Hour

	attrPK            = "PK"
	attrRouteID       = "route_id"
	attrBindingID     = "binding_id"
	attrSessionID     = "session_id"
	attrSourceID      = "source_id"
	attrCorrelationID = "correlation_id"
	attrReason        = "reason"
	attrCategory      = "category"
	attrErrorCode     = "error_code"
	attrLastError     = "last_error"
	attrEnvelopeJSON  = "envelope_json"
	attrFailedAt      = "failed_at"
	attrAttempts      = "attempts"
	attrTTL           = "ttl"
)

// Store implements ports.DLQStore using DynamoDB with conditional writes
// for idempotent entry creation and GSI-backed queries for listing by
// route and category.
type Store struct {
	client      *dynamodb.Client
	tableName   string
	gracePeriod time.Duration
	logger      *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-dlq").
func WithTableName(name string) Option {
	return func(s *Store) { s.tableName = name }
}

// WithGracePeriod sets the TTL grace period added to failed_at for DynamoDB
// TTL-based item deletion (default: 1 hour).
func WithGracePeriod(d time.Duration) Option {
	return func(s *Store) { s.gracePeriod = d }
}

// WithLogger sets the structured logger for trace/debug diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// NewStore creates a new DynamoDB-backed DLQStore.
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:      client,
		tableName:   defaultTableName,
		gracePeriod: defaultGracePeriod,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// EnsureTable creates the DynamoDB table with the required schema and GSIs.
// It is idempotent: if the table already exists, it returns nil.
func (s *Store) EnsureTable(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "dynamodbdlq: create_table")
	}

	_, err := s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(s.tableName),
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(attrPK), KeyType: ddbtypes.KeyTypeHash},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(attrPK), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrRouteID), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrCategory), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrFailedAt), AttributeType: ddbtypes.ScalarAttributeTypeN},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String("RouteIndex"),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String(attrRouteID), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String(attrFailedAt), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
			{
				IndexName: aws.String("CategoryIndex"),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String(attrCategory), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String(attrFailedAt), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		if isResourceInUse(err) {
			return nil
		}
		return wrapErr(err, "create dlq table failed", "table", s.tableName)
	}

	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	}, 2*time.Minute); err != nil {
		return wrapErr(err, "wait for dlq table to exist failed", "table", s.tableName)
	}
	return nil
}

// Write stores a DLQ entry. The write is idempotent: if an entry with the
// same ID already exists, shared.ErrDuplicateRecord is returned.
func (s *Store) Write(ctx context.Context, entry routing.DLQEntry) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: write",
			"entry_id", entry.ID, "route_id", entry.RouteID, "category", entry.Category)
	}

	envJSON, err := json.Marshal(&entry.Envelope)
	if err != nil {
		return fmt.Errorf("dynamodbdlq: marshal envelope: %w", err)
	}

	failedAtMs := entry.FailedAt.UnixMilli()
	ttlEpoch := entry.FailedAt.Add(s.gracePeriod).Unix()

	item := map[string]ddbtypes.AttributeValue{
		attrPK:            &ddbtypes.AttributeValueMemberS{Value: dlqKey(entry.ID)},
		attrRouteID:       &ddbtypes.AttributeValueMemberS{Value: entry.RouteID},
		attrBindingID:     &ddbtypes.AttributeValueMemberS{Value: entry.BindingID},
		attrSessionID:     &ddbtypes.AttributeValueMemberS{Value: entry.SessionID},
		attrSourceID:      &ddbtypes.AttributeValueMemberS{Value: entry.SourceID},
		attrCorrelationID: &ddbtypes.AttributeValueMemberS{Value: entry.CorrelationID},
		attrReason:        &ddbtypes.AttributeValueMemberS{Value: entry.Reason},
		attrCategory:      &ddbtypes.AttributeValueMemberS{Value: entry.Category},
		attrErrorCode:     &ddbtypes.AttributeValueMemberS{Value: entry.ErrorCode},
		attrLastError:     &ddbtypes.AttributeValueMemberS{Value: entry.LastError},
		attrEnvelopeJSON:  &ddbtypes.AttributeValueMemberS{Value: string(envJSON)},
		attrFailedAt:      &ddbtypes.AttributeValueMemberN{Value: i64(failedAtMs)},
		attrAttempts:      &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(entry.Attempts)},
		attrTTL:           &ddbtypes.AttributeValueMemberN{Value: i64(ttlEpoch)},
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.tableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{
			"#pk": attrPK,
		},
	})
	if err != nil {
		if isConditionFailed(err) {
			return shared.ErrDuplicateRecord.
				WithMessage("duplicate DLQ entry").
				With("entryID", entry.ID)
		}
		return wrapErr(err, "dlq write failed", "entryID", entry.ID, "routeID", entry.RouteID)
	}
	return nil
}

// List queries DLQ entries matching the given filter. The query path is
// chosen based on which filter fields are set:
//   - RouteID only  → RouteIndex GSI
//   - Category only → CategoryIndex GSI
//   - Both          → RouteIndex GSI with post-filter on category
//   - Neither       → full table Scan
func (s *Store) List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: list",
			"route_id", filter.RouteID, "category", filter.Category, "limit", filter.Limit)
	}

	switch {
	case filter.RouteID != "":
		return s.listByIndex(ctx, "RouteIndex", attrRouteID, filter.RouteID, filter)
	case filter.Category != "":
		return s.listByIndex(ctx, "CategoryIndex", attrCategory, filter.Category, filter)
	default:
		return s.listByScan(ctx, filter)
	}
}

func (s *Store) listByIndex(
	ctx context.Context,
	indexName, pkAttr, pkValue string,
	filter routing.DLQFilter,
) ([]routing.DLQEntry, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	keyExpr := "#pk_attr = :pk_val"
	exprNames := map[string]string{"#pk_attr": pkAttr}
	exprValues := map[string]ddbtypes.AttributeValue{
		":pk_val": &ddbtypes.AttributeValueMemberS{Value: pkValue},
	}

	// DynamoDB KeyConditionExpression supports at most one comparison on
	// the sort key, and sort key attributes cannot appear in
	// FilterExpression. When both Since and Before are set, use BETWEEN
	// with Before-1ms to achieve a half-open range [Since, Before).
	var filterParts []string

	if !filter.Since.IsZero() && !filter.Before.IsZero() {
		// Both bounds: BETWEEN :since AND :before_excl (Before - 1ms for half-open range).
		keyExpr += " AND #fa BETWEEN :since AND :before_excl"
		exprNames["#fa"] = attrFailedAt
		exprValues[":since"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Since.UnixMilli())}
		exprValues[":before_excl"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Before.UnixMilli() - 1)}
	} else if !filter.Since.IsZero() {
		keyExpr += " AND #fa >= :since"
		exprNames["#fa"] = attrFailedAt
		exprValues[":since"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Since.UnixMilli())}
	} else if !filter.Before.IsZero() {
		keyExpr += " AND #fa < :before"
		exprNames["#fa"] = attrFailedAt
		exprValues[":before"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Before.UnixMilli())}
	}

	// When querying RouteIndex but category is also set, post-filter on category.
	if filter.RouteID != "" && filter.Category != "" && pkAttr == attrRouteID {
		filterParts = append(filterParts, "#cat = :cat_val")
		exprNames["#cat"] = attrCategory
		exprValues[":cat_val"] = &ddbtypes.AttributeValueMemberS{Value: filter.Category}
	}

	var filterExpr *string
	if len(filterParts) > 0 {
		filterExpr = aws.String(strings.Join(filterParts, " AND "))
	}

	var entries []routing.DLQEntry
	var startKey map[string]ddbtypes.AttributeValue

	for len(entries) < limit {
		input := &dynamodb.QueryInput{
			TableName:                 aws.String(s.tableName),
			IndexName:                 aws.String(indexName),
			KeyConditionExpression:    aws.String(keyExpr),
			ExpressionAttributeNames:  exprNames,
			ExpressionAttributeValues: exprValues,
		}
		if filterExpr != nil {
			input.FilterExpression = filterExpr
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "dlq list query failed", "index", indexName, "pkAttr", pkAttr)
		}

		for _, item := range out.Items {
			entry, err := unmarshalEntry(item)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].FailedAt.Before(entries[j].FailedAt)
	})

	if len(entries) > limit {
		entries = entries[:limit]
	}

	return entries, nil
}

func (s *Store) listByScan(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	var filterParts []string
	exprNames := map[string]string{}
	exprValues := map[string]ddbtypes.AttributeValue{}

	if !filter.Since.IsZero() {
		filterParts = append(filterParts, "#fa >= :since")
		exprNames["#fa"] = attrFailedAt
		exprValues[":since"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Since.UnixMilli())}
	}
	if !filter.Before.IsZero() {
		filterParts = append(filterParts, "#fa < :before")
		if exprNames["#fa"] == "" {
			exprNames["#fa"] = attrFailedAt
		}
		exprValues[":before"] = &ddbtypes.AttributeValueMemberN{Value: i64(filter.Before.UnixMilli())}
	}

	var filterExpr *string
	if len(filterParts) > 0 {
		filterExpr = aws.String(strings.Join(filterParts, " AND "))
	}

	var entries []routing.DLQEntry
	var startKey map[string]ddbtypes.AttributeValue

	for len(entries) < limit {
		input := &dynamodb.ScanInput{
			TableName: aws.String(s.tableName),
		}
		if filterExpr != nil {
			input.FilterExpression = filterExpr
			input.ExpressionAttributeNames = exprNames
			input.ExpressionAttributeValues = exprValues
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Scan(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "dlq list scan failed", "table", s.tableName)
		}

		for _, item := range out.Items {
			entry, err := unmarshalEntry(item)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].FailedAt.Before(entries[j].FailedAt)
	})

	if len(entries) > limit {
		entries = entries[:limit]
	}

	return entries, nil
}

// Get retrieves a single DLQ entry by ID.
// Returns shared.ErrNotFound if the entry does not exist.
func (s *Store) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: get", "entry_id", id)
	}

	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: dlqKey(id)},
		},
	})
	if err != nil {
		return routing.DLQEntry{}, wrapErr(err, "dlq get failed", "entryID", id)
	}
	if out.Item == nil {
		return routing.DLQEntry{}, shared.ErrNotFound.
			WithMessage("dlq entry not found").
			With("entryID", id)
	}

	return unmarshalEntry(out.Item)
}

// Delete removes specific DLQ entries by ID. Returns the count of
// entries actually deleted. Missing IDs are silently skipped.
func (s *Store) Delete(ctx context.Context, ids []string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: delete", "count", len(ids))
	}

	var count int
	for _, id := range ids {
		_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.tableName),
			Key: map[string]ddbtypes.AttributeValue{
				attrPK: &ddbtypes.AttributeValueMemberS{Value: dlqKey(id)},
			},
			ConditionExpression: aws.String("attribute_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{
				"#pk": attrPK,
			},
		})
		if err != nil {
			if isConditionFailed(err) {
				continue // entry doesn't exist — skip
			}
			return count, wrapErr(err, "dlq delete failed", "entryID", id)
		}
		count++
	}

	return count, nil
}

// DeleteByFilter removes all DLQ entries matching the filter criteria.
// Returns the count of entries deleted.
func (s *Store) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: delete_by_filter",
			"route_id", filter.RouteID, "category", filter.Category)
	}

	entries, err := s.List(ctx, filter)
	if err != nil {
		return 0, err
	}

	var count int
	for _, e := range entries {
		_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.tableName),
			Key: map[string]ddbtypes.AttributeValue{
				attrPK: &ddbtypes.AttributeValueMemberS{Value: dlqKey(e.ID)},
			},
		})
		if err != nil {
			return count, wrapErr(err, "dlq delete_by_filter delete failed", "entryID", e.ID)
		}
		count++
	}

	return count, nil
}

// Purge deletes entries whose failed_at is before the given time.
// Returns the count of deleted items.
func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: purge", "before", before)
	}

	beforeMs := before.UnixMilli()
	count := 0

	var startKey map[string]ddbtypes.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:        aws.String(s.tableName),
			FilterExpression: aws.String("#fa < :before"),
			ExpressionAttributeNames: map[string]string{
				"#fa": attrFailedAt,
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":before": &ddbtypes.AttributeValueMemberN{Value: i64(beforeMs)},
			},
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Scan(ctx, input)
		if err != nil {
			return count, wrapErr(err, "dlq purge scan failed", "table", s.tableName)
		}

		for _, item := range out.Items {
			pk := strAttr(item, attrPK)

			_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(s.tableName),
				Key: map[string]ddbtypes.AttributeValue{
					attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
				},
			})
			if err != nil {
				return count, wrapErr(err, "dlq purge delete failed", "pk", pk)
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
