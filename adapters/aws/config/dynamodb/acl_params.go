package dynamodb

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Per-call SDK request/response translation for the DynamoDB
// configuration table.
//
// This file is the request/response half of the dynamodb config ACL:
// every dynamodb.*Input / *Output value the loader constructs or
// consumes lives here, alongside the only references to ddbtypes
// outside of error classification. loader.go calls these methods using
// plain Go types and never sees an SDK request struct.

// getConfigItem fetches the (PK, current) row. found is false when the
// row does not exist; in that case rawData is "" and version is 0.
func (s *session) getConfigItem(ctx context.Context, pk string) (rawData string, version int64, found bool, err error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
			attrSK: &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		},
		// Strong read: config Load/reload must observe the latest committed
		// version, otherwise a watcher can reload a stale document right
		// after an admin Save.
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return "", 0, false, fmt.Errorf("dynamodb config load: %w", err)
	}
	if out.Item == nil {
		return "", 0, false, nil
	}

	dataAttr, ok := out.Item[attrData].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", 0, false, fmt.Errorf("dynamodb config load: missing or invalid %q attribute", attrData)
	}

	if vAttr, ok := out.Item[attrVersion].(*ddbtypes.AttributeValueMemberN); ok {
		if v, err := strconv.ParseInt(vAttr.Value, 10, 64); err == nil {
			version = v
		}
	}

	return dataAttr.Value, version, true, nil
}

// getCurrentVersion returns just the version attribute of the
// (PK, current) row using a projection expression. A missing row
// yields version 0 with no error so callers can distinguish absent
// from mismatched.
func (s *session) getCurrentVersion(ctx context.Context, pk string) (int64, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
			attrSK: &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		},
		ProjectionExpression: aws.String("#v"),
		ExpressionAttributeNames: map[string]string{
			"#v": attrVersion,
		},
		// Strong read so the poll-mode change detector and the Save CAS see
		// the latest committed version.
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return 0, fmt.Errorf("dynamodb config current version: %w", err)
	}
	if out.Item == nil {
		return 0, nil
	}

	vAttr, ok := out.Item[attrVersion].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	v, err := strconv.ParseInt(vAttr.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dynamodb config current version: parse %q: %w", vAttr.Value, err)
	}
	return v, nil
}

// putConfigItem writes the (PK, current) row with the supplied JSON
// payload and version, guarded by a compare-and-set condition:
// the write succeeds only if the row is absent (first write) or its
// stored version equals expectedVersion. On a version-conflict the SDK
// ConditionalCheckFailedException is returned unwrapped so the loader
// can classify it as a lost-update conflict.
func (s *session) putConfigItem(ctx context.Context, pk string, data []byte, version, expectedVersion int64) error {
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &s.tableName,
		Item: map[string]ddbtypes.AttributeValue{
			attrPK:      &ddbtypes.AttributeValueMemberS{Value: pk},
			attrSK:      &ddbtypes.AttributeValueMemberS{Value: skCurrent},
			attrData:    &ddbtypes.AttributeValueMemberS{Value: string(data)},
			attrVersion: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(#pk) OR #v = :expected"),
		ExpressionAttributeNames: map[string]string{
			"#pk": attrPK,
			"#v":  attrVersion,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":expected": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expectedVersion, 10)},
		},
	})
	if err != nil {
		if isConditionFailed(err) {
			return err
		}
		return fmt.Errorf("dynamodb config save: %w", err)
	}
	return nil
}

// describeStreamArn returns the table's LatestStreamArn when streams
// are enabled and usable. The reason is a non-empty human-readable
// explanation when streams cannot be used; when reason is empty the
// arn is valid.
func (s *session) describeStreamArn(ctx context.Context) (arn string, reason string, err error) {
	if s.streams == nil {
		return "", "streams client not configured (use WithStreamsClient)", nil
	}
	out, derr := s.ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: &s.tableName,
	})
	if derr != nil {
		return "", fmt.Sprintf("describe table: %v", derr), nil
	}
	if out.Table == nil {
		return "", "describe table returned no table", nil
	}
	spec := out.Table.StreamSpecification
	if spec == nil || spec.StreamEnabled == nil || !*spec.StreamEnabled {
		return "", "stream not enabled on table", nil
	}
	if out.Table.LatestStreamArn == nil || *out.Table.LatestStreamArn == "" {
		return "", "table has no LatestStreamArn", nil
	}
	return *out.Table.LatestStreamArn, "", nil
}

// ensureTable creates the configuration table if it does not already
// exist. ResourceInUseException (table already present) is swallowed
// via mapEnsureTableError.
func (s *session) ensureTable(ctx context.Context) error {
	_, err := s.ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: &s.tableName,
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(attrPK), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String(attrSK), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(attrPK), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrSK), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err := mapEnsureTableError(err); err != nil {
		return fmt.Errorf("dynamodb config ensure table: %w", err)
	}
	return nil
}

// waitTableExists blocks until the configured table is ACTIVE, up to
// the supplied timeout. Uses the SDK's NewTableExistsWaiter under the
// hood.
func (s *session) waitTableExists(ctx context.Context, timeout time.Duration) error {
	waiterClient := dynamodb.DescribeTableAPIClient(s.ddb)
	if s.ddbConcrete != nil {
		waiterClient = s.ddbConcrete
	}
	waiter := dynamodb.NewTableExistsWaiter(waiterClient)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: &s.tableName,
	}, timeout); err != nil {
		return fmt.Errorf("dynamodb config ensure table: wait: %w", err)
	}
	return nil
}
