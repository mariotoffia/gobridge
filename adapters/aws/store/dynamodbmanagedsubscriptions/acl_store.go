// Package dynamodbmanagedsubscriptions persists exact durable MQTT
// topic-filter history in a dedicated DynamoDB table.
package dynamodbmanagedsubscriptions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	// DefaultTableName is the adapter default used when no table override is configured.
	DefaultTableName = "gobridge-managed-subscriptions"
	attrIdentity     = "storage_identity"
	attrBaseline     = "baseline"
	attrFilters      = "filters"
)

type dynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// Store is a DynamoDB-backed managed-subscription history.
type Store struct {
	client    dynamoAPI
	tableName string
}

type Option func(*Store)

// WithTableName overrides the default dedicated table name.
func WithTableName(name string) Option { return func(s *Store) { s.tableName = name } }

// NewStore wraps the AWS SDK client behind the adapter ACL.
//
//aclcheck:allow-export
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	store := &Store{client: client, tableName: DefaultTableName}
	for _, opt := range opts {
		opt(store)
	}
	return store
}

var _ ports.ManagedSubscriptionStore = (*Store)(nil)

// EnsureTable provisions the minimal one-hash-key table. Production
// composition uses Preflight; this method is for explicit provisioning/tests.
func (s *Store) EnsureTable(ctx context.Context) error {
	_, err := s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(s.tableName),
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String(attrIdentity), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String(attrIdentity), KeyType: ddbtypes.KeyTypeHash}},
		BillingMode:          ddbtypes.BillingModePayPerRequest,
	})
	var inUse *ddbtypes.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	return mapError("create managed subscription table", err)
}

// Preflight verifies the configured table exists with exactly the required key.
func (s *Store) Preflight(ctx context.Context) error {
	out, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.tableName)})
	if err != nil {
		var missing *ddbtypes.ResourceNotFoundException
		if errors.As(err, &missing) {
			return nil
		}
		return mapError("describe managed subscription table", err)
	}
	validDefinition := out.Table != nil && len(out.Table.AttributeDefinitions) == 1 &&
		aws.ToString(out.Table.AttributeDefinitions[0].AttributeName) == attrIdentity &&
		out.Table.AttributeDefinitions[0].AttributeType == ddbtypes.ScalarAttributeTypeS
	if out.Table == nil || !validDefinition || len(out.Table.KeySchema) != 1 || out.Table.KeySchema[0].KeyType != ddbtypes.KeyTypeHash || aws.ToString(out.Table.KeySchema[0].AttributeName) != attrIdentity {
		return shared.ErrInvalidConfig.
			WithMessage(fmt.Sprintf("dynamodbmanagedsubscriptions: table %q schema mismatch: expected one string HASH key named %q", s.tableName, attrIdentity)).
			With("table", s.tableName)
	}
	return nil
}

// List performs a strongly consistent read and sorts the exact filter set.
func (s *Store) List(ctx context.Context, storageIdentity string) ([]string, error) {
	if err := validate(storageIdentity, nil); err != nil {
		return nil, err
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName), ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{attrIdentity: &ddbtypes.AttributeValueMemberS{Value: storageIdentity}},
	})
	if err != nil {
		return nil, mapError("list managed subscriptions", err)
	}
	if len(out.Item) == 0 {
		return nil, shared.ErrNotFound.WithMessage("managed subscription baseline not found")
	}
	baselineValue, ok := out.Item[attrBaseline]
	if !ok {
		return nil, shared.ErrNotFound.WithMessage("managed subscription baseline not found")
	}
	baseline, ok := baselineValue.(*ddbtypes.AttributeValueMemberBOOL)
	if !ok || !baseline.Value {
		return nil, shared.ErrInvalidConfig.WithMessage("managed subscription DynamoDB item has invalid baseline marker")
	}
	filters := make([]string, 0)
	if value, ok := out.Item[attrFilters]; ok {
		set, ok := value.(*ddbtypes.AttributeValueMemberSS)
		if !ok {
			return nil, shared.ErrInvalidConfig.WithMessage("managed subscription DynamoDB item has invalid filters")
		}
		filters = append(filters, set.Value...)
	}
	sort.Strings(filters)
	return filters, nil
}

