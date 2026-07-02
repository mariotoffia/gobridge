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
// with attribute_not_exists. If the item already exists, it attempts an expired
// takeover via UpdateItem with an expires_at < :now condition, atomically
// incrementing the version.
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
		return persistence.LeaseToken{Version: 1, Owner: ownerID}, nil
	}
	if !isConditionFailed(err) {
		return persistence.LeaseToken{}, wrapErr(err, "", "leaseID", leaseID, "ownerID", ownerID)
	}

	// Item exists -- attempt expired takeover with atomic version increment.
	updateExpr := "SET #own = :owner, #ver = #ver + :one, " +
		"#acq = :now_ms, #exp = :exp_ms, #ren = :now_ms"
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
	if len(endpoints) > 0 {
		updateExpr += ", #ep = :ep"
		exprNames["#ep"] = attrEndpoints
		exprValues[":ep"] = marshalEndpoints(endpoints)
	}
	// Strip any legacy ttl a pre-fix build may have stamped so a reaper can
	// never delete this actively-taken lease row (MF-1/J1). REMOVE on an
	// absent attribute is a harmless no-op on fresh rows.
	updateExpr += " REMOVE #ttl"
	exprNames["#ttl"] = attrTTL

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

	version, _ := numAttr(result.Item, attrVersion)
	expiresAtMillis, _ := numAttr(result.Item, attrExpiresAt)

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
