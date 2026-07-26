package dynamodbrollout

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// dynamoAPI is the subset of the DynamoDB SDK client used by this package. The
// real *dynamodb.Client satisfies it; tests supply a mock to drive the decode /
// fail-closed paths without a live DynamoDB.
//
// The rollout store never uses UpdateItem: every write is a whole-item
// conditional PutItem (the aggregate is one row), which keeps the compare-and-set
// trivially atomic against the revision guard.
type dynamoAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

var _ dynamoAPI = (*dynamodb.Client)(nil)
