package dynamodbrollout

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// committedPK is the fixed partition key of the durable last-committed config
// artifact -- a SECOND row in the same table as the active rollout (singletonPK),
// deliberately distinct: the active rollout row is single-active and overwritten
// on the next Propose, whereas the committed artifact must survive across
// proposals so a (re)joining member can always recover the config to run.
const committedPK = "ROLLOUT#committed"

const (
	attrConfigBytes = "config_bytes"
	attrDigest      = "digest"
)

// PutCommittedConfig durably records the last-committed config artifact with an
// optimistic read-modify-write: read the committed row, apply the monotonic /
// same-generation-idempotent rules (see the port contract), and write back under
// a revision-gated conditional PutItem so concurrent committers converge on the
// single highest generation without a lost update.
func (s *Store) PutCommittedConfig(ctx context.Context, cfg persistence.CommittedRolloutConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.trace(ctx, "put-committed", cfg.Generation)

	for range maxCASRetries {
		stored, rev, err := s.readCommitted(ctx)
		if errors.Is(err, shared.ErrNotFound) {
			written, putErr := s.putCommittedIfAbsent(ctx, cfg)
			if putErr != nil {
				return putErr
			}
			if written {
				return nil
			}
			continue // lost the create race: re-read to observe the winner
		}
		if err != nil {
			return err
		}
		switch {
		case cfg.Generation < stored.Generation:
			return nil // stale writer: never regress the artifact
		case cfg.Generation == stored.Generation:
			if cfg.Digest != stored.Digest {
				return shared.ErrRolloutDigestMismatch.
					WithMessage("committed config digest conflict at the same generation").
					With("generation", cfg.Generation).
					With("stored", stored.Digest).With("given", cfg.Digest)
			}
			return nil // idempotent: same generation, same digest
		}
		written, putErr := s.putCommittedIfRev(ctx, cfg, rev)
		if putErr != nil {
			return putErr
		}
		if written {
			return nil
		}
		// lost the revision race: re-read and re-apply.
	}
	return retryExhausted("put-committed")
}

// CommittedConfig returns the last-committed config artifact, or ErrNotFound if
// nothing has been committed or seeded yet.
func (s *Store) CommittedConfig(ctx context.Context) (persistence.CommittedRolloutConfig, error) {
	s.trace(ctx, "committed", 0)
	cfg, _, err := s.readCommitted(ctx)
	return cfg, err
}

// readCommitted reads the committed row with a strong ConsistentRead and returns
// the artifact plus its revision. A missing row is ErrNotFound; any corruption
// fails closed as a corrupt-row error so a member never boots on a malformed
// artifact.
func (s *Store) readCommitted(ctx context.Context) (persistence.CommittedRolloutConfig, uint64, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.tableName,
		Key:            map[string]ddbtypes.AttributeValue{attrPK: sAttr(committedPK)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return persistence.CommittedRolloutConfig{}, 0, wrapErr(err, "read committed config row failed")
	}
	if len(out.Item) == 0 {
		return persistence.CommittedRolloutConfig{}, 0, shared.ErrNotFound.WithMessage("no committed config artifact")
	}
	return decodeCommittedItem(out.Item)
}

func (s *Store) putCommittedIfAbsent(ctx context.Context, cfg persistence.CommittedRolloutConfig) (bool, error) {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                &s.tableName,
		Item:                     committedItem(cfg, 1),
		ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{"#pk": attrPK},
	})
	if err == nil {
		return true, nil
	}
	if isConditionFailed(err) {
		return false, nil
	}
	return false, wrapErr(err, "create committed config row failed")
}

func (s *Store) putCommittedIfRev(ctx context.Context, cfg persistence.CommittedRolloutConfig, expectedRev uint64) (bool, error) {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                &s.tableName,
		Item:                     committedItem(cfg, expectedRev+1),
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
	return false, wrapErr(err, "write committed config row failed")
}

// committedItem serializes the artifact plus its revision counter into a
// DynamoDB item. The config bytes are stored as a Binary attribute (B) so the
// exact document round-trips byte-for-byte.
func committedItem(cfg persistence.CommittedRolloutConfig, rev uint64) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK:          sAttr(committedPK),
		attrRev:         nUintAttr(rev),
		attrGeneration:  nUintAttr(cfg.Generation),
		attrConfigVer:   nAttr(int64(cfg.ConfigVersion)),
		attrConfigBytes: &ddbtypes.AttributeValueMemberB{Value: cfg.ConfigBytes},
		attrDigest:      sAttr(cfg.Digest),
	}
}

// decodeCommittedItem is the fail-closed deserialization boundary for the
// committed artifact: a missing/mistyped attribute or an artifact that fails the
// domain validation yields a corrupt-row error rather than an empty config a
// member would build nothing from.
func decodeCommittedItem(item map[string]ddbtypes.AttributeValue) (persistence.CommittedRolloutConfig, uint64, error) {
	pk, err := reqString(item, attrPK)
	if err != nil || pk != committedPK {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("missing or mismatched committed partition key")
	}
	rev, err := reqUint(item, attrRev)
	if err != nil || rev == 0 {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed revision must be present and positive")
	}
	gen, err := reqUint(item, attrGeneration)
	if err != nil {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed generation: " + err.Error())
	}
	cfgVer, err := reqInt64(item, attrConfigVer)
	if err != nil {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed config_version: " + err.Error())
	}
	bytes, err := reqBinary(item, attrConfigBytes)
	if err != nil {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed config_bytes: " + err.Error())
	}
	digest, err := reqString(item, attrDigest)
	if err != nil {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed digest: " + err.Error())
	}
	cfg := persistence.CommittedRolloutConfig{
		Generation:    gen,
		ConfigVersion: int(cfgVer),
		ConfigBytes:   bytes,
		Digest:        digest,
	}
	if berr := cfg.Validate(); berr != nil {
		return persistence.CommittedRolloutConfig{}, 0, corruptRow("committed artifact failed validation: " + berr.Error())
	}
	return cfg, rev, nil
}

func reqBinary(attrs map[string]ddbtypes.AttributeValue, key string) ([]byte, error) {
	v, ok := attrs[key]
	if !ok {
		return nil, errors.New("attribute " + key + " is missing")
	}
	b, ok := v.(*ddbtypes.AttributeValueMemberB)
	if !ok {
		return nil, errors.New("attribute " + key + " is not binary")
	}
	return b.Value, nil
}
