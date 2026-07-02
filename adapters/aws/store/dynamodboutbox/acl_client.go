package dynamodboutbox

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// dynamoAPI is the subset of the DynamoDB SDK client used by this package.
// The real *dynamodb.Client satisfies this interface; tests supply a mock.
//
// It is the seam that lets unit tests drive the outbox store's control
// flow (fence metadata, GSI-miss retries, key caching) without a live
// DynamoDB backend.
//
// store legitimately calls; splitting it would not reduce the real surface.
//
//nolint:interfacebloat // SDK-boundary seam: one method per DynamoDB API the
type dynamoAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	TransactWriteItems(ctx context.Context, params *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	UpdateTimeToLive(ctx context.Context, params *dynamodb.UpdateTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
}

var _ dynamoAPI = (*dynamodb.Client)(nil)
