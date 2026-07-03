package dynamodb

import (
	"context"
	"fmt"
	"time"

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
// matches the loader's PK/SK. The loop owns ch and closes it on exit —
// either directly, or by handing ownership to pollLoop when streams
// become persistently unavailable.
//
// Shard handling is intentionally simple: the configuration table is
// expected to have a single active shard most of the time. Cadence
// between GetRecords calls is governed by streamPollInterval through
// the injected clock, so tests can drive it deterministically.
//
// Failure semantics (the cluster-safety rules):
//
//   - GetRecords failures back off exponentially up to maxStreamBackoff
//     WITHOUT discarding the iterator, so a throttle (the ~5 TPS shard
//     budget is shared across all instances) never loses stream
//     position.
//   - The iterator is discarded only when it is genuinely invalid
//     (expired/trimmed/gone) or after streamErrorsBeforeIteratorReset
//     consecutive unknown failures. Every re-acquire lands at LATEST,
//     which skips records written in between — so each re-acquire is
//     followed by a version reconciliation that reloads when the stored
//     version advanced past the last delivered one.
//   - Persistent acquisition failure (stream disabled/deleted) falls
//     back to poll mode with a single Warn instead of warn-spamming at
//     stream cadence forever.
func (l *Loader) streamLoop(ctx context.Context, ch chan *ports.BridgeConfig, streamArn string) {
	if l.runStreams(ctx, ch, streamArn) {
		if l.logger != nil {
			l.logger.Warn("dynamodb config loader: streams persistently unavailable; falling back to poll mode",
				"stream_arn", streamArn,
				"table", l.session.tableName,
				"poll_interval", l.pollInterval.String(),
			)
		}
		// pollLoop takes over channel ownership and closes it on exit.
		l.pollLoop(ctx, ch, l.clk.NewTicker(l.pollInterval))
		return
	}
	close(ch)
}

// runStreams is the streams consumption body. It returns true when the
// caller should fall back to poll mode, false when ctx was cancelled.
func (l *Loader) runStreams(ctx context.Context, ch chan *ports.BridgeConfig, streamArn string) bool {
	var shardIter string
	acquireFailures := 0
	recordFailures := 0
	backoff := l.streamPollInterval

	for {
		if shardIter == "" {
			iter, err := l.session.acquireLatestIterator(ctx, streamArn)
			if err != nil || iter == "" {
				acquireFailures++
				if l.logger != nil {
					l.logger.Warn("dynamodb config loader: stream shard acquisition failed",
						"error", err,
						"stream_arn", streamArn,
						"consecutive_failures", acquireFailures,
					)
				}
				if acquireFailures >= streamAcquireFallbackAfter {
					return true
				}
				if !l.wait(ctx, backoff) {
					return false
				}
				backoff = nextBackoff(backoff)
				continue
			}
			acquireFailures = 0
			backoff = l.streamPollInterval
			shardIter = iter
			// A freshly acquired LATEST iterator skips anything written
			// while no iterator was held — reconcile via version check.
			l.reloadIfVersionAdvanced(ctx, ch)
		}

		records, nextIter, err := l.session.getRecords(ctx, shardIter)
		if err != nil {
			recordFailures++
			if l.logger != nil {
				l.logger.Warn("dynamodb config loader: GetRecords failed",
					"error", err,
					"stream_arn", streamArn,
					"consecutive_failures", recordFailures,
					"backoff", backoff.String(),
				)
			}
			switch {
			case isStreamIteratorInvalid(err):
				// Iterator lost: re-acquire (at LATEST) and reconcile.
				shardIter = ""
				recordFailures = 0
			case isStreamThrottle(err):
				// Throttle: KEEP the iterator so stream position is
				// preserved — backoff alone sheds load. If the iterator
				// expires while backing off, the next call surfaces
				// ExpiredIteratorException and is handled above.
			case recordFailures >= streamErrorsBeforeIteratorReset:
				// Persistent unknown failure: force a re-acquire.
				shardIter = ""
				recordFailures = 0
			}
			if !l.wait(ctx, backoff) {
				return false
			}
			backoff = nextBackoff(backoff)
			continue
		}
		recordFailures = 0
		backoff = l.streamPollInterval

		matched := false
		for _, rec := range records {
			if matchesWatchedKey(rec, l.pk()) {
				matched = true
				break
			}
		}
		if matched {
			cfg, err := l.Load(ctx)
			if err != nil {
				if l.logger != nil {
					l.logger.Warn("dynamodb config loader: reload after stream event failed",
						"error", err,
					)
				}
			} else {
				l.deliverLatest(ch, cfg)
			}
		}

		if nextIter == "" {
			// Shard closed: the next acquisition starts at LATEST on a
			// new shard, so reconcile covers the gap.
			shardIter = ""
		} else {
			shardIter = nextIter
		}

		if !l.wait(ctx, l.streamPollInterval) {
			return false
		}
	}
}

// reloadIfVersionAdvanced closes the gap a LATEST iterator opens: any
// Save committed while no iterator was held produced no observable
// stream record for this consumer. Compare the stored version to the
// last loaded one and deliver a fresh config when it advanced. A zero
// lastVersion means no baseline Load has happened yet — the initial
// config is the caller's Load, not the watcher's, so nothing is
// delivered in that case.
func (l *Loader) reloadIfVersionAdvanced(ctx context.Context, ch chan *ports.BridgeConfig) {
	l.mu.Lock()
	lastSeen := l.lastVersion
	l.mu.Unlock()
	if lastSeen == 0 {
		return
	}

	v, err := l.currentVersion(ctx)
	if err != nil {
		if l.logger != nil {
			l.logger.Warn("dynamodb config loader: post-gap version reconciliation failed",
				"error", err,
				"table", l.session.tableName,
			)
		}
		return
	}
	if v == lastSeen {
		return
	}

	cfg, err := l.Load(ctx)
	if err != nil {
		if l.logger != nil {
			l.logger.Warn("dynamodb config loader: post-gap reload failed",
				"error", err,
				"table", l.session.tableName,
			)
		}
		return
	}
	l.deliverLatest(ch, cfg)
}

// wait blocks for d using the injected clock. It returns false when
// ctx is cancelled so the caller can terminate.
func (l *Loader) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.clk.After(d):
		return true
	}
}

// nextBackoff doubles d up to maxStreamBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxStreamBackoff {
		return maxStreamBackoff
	}
	return d
}

// acquireLatestIterator finds the first open shard on the stream and
// returns a LATEST-type iterator for it, paginating through the shard
// list (DescribeStream returns at most 100 shards per page). An empty
// string return (with nil error) indicates there are currently no open
// shards and the caller should retry later.
func (s *session) acquireLatestIterator(ctx context.Context, streamArn string) (string, error) {
	var shardID *string
	var exclusiveStart *string

	for {
		desc, err := s.streams.DescribeStream(ctx, &dynamodbstreams.DescribeStreamInput{
			StreamArn:             aws.String(streamArn),
			ExclusiveStartShardId: exclusiveStart,
		})
		if err != nil {
			return "", fmt.Errorf("dynamodb config streams: describe stream: %w", err)
		}
		if desc.StreamDescription == nil {
			return "", nil
		}

		for _, sh := range desc.StreamDescription.Shards {
			if sh.SequenceNumberRange != nil && sh.SequenceNumberRange.EndingSequenceNumber == nil {
				shardID = sh.ShardId
				break
			}
		}
		if shardID != nil {
			break
		}
		if desc.StreamDescription.LastEvaluatedShardId == nil {
			return "", nil
		}
		exclusiveStart = desc.StreamDescription.LastEvaluatedShardId
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
