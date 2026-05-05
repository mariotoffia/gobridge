package dynamodb

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// ddbAPI is the unexported subset of the DynamoDB SDK client used by
// the loader. The real *dynamodb.Client satisfies this interface; tests
// supply a mock.
//
// This file is the lifecycle/orchestration half of the DynamoDB config
// ACL: every AWS SDK construction the adapter performs flows through
// this file. Per-call request/response translation lives in
// acl_params.go, the streams-side SDK boundary lives in acl_streams.go,
// and SDK error classification lives in acl_errors.go.
type ddbAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// streamsAPI is the unexported subset of the DynamoDB Streams SDK
// client used by the loader. *dynamodbstreams.Client satisfies it
// structurally so tests can substitute an in-memory fake.
type streamsAPI interface {
	DescribeStream(ctx context.Context, params *dynamodbstreams.DescribeStreamInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.DescribeStreamOutput, error)
	GetShardIterator(ctx context.Context, params *dynamodbstreams.GetShardIteratorInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetShardIteratorOutput, error)
	GetRecords(ctx context.Context, params *dynamodbstreams.GetRecordsInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetRecordsOutput, error)
}

var (
	_ ddbAPI     = (*dynamodb.Client)(nil)
	_ streamsAPI = (*dynamodbstreams.Client)(nil)
)

// session wraps the SDK client handles. It is the only place in the
// package allowed to import the AWS SDK constructors. Loader holds a
// *session and never touches SDK types directly outside ACL files.
//
// ddbConcrete retains the concrete *dynamodb.Client when one was
// supplied so EnsureTable can drive NewTableExistsWaiter (which
// requires the concrete type). When tests construct a session with a
// mock ddbAPI, ddbConcrete is nil and the waiter falls back to using
// ddb directly (best-effort; tests typically do not exercise the
// waiter path).
type session struct {
	ddb         ddbAPI
	ddbConcrete *dynamodb.Client
	streams     streamsAPI
	tableName   string
}

// newSession builds a session bound to a concrete DynamoDB client and
// table. Tests construct &session{...} literals with fakes directly.
func newSession(client *dynamodb.Client, tableName string) *session {
	return &session{
		ddb:         client,
		ddbConcrete: client,
		tableName:   tableName,
	}
}

// NewLoader creates a DynamoDB-backed ports.Reloader.
func NewLoader(client *dynamodb.Client, opts ...Option) *Loader {
	l := &Loader{
		session:            newSession(client, defaultTableName),
		bridgeID:           defaultBridgeID,
		pollInterval:       defaultPollInterval,
		streamPollInterval: defaultStreamPollInterval,
		mode:               ModeStreams,
		clk:                clock.System,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// WithStreamsClient configures the DynamoDB Streams client used by
// ModeStreams. If not set, Watch falls back to ModePoll with a warning.
func WithStreamsClient(c *dynamodbstreams.Client) Option {
	return func(l *Loader) {
		if c != nil {
			l.session.streams = c
		}
	}
}
