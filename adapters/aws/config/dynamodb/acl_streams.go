package dynamodb

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	dstreamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/mariotoffia/gobridge/ports"
)

// Streams-side SDK boundary.
//
// This file is the streams half of the dynamodb config ACL: it owns
// the only references to dynamodbstreams.* / dstreamtypes.* outside of
// the session interface declaration. The streamLoop orchestration body
// also lives here so the SDK record types never escape ACL files.

// streamLoop consumes DynamoDB Streams records for the watched table
// and emits parsed BridgeConfig values on ch when a modification
// matches the loader's PK/SK. The loop owns ch and closes it on exit.
//
// Shard handling is intentionally simple: the configuration table is
// expected to have a single active shard most of the time. When the
// current shard closes (NextShardIterator == nil) or the iterator
// expires (GetRecords returns an error), the loop re-discovers the
// latest open shard on the next iteration. Cadence between GetRecords
// calls is governed by streamPollInterval through the injected clock,
// so tests can drive it deterministically.
func (l *Loader) streamLoop(ctx context.Context, ch chan<- *ports.BridgeConfig, streamArn string) {
	defer close(ch)

	var shardIter string
	for {
		if shardIter == "" {
			iter, err := l.session.acquireLatestIterator(ctx, streamArn)
			if err != nil {
				if l.logger != nil {
					l.logger.Warn("dynamodb config loader: stream shard discovery failed",
						"error", err,
						"stream_arn", streamArn,
					)
				}
				if !l.waitTick(ctx) {
					return
				}
				continue
			}
			if iter == "" {
				if !l.waitTick(ctx) {
					return
				}
				continue
			}
			shardIter = iter
		}

		records, nextIter, err := l.session.getRecords(ctx, shardIter)
		if err != nil {
			if l.logger != nil {
				l.logger.Warn("dynamodb config loader: GetRecords failed",
					"error", err,
					"stream_arn", streamArn,
				)
			}
			shardIter = ""
			if !l.waitTick(ctx) {
				return
			}
			continue
		}

		for _, rec := range records {
			if !matchesWatchedKey(rec, l.pk()) {
				continue
			}
			cfg, err := l.Load(ctx)
			if err != nil {
				if l.logger != nil {
					l.logger.Warn("dynamodb config loader: reload after stream event failed",
						"error", err,
					)
				}
				continue
			}
			select {
			case ch <- cfg:
			default:
			}
		}

		shardIter = nextIter

		if !l.waitTick(ctx) {
			return
		}
	}
}

// waitTick blocks for streamPollInterval using the injected clock. It
// returns false when ctx is cancelled so the caller can terminate.
func (l *Loader) waitTick(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.clk.After(l.streamPollInterval):
		return true
	}
}

// acquireLatestIterator finds the first open shard on the stream and
// returns a LATEST-type iterator for it. An empty string return (with
// nil error) indicates there are currently no open shards and the
// caller should retry later.
func (s *session) acquireLatestIterator(ctx context.Context, streamArn string) (string, error) {
	desc, err := s.streams.DescribeStream(ctx, &dynamodbstreams.DescribeStreamInput{
		StreamArn: aws.String(streamArn),
	})
	if err != nil {
		return "", fmt.Errorf("dynamodb config streams: describe stream: %w", err)
	}
	if desc.StreamDescription == nil {
		return "", nil
	}

	var shardID *string
	for _, sh := range desc.StreamDescription.Shards {
		if sh.SequenceNumberRange != nil && sh.SequenceNumberRange.EndingSequenceNumber == nil {
			shardID = sh.ShardId
			break
		}
	}
	if shardID == nil {
		return "", nil
	}

	iter, err := s.streams.GetShardIterator(ctx, &dynamodbstreams.GetShardIteratorInput{
		StreamArn:         aws.String(streamArn),
		ShardId:           shardID,
		ShardIteratorType: dstreamtypes.ShardIteratorTypeLatest,
	})
	if err != nil {
		return "", fmt.Errorf("dynamodb config streams: get shard iterator: %w", err)
	}
	if iter.ShardIterator == nil {
		return "", nil
	}
	return *iter.ShardIterator, nil
}

// getRecords fetches the next batch of stream records using shardIter.
// It returns the records, the next shard iterator (empty string when
// the shard is closed and re-discovery is required), and any error
// from the SDK call.
func (s *session) getRecords(ctx context.Context, shardIter string) ([]dstreamtypes.Record, string, error) {
	out, err := s.streams.GetRecords(ctx, &dynamodbstreams.GetRecordsInput{
		ShardIterator: aws.String(shardIter),
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if out.NextShardIterator != nil {
		next = *out.NextShardIterator
	}
	return out.Records, next, nil
}

// matchesWatchedKey returns true when the streams record refers to the
// (pk, current) pair this loader tracks. Records for unrelated items
// are ignored to avoid spurious reloads.
func matchesWatchedKey(rec dstreamtypes.Record, pk string) bool {
	if rec.Dynamodb == nil || rec.Dynamodb.Keys == nil {
		return false
	}
	pkAttr, ok := rec.Dynamodb.Keys[attrPK].(*dstreamtypes.AttributeValueMemberS)
	if !ok || pkAttr.Value != pk {
		return false
	}
	skAttr, ok := rec.Dynamodb.Keys[attrSK].(*dstreamtypes.AttributeValueMemberS)
	if !ok || skAttr.Value != skCurrent {
		return false
	}
	return true
}
