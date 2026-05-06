package dynamodblease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

const (
	defaultTableName   = "gobridge-leases"
	defaultGracePeriod = 1 * time.Hour

	attrPK         = "PK"
	attrOwner      = "owner"
	attrVersion    = "version"
	attrAcquiredAt = "acquired_at"
	attrExpiresAt  = "expires_at"
	attrRenewedAt  = "renewed_at"
	attrTTL        = "ttl"
	attrEndpoints  = "endpoints"
)

// Store implements ports.LeaseStore using DynamoDB conditional writes
// for fencing-safe lease management.
//
// Table schema (configurable via WithTableName):
//
//	PK (String): "LEASE#<lease_id>" -- partition key, no sort key
//	owner, version, acquired_at, expires_at, renewed_at, ttl
type Store struct {
	client      *dynamodb.Client
	tableName   string
	gracePeriod time.Duration
	clk         clock.Clock
	logger      *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-leases").
func WithTableName(name string) Option {
	return func(s *Store) { s.tableName = name }
}

// WithGracePeriod sets the TTL grace period added to expires_at for DynamoDB
// TTL-based item deletion (default: 1 hour).
func WithGracePeriod(d time.Duration) Option {
	return func(s *Store) { s.gracePeriod = d }
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
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:      client,
		tableName:   defaultTableName,
		gracePeriod: defaultGracePeriod,
		clk:         clock.System,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Acquire attempts to obtain a lease. It first tries a fresh acquire via PutItem
// with attribute_not_exists. If the item already exists, it attempts an expired
// takeover via UpdateItem with an expires_at < :now condition, atomically
// incrementing the version.
func (s *Store) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
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
		attrTTL:        &ddbtypes.AttributeValueMemberN{Value: epochStr(expiresAt.Add(s.gracePeriod))},
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
		return domain.LeaseToken{Version: 1, Owner: ownerID}, nil
	}
	if !isConditionFailed(err) {
		return domain.LeaseToken{}, wrapErr(err, "", "leaseID", leaseID, "ownerID", ownerID)
	}

	// Item exists -- attempt expired takeover with atomic version increment.
	updateExpr := "SET #own = :owner, #ver = #ver + :one, " +
		"#acq = :now_ms, #exp = :exp_ms, " +
		"#ren = :now_ms, #ttl_a = :ttl_epoch"
	exprNames := map[string]string{
		"#own":   attrOwner,
		"#ver":   attrVersion,
		"#acq":   attrAcquiredAt,
		"#exp":   attrExpiresAt,
		"#ren":   attrRenewedAt,
		"#ttl_a": attrTTL,
	}
	exprValues := map[string]ddbtypes.AttributeValue{
		":owner":     &ddbtypes.AttributeValueMemberS{Value: ownerID},
		":one":       &ddbtypes.AttributeValueMemberN{Value: "1"},
		":now_ms":    &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
		":exp_ms":    &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
		":ttl_epoch": &ddbtypes.AttributeValueMemberN{Value: epochStr(expiresAt.Add(s.gracePeriod))},
	}
	if len(endpoints) > 0 {
		updateExpr += ", #ep = :ep"
		exprNames["#ep"] = attrEndpoints
		exprValues[":ep"] = marshalEndpoints(endpoints)
	}

	result, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConditionExpression:       aws.String("#exp < :now_ms"),
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
		ReturnValues:              ddbtypes.ReturnValueAllNew,
	})
	if err != nil {
		if isConditionFailed(err) {
			return domain.LeaseToken{}, shared.ErrAlreadyExists.
				WithMessage("lease already held").
				With("leaseID", leaseID)
		}
		return domain.LeaseToken{}, wrapErr(err, "lease takeover update failed", "leaseID", leaseID, "ownerID", ownerID)
	}

	ver, err := numAttr(result.Attributes, attrVersion)
	if err != nil {
		return domain.LeaseToken{}, fmt.Errorf("dynamodblease: parse version from takeover result: %w", err)
	}
	return domain.LeaseToken{Version: ver, Owner: ownerID}, nil
}

// Renew extends the lease TTL. The caller's token must match the stored
// owner and version. The returned token keeps the same version.
func (s *Store) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodblease: renew", "lease_id", leaseID, "owner_id", token.Owner)
	}

	now := s.clk.Now()
	expiresAt := now.Add(ttl)
	pk := leaseKey(leaseID)

	updateExpr := "SET #exp = :exp_ms, #ren = :now_ms, #ttl_a = :ttl_epoch"
	exprNames := map[string]string{
		"#own":   attrOwner,
		"#ver":   attrVersion,
		"#exp":   attrExpiresAt,
		"#ren":   attrRenewedAt,
		"#ttl_a": attrTTL,
	}
	exprValues := map[string]ddbtypes.AttributeValue{
		":owner":     &ddbtypes.AttributeValueMemberS{Value: token.Owner},
		":ver":       &ddbtypes.AttributeValueMemberN{Value: uintStr(token.Version)},
		":exp_ms":    &ddbtypes.AttributeValueMemberN{Value: millisStr(expiresAt)},
		":now_ms":    &ddbtypes.AttributeValueMemberN{Value: millisStr(now)},
		":ttl_epoch": &ddbtypes.AttributeValueMemberN{Value: epochStr(expiresAt.Add(s.gracePeriod))},
	}
	if len(endpoints) > 0 {
		updateExpr += ", #ep = :ep"
		exprNames["#ep"] = attrEndpoints
		exprValues[":ep"] = marshalEndpoints(endpoints)
	}

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
			return domain.LeaseToken{}, s.classifyConditionFailure(ctx, leaseID)
		}
		return domain.LeaseToken{}, wrapErr(err, "lease renew update failed", "leaseID", leaseID, "ownerID", token.Owner)
	}
	return token, nil
}

// Release marks the lease as released by clearing the owner and setting
// expires_at to zero. The item is preserved so that the version counter
// remains available for monotonic increments on subsequent acquires.
// DynamoDB TTL will eventually remove the item after the grace period.
func (s *Store) Release(ctx context.Context, leaseID string, token domain.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodblease: release", "lease_id", leaseID)
	}

	pk := leaseKey(leaseID)
	now := s.clk.Now()

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConditionExpression: aws.String("#own = :owner AND #ver = :ver"),
		UpdateExpression: aws.String(
			"SET #own = :empty, #exp = :zero, #ttl_a = :ttl_epoch"),
		ExpressionAttributeNames: map[string]string{
			"#own":   attrOwner,
			"#ver":   attrVersion,
			"#exp":   attrExpiresAt,
			"#ttl_a": attrTTL,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner":     &ddbtypes.AttributeValueMemberS{Value: token.Owner},
			":ver":       &ddbtypes.AttributeValueMemberN{Value: uintStr(token.Version)},
			":empty":     &ddbtypes.AttributeValueMemberS{Value: ""},
			":zero":      &ddbtypes.AttributeValueMemberN{Value: "0"},
			":ttl_epoch": &ddbtypes.AttributeValueMemberN{Value: epochStr(now.Add(s.gracePeriod))},
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
func (s *Store) Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error) {
	pk := leaseKey(leaseID)

	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return domain.LeaseInfo{}, wrapErr(err, "lease get failed", "leaseID", leaseID)
	}
	owner := strAttr(result.Item, attrOwner)
	if len(result.Item) == 0 || owner == "" {
		return domain.LeaseInfo{}, shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	version, _ := numAttr(result.Item, attrVersion)
	expiresAtMillis, _ := numAttr(result.Item, attrExpiresAt)

	return domain.LeaseInfo{
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

func epochStr(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
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