// Remember atomically creates the baseline and adds the exact filter set.
func (s *Store) Remember(ctx context.Context, storageIdentity string, filters []string) error {
	values, err := normalized(storageIdentity, filters)
	if err != nil {
		return err
	}
	names := map[string]string{"#baseline": attrBaseline}
	attrs := map[string]ddbtypes.AttributeValue{":true": &ddbtypes.AttributeValueMemberBOOL{Value: true}}
	expression := "SET #baseline = :true"
	if len(values) > 0 {
		names["#filters"] = attrFilters
		attrs[":filters"] = &ddbtypes.AttributeValueMemberSS{Value: values}
		expression += " ADD #filters :filters"
	}
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(s.tableName),
		Key:                      map[string]ddbtypes.AttributeValue{attrIdentity: &ddbtypes.AttributeValueMemberS{Value: storageIdentity}},
		ExpressionAttributeNames: names, ExpressionAttributeValues: attrs,
		UpdateExpression: aws.String(expression),
	})
	return mapError("remember managed subscriptions", err)
}

// Forget atomically removes exact filters and never removes the baseline.
func (s *Store) Forget(ctx context.Context, storageIdentity string, filters []string) error {
	values, err := normalized(storageIdentity, filters)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		_, err := s.List(ctx, storageIdentity)
		return err
	}
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.tableName),
		Key:                 map[string]ddbtypes.AttributeValue{attrIdentity: &ddbtypes.AttributeValueMemberS{Value: storageIdentity}},
		ConditionExpression: aws.String("attribute_exists(#identity) AND #baseline = :true"),
		UpdateExpression:    aws.String("DELETE #filters :filters"),
		ExpressionAttributeNames: map[string]string{
			"#identity": attrIdentity, "#baseline": attrBaseline, "#filters": attrFilters,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":true":    &ddbtypes.AttributeValueMemberBOOL{Value: true},
			":filters": &ddbtypes.AttributeValueMemberSS{Value: values},
		},
	})
	var condition *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &condition) {
		return shared.ErrNotFound.WithMessage("managed subscription baseline not found")
	}
	return mapError("forget managed subscriptions", err)
}

func normalized(identity string, filters []string) ([]string, error) {
	if err := validate(identity, filters); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(filters))
	values := make([]string, 0, len(filters))
	for _, filter := range filters {
		if _, ok := seen[filter]; ok {
			continue
		}
		seen[filter] = struct{}{}
		values = append(values, filter)
	}
	sort.Strings(values)
	return values, nil
}

func validate(identity string, filters []string) error {
	if identity == "" {
		return shared.ErrInvalidConfig.WithMessage("managed subscription storage identity is required")
	}
	for _, filter := range filters {
		if filter == "" {
			return shared.ErrInvalidConfig.WithMessage("managed subscription filter must not be empty")
		}
	}
	return nil
}

func mapError(message string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var notFound *ddbtypes.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return shared.ErrNotFound.WithMessage(message).Wrap(err)
	}
	var throttled *ddbtypes.ProvisionedThroughputExceededException
	if errors.As(err, &throttled) {
		return shared.ErrThrottled.WithMessage(message).Wrap(err)
	}
	var requestLimit *ddbtypes.RequestLimitExceeded
	if errors.As(err, &requestLimit) {
		return shared.ErrThrottled.WithMessage(message).Wrap(err)
	}
	var internal *ddbtypes.InternalServerError
	if errors.As(err, &internal) {
		return shared.ErrUnavailable.WithMessage(message).Wrap(err)
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "throttl") || strings.Contains(lower, "rate exceeded"):
		return shared.ErrThrottled.WithMessage(message).Wrap(err)
	case strings.Contains(lower, "expiredtoken") || strings.Contains(lower, "expired credentials"):
		return shared.ErrTemporaryAuthFailure.WithMessage(message).Wrap(err)
	case strings.Contains(lower, "accessdenied") || strings.Contains(lower, "notauthorized") || strings.Contains(lower, "unauthorized"):
		return shared.ErrNotAuthorized.WithMessage(message).Wrap(err)
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "no such host"):
		return shared.ErrConnectionLost.WithMessage(message).Wrap(err)
	default:
		return shared.ErrUnavailable.WithMessage(message).Wrap(err)
	}
}
