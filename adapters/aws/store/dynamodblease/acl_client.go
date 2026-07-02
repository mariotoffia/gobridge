package dynamodblease

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// dynamoAPI is the subset of the DynamoDB SDK client used by this package.
// The real *dynamodb.Client satisfies this interface; tests supply a mock.
//
// It doubles as the seam that lets unit tests assert the exact item shape
// written by the lease store (notably the absence of a TTL attribute on
// fencing-counter rows) without a live DynamoDB.
type dynamoAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	DescribeTimeToLive(ctx context.Context, params *dynamodb.DescribeTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
}

var _ dynamoAPI = (*dynamodb.Client)(nil)
