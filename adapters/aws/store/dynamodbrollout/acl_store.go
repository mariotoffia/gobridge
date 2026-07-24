package dynamodbrollout

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// maxCASRetries bounds the optimistic read-modify-write loop. Every retry means
// another writer won the revision race, so it converges in at most one round per
// competing writer; the bound is a livelock backstop, far above realistic
// single-partition contention. On exhaustion the store surfaces the transient
// ErrUnavailable so the caller may retry.
const maxCASRetries = 64

// Store implements ports.ClusterRolloutStore against a single DynamoDB row.
// See the package doc for the single-row / optimistic-concurrency / single-Region
// design. All state lives in one item under singletonPK; the domain aggregate
// (persistence.Rollout) owns every invariant, the store owns only durability and
// the compare-and-set that makes each transition atomic.
type Store struct {
	client    dynamoAPI
	tableName string
	clk       clock.Clock
	logger    *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-rollouts").
func WithTableName(name string) Option {
	return func(s *Store) {
		if name != "" {
			s.tableName = name
		}
	}
}

// WithClock overrides the clock used to stamp rollout deadlines and ack times.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithLogger sets a structured logger for trace-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// NewStore creates a new DynamoDB-backed ClusterRolloutStore.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL constructor
// exists to wrap; it is injected by the composition root and stored behind an
// unexported field.
//
//aclcheck:allow-export
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:    client,
		tableName: DefaultTableName,
		clk:       clock.System,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Propose opens a new rollout at the next monotonic generation. On an empty
// store it creates generation 1 with a conditional PutItem that exactly one
// racer can win (invariant I1); the rest observe the now-present row and get
// ErrAlreadyExists. Over a terminal row it opens generation+1, guarded by the
// revision counter so a concurrent proposer cannot double-open.
func (s *Store) Propose(ctx context.Context, proposal persistence.RolloutProposal) (persistence.Rollout, error) {
	s.trace(ctx, "propose", 0)

	for range maxCASRetries {
		cur, rev, err := s.readCurrent(ctx)
		if errors.Is(err, shared.ErrNotFound) {
			r, berr := persistence.NewRollout(1, proposal, s.deadline(proposal))
			if berr != nil {
				return persistence.Rollout{}, berr
			}
			written, putErr := s.putIfAbsent(ctx, r.Snapshot())
			if putErr != nil {
				return persistence.Rollout{}, putErr
			}
			if written {
				return r, nil
			}
			continue // lost the create race: re-read to observe the winner
		}
		if err != nil {
			return persistence.Rollout{}, err
		}
		if !cur.State().IsTerminal() {
			return persistence.Rollout{}, shared.ErrAlreadyExists.
				WithMessage("a rollout is already active").
				With("generation", cur.Generation()).With("state", string(cur.State()))
		}
		r, berr := persistence.NewRollout(cur.Generation()+1, proposal, s.deadline(proposal))
		if berr != nil {
			return persistence.Rollout{}, berr
		}
		written, putErr := s.putIfRev(ctx, r.Snapshot(), rev)
		if putErr != nil {
			return persistence.Rollout{}, putErr
		}
		if written {
			return r, nil
		}
		// lost the revision race: re-read (the winner may now be non-terminal).
	}
	return persistence.Rollout{}, retryExhausted("propose")
}

// Ack records a member's acknowledgement, advancing Proposed → Staging on the
// first ack.
func (s *Store) Ack(ctx context.Context, generation uint64, memberID, buildDigest string) error {
	return s.mutate(ctx, generation, "ack", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithAck(memberID, buildDigest, s.clk.Now())
	})
}

// Nack records a member's rejection without terminating the rollout.
func (s *Store) Nack(ctx context.Context, generation uint64, memberID, reason string) error {
	return s.mutate(ctx, generation, "nack", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithNack(memberID, reason)
	})
}

// Commit commits the rollout under the coordinator's fencing token.
func (s *Store) Commit(ctx context.Context, generation uint64, token persistence.LeaseToken) error {
	return s.mutate(ctx, generation, "commit", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithCommit(token)
	})
}

// Abort aborts the rollout under the coordinator's fencing token.
func (s *Store) Abort(ctx context.Context, generation uint64, token persistence.LeaseToken, reason string) error {
	return s.mutate(ctx, generation, "abort", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithAbort(token, reason)
	})
}

// Current returns the current (active or last-decided) rollout, or ErrNotFound
// if none has ever been proposed.
func (s *Store) Current(ctx context.Context) (persistence.Rollout, error) {
	s.trace(ctx, "current", 0)
	r, _, err := s.readCurrent(ctx)
	return r, err
}

