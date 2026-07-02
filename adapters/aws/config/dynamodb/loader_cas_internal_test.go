package dynamodb

import (
	"context"
	"errors"
	"strconv"
	"testing"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// casFakeDDB models DynamoDB's conditional-write and strong-read semantics
// for the config CAS path (J3): PutItem is honoured only when the
// attribute_not_exists / version==expected condition holds.
type casFakeDDB struct {
	hasRow        bool
	storedVersion int64
	// getReturnsVersion, when >= 0, overrides the version reported by GetItem
	// to simulate a concurrent writer that advanced storedVersion after this
	// loader observed the version but before it wrote.
	getReturnsVersion int64

	lastGetConsistent bool
	lastPutHadCond    bool
}

func (f *casFakeDDB) GetItem(_ context.Context, in *awsddb.GetItemInput, _ ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error) {
	f.lastGetConsistent = in.ConsistentRead != nil && *in.ConsistentRead
	if !f.hasRow {
		return &awsddb.GetItemOutput{}, nil
	}
	v := f.storedVersion
	if f.getReturnsVersion >= 0 {
		v = f.getReturnsVersion
	}
	return &awsddb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
		attrPK:      &ddbtypes.AttributeValueMemberS{Value: "config#cas"},
		attrSK:      &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		attrData:    &ddbtypes.AttributeValueMemberS{Value: "{}"},
		attrVersion: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)},
	}}, nil
}

func (f *casFakeDDB) PutItem(_ context.Context, in *awsddb.PutItemInput, _ ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error) {
	f.lastPutHadCond = in.ConditionExpression != nil
	expected := int64(-1)
	if av, ok := in.ExpressionAttributeValues[":expected"].(*ddbtypes.AttributeValueMemberN); ok {
		expected, _ = strconv.ParseInt(av.Value, 10, 64)
	}
	// attribute_not_exists(PK) OR version == expected
	if f.hasRow && f.storedVersion != expected {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	newV, _ := strconv.ParseInt(in.Item[attrVersion].(*ddbtypes.AttributeValueMemberN).Value, 10, 64)
	f.hasRow = true
	f.storedVersion = newV
	f.getReturnsVersion = -1
	return &awsddb.PutItemOutput{}, nil
}

func (f *casFakeDDB) CreateTable(_ context.Context, _ *awsddb.CreateTableInput, _ ...func(*awsddb.Options)) (*awsddb.CreateTableOutput, error) {
	return &awsddb.CreateTableOutput{}, nil
}

func (f *casFakeDDB) DescribeTable(_ context.Context, _ *awsddb.DescribeTableInput, _ ...func(*awsddb.Options)) (*awsddb.DescribeTableOutput, error) {
	return &awsddb.DescribeTableOutput{}, nil
}

func newCASLoader(f *casFakeDDB) *Loader {
	return &Loader{
		session:  &session{ddb: f, tableName: "cfg"},
		bridgeID: "cas",
		registry: ports.NewRegistry(),
	}
}

// Regression for J3: Save must use a strong read and a conditional
// (compare-and-set) write so a concurrent admin write cannot be silently
// lost. A stale expected-version write is rejected as ErrVersionMismatch.
func TestSave_CompareAndSet(t *testing.T) {
	ctx := context.Background()
	cfg := &ports.BridgeConfig{}

	t.Run("first_write_succeeds_and_is_conditional", func(t *testing.T) {
		f := &casFakeDDB{getReturnsVersion: -1}
		l := newCASLoader(f)

		if err := l.Save(ctx, cfg); err != nil {
			t.Fatalf("first save: %v", err)
		}
		if !f.lastGetConsistent {
			t.Fatalf("Save must read the current version with a strong (consistent) read")
		}
		if !f.lastPutHadCond {
			t.Fatalf("Save must write with a ConditionExpression (CAS)")
		}
		if f.storedVersion != 1 {
			t.Fatalf("stored version = %d, want 1", f.storedVersion)
		}
	})

	t.Run("concurrent_update_is_rejected", func(t *testing.T) {
		// Row at version 1, but a concurrent writer advanced storedVersion to
		// 2 after our loader read version 1.
		f := &casFakeDDB{hasRow: true, storedVersion: 2, getReturnsVersion: 1}
		l := newCASLoader(f)

		err := l.Save(ctx, cfg)
		if !errors.Is(err, shared.ErrVersionMismatch) {
			t.Fatalf("expected ErrVersionMismatch on lost-update conflict, got %v", err)
		}
		if f.storedVersion != 2 {
			t.Fatalf("conflicting save must not clobber; stored version = %d, want 2", f.storedVersion)
		}
	})
}
