package dynamodb

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
)

// cursorDDB reuses the CAS fake under a mutex so peer writes can be ordered
// against a live watcher without Docker or racing the fake state.
type cursorDDB struct {
	mu sync.Mutex
	casFakeDDB
}

func (f *cursorDDB) GetItem(ctx context.Context, in *awsddb.GetItemInput, opts ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.casFakeDDB.GetItem(ctx, in, opts...)
}

func (f *cursorDDB) PutItem(ctx context.Context, in *awsddb.PutItemInput, opts ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.casFakeDDB.PutItem(ctx, in, opts...)
}

func (f *cursorDDB) DescribeTable(context.Context, *awsddb.DescribeTableInput, ...func(*awsddb.Options)) (*awsddb.DescribeTableOutput, error) {
	return &awsddb.DescribeTableOutput{Table: &ddbtypes.TableDescription{
		StreamSpecification: &ddbtypes.StreamSpecification{StreamEnabled: aws.Bool(true)},
		LatestStreamArn:     aws.String("arn:test-stream"),
	}}, nil
}

// cursorStreams gates each LATEST acquisition. GetRecords signals that the
// preceding reconciliation completed, then closes the shard to open another gap.
type cursorStreams struct {
	fakeStreams
	acquiring  chan struct{}
	resume     chan struct{}
	reconciled chan struct{}
}

func (f *cursorStreams) GetShardIterator(ctx context.Context, in *dynamodbstreams.GetShardIteratorInput, opts ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetShardIteratorOutput, error) {
	select {
	case f.acquiring <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-f.resume:
		return f.fakeStreams.GetShardIterator(ctx, in, opts...)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *cursorStreams) GetRecords(ctx context.Context, in *dynamodbstreams.GetRecordsInput, opts ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetRecordsOutput, error) {
	out, err := f.fakeStreams.GetRecords(ctx, in, opts...)
	select {
	case f.reconciled <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return out, err
}

var (
	_ ddbAPI     = (*cursorDDB)(nil)
	_ streamsAPI = (*cursorStreams)(nil)
)