// mutate is the optimistic read-modify-write loop: read the current row with a
// strong ConsistentRead, apply the domain transition, and write the result back
// with a conditional PutItem gated on the unchanged revision. A revision-race
// loss re-reads and retries so the domain invariant checks act as an atomic
// compare-and-set even under concurrency.
func (s *Store) mutate(
	ctx context.Context,
	generation uint64,
	op string,
	fn func(persistence.Rollout) (persistence.Rollout, *shared.BridgeError),
) error {
	s.trace(ctx, op, generation)

	for range maxCASRetries {
		cur, rev, err := s.readCurrent(ctx)
		if err != nil {
			return err // ErrNotFound (no row), or a corrupt/IO error: fail closed
		}
		if cur.Generation() != generation {
			return shared.ErrNotFound.
				WithMessage("no active rollout at generation").With("generation", generation)
		}
		next, berr := fn(cur)
		if berr != nil {
			return berr // domain rejected the transition (stale token, terminal, …)
		}
		// ponytail: an idempotent no-op transition (same-or-newer token re-committing
		// an already-terminal rollout) still writes byte-identical content at rev+1.
		// Harmless — the loop terminates in ≤1 retry and content is unchanged; skip
		// the write only if this rare crash-resume path ever shows as write pressure.
		written, putErr := s.putIfRev(ctx, next.Snapshot(), rev)
		if putErr != nil {
			return putErr
		}
		if written {
			return nil
		}
		// lost the revision race: re-read and re-apply.
	}
	return retryExhausted(op)
}

// readCurrent reads the singleton row with a strong ConsistentRead and returns
// the rehydrated aggregate plus its revision. Any corruption (attribute decode
// or domain rehydration) fails closed as ErrInvalidConfig so a coordinator never
// acts on a malformed row.
func (s *Store) readCurrent(ctx context.Context) (persistence.Rollout, uint64, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.tableName,
		Key:            map[string]ddbtypes.AttributeValue{attrPK: sAttr(singletonPK)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return persistence.Rollout{}, 0, wrapErr(err, "read rollout row failed")
	}
	if len(out.Item) == 0 {
		return persistence.Rollout{}, 0, shared.ErrNotFound.WithMessage("no rollout has been proposed")
	}
	snap, rev, err := decodeRolloutItem(out.Item)
	if err != nil {
		return persistence.Rollout{}, 0, err
	}
	r, berr := persistence.RehydrateRollout(snap)
	if berr != nil {
		return persistence.Rollout{}, 0, corruptRow("rehydrate rejected the persisted rollout: " + berr.Error())
	}
	return r, rev, nil
}

// putIfAbsent writes a fresh (generation 1) rollout only if no row exists yet.
// Returns written=false (nil error) when another racer already created it.
func (s *Store) putIfAbsent(ctx context.Context, snap persistence.RolloutSnapshot) (bool, error) {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                &s.tableName,
		Item:                     rolloutItem(snap, 1),
		ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{"#pk": attrPK},
	})
	if err == nil {
		return true, nil
	}
	if isConditionFailed(err) {
		return false, nil
	}
	return false, wrapErr(err, "create rollout row failed")
}

// putIfRev writes the snapshot with rev+1 only if the stored revision still
// equals expectedRev. Returns written=false (nil error) on a revision-race loss.
func (s *Store) putIfRev(ctx context.Context, snap persistence.RolloutSnapshot, expectedRev uint64) (bool, error) {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                &s.tableName,
		Item:                     rolloutItem(snap, expectedRev+1),
		ConditionExpression:      aws.String("#rev = :rev"),
		ExpressionAttributeNames: map[string]string{"#rev": attrRev},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rev": nUintAttr(expectedRev),
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionFailed(err) {
		return false, nil
	}
	return false, wrapErr(err, "write rollout row failed")
}

// EnsureTable creates the rollout table (single hash key, on-demand billing) if
// it does not already exist. It is a dev/test convenience; production tables are
// provisioned out of band. No TTL is configured (see the package doc).
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
			return nil // already exists: idempotent
		}
		return wrapErr(err, "create rollout table failed")
	}
	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: &s.tableName}, 30*time.Second); err != nil {
		return wrapErr(err, "wait for rollout table to exist failed")
	}
	return nil
}

func (s *Store) deadline(p persistence.RolloutProposal) time.Time {
	return s.clk.Now().Add(p.TTL)
}

func (s *Store) trace(ctx context.Context, op string, generation uint64) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbrollout: "+op, "generation", generation)
	}
}

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func retryExhausted(op string) error {
	return shared.ErrUnavailable.
		WithMessage("dynamodbrollout: "+op+" exhausted optimistic-concurrency retries").
		With("retries", maxCASRetries)
}

func wrapErr(err error, msg string) error {
	return shared.ErrUnavailable.WithMessage("dynamodbrollout: " + msg).Wrap(err)
}
