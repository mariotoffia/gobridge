package dynamodb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// casFakeDDB models DynamoDB's conditional-write and strong-read semantics
// for the config CAS path: PutItem is honoured only when the
// attribute_not_exists / version==expected condition holds.
type casFakeDDB struct {
	hasRow        bool
	storedVersion int64
	// versionAbsent models a row seeded outside this loader (AWS console,
	// Terraform, data import): PK/SK/data are present but the `version`
	// attribute is missing. GetItem omits the version attribute and PutItem
	// applies the version-less CAS clause.
	versionAbsent bool
	// getReturnsVersion, when >= 0, overrides the version reported by GetItem
	// to simulate a concurrent writer that advanced storedVersion after this
	// loader observed the version but before it wrote.
	getReturnsVersion int64

	lastGetConsistent bool
	lastPutHadCond    bool
	// putCalls counts PutItem invocations so a test can assert the Save
	// pre-checks short-circuited before ever reaching the SDK write.
	putCalls int
	// lastPutCond / lastPutValues capture the built CAS expression for
	// white-box assertions on the condition wired by putConfigItem.
	lastPutCond   string
	lastPutValues map[string]ddbtypes.AttributeValue
}

func (f *casFakeDDB) GetItem(_ context.Context, in *awsddb.GetItemInput, _ ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error) {
	f.lastGetConsistent = in.ConsistentRead != nil && *in.ConsistentRead
	if !f.hasRow {
		return &awsddb.GetItemOutput{}, nil
	}
	item := map[string]ddbtypes.AttributeValue{
		attrPK:   &ddbtypes.AttributeValueMemberS{Value: "config#cas"},
		attrSK:   &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		attrData: &ddbtypes.AttributeValueMemberS{Value: "{}"},
	}
	// A version-less (externally seeded) row reports no version attribute, so
	// getCurrentVersion sees 0 — exactly what the version-less CAS clause keys
	// off. Otherwise report the stored (or override) version.
	if !f.versionAbsent {
		v := f.storedVersion
		if f.getReturnsVersion >= 0 {
			v = f.getReturnsVersion
		}
		item[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
	}
	return &awsddb.GetItemOutput{Item: item}, nil
}

func (f *casFakeDDB) PutItem(_ context.Context, in *awsddb.PutItemInput, _ ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error) {
	f.putCalls++
	f.lastPutHadCond = in.ConditionExpression != nil
	if in.ConditionExpression != nil {
		f.lastPutCond = *in.ConditionExpression
	}
	f.lastPutValues = in.ExpressionAttributeValues

	expected := int64(-1)
	if av, ok := in.ExpressionAttributeValues[":expected"].(*ddbtypes.AttributeValueMemberN); ok {
		expected, _ = strconv.ParseInt(av.Value, 10, 64)
	}

	// Model the CAS condition:
	//   attribute_not_exists(PK)
	//   OR version == expected
	//   OR (attribute_not_exists(version) AND expected == 0)
	allowed := !f.hasRow ||
		(!f.versionAbsent && f.storedVersion == expected) ||
		(f.versionAbsent && expected == 0)
	if !allowed {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}

	newV, _ := strconv.ParseInt(in.Item[attrVersion].(*ddbtypes.AttributeValueMemberN).Value, 10, 64)
	f.hasRow = true
	f.versionAbsent = false // the write stamps the version
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

// Regression: Save must use a strong read and a conditional
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

// TestSave_AdoptsVersionlessItem is the regression for the CAS-bricking finding:
// a row seeded outside this loader (AWS console / Terraform / data import) has
// PK/SK/data but no `version` attribute. The old condition
// "attribute_not_exists(#pk) OR #v = :expected" was permanently false for such a
// row (PK exists, #v is absent so the equality is false), so every Save was
// misclassified as a version conflict and the config became un-writable forever.
// The added "(attribute_not_exists(#v) AND :expected = :zero)" clause lets a
// first version-0 CAS adopt the row and stamp its version.
func TestSave_AdoptsVersionlessItem(t *testing.T) {
	ctx := context.Background()
	cfg := &ports.BridgeConfig{}

	// Externally-seeded row: exists, but carries no version attribute.
	f := &casFakeDDB{hasRow: true, versionAbsent: true, getReturnsVersion: -1}
	l := newCASLoader(f)

	if err := l.Save(ctx, cfg); err != nil {
		t.Fatalf("save against a version-less seeded item must succeed, got %v", err)
	}
	if f.versionAbsent {
		t.Fatalf("Save must stamp the missing version attribute")
	}
	if f.storedVersion != 1 {
		t.Fatalf("stored version = %d, want 1 (first CAS stamps version 1)", f.storedVersion)
	}

	// The built condition must actually carry the version-less clause and its
	// :zero value — not merely happen to pass in the fake.
	if !strings.Contains(f.lastPutCond, "attribute_not_exists(#v)") {
		t.Fatalf("condition expression must include the version-less clause, got %q", f.lastPutCond)
	}
	if _, ok := f.lastPutValues[":zero"].(*ddbtypes.AttributeValueMemberN); !ok {
		t.Fatalf("expression attribute values must include a :zero number, got %#v", f.lastPutValues)
	}

	// A NON-zero expected version against a version-less row is a genuine
	// inconsistency and must still be rejected: the version-less clause is
	// guarded on :expected == :zero. This is unreachable via Save (which always
	// derives expected 0 from the absent version), so assert it at the
	// condition level where the guard lives.
	f2 := &casFakeDDB{hasRow: true, versionAbsent: true, getReturnsVersion: -1}
	s := &session{ddb: f2, tableName: "cfg"}
	if err := s.putConfigItem(ctx, "config#cas", []byte("{}"), 6, 5); !isConditionFailed(err) {
		t.Fatalf("non-zero expected against a version-less row must fail the CAS condition, got %v", err)
	}
}

// TestSave_RejectsOversizedConfig is the regression for the 400 KB item finding:
// a config whose serialized payload exceeds the per-item limit must fail with a
// descriptive, actionable error BEFORE any SDK write, instead of surfacing an
// opaque DynamoDB ValidationException after a round trip.
func TestSave_RejectsOversizedConfig(t *testing.T) {
	ctx := context.Background()

	// One session with a giant ID inflates the marshalled payload past the
	// pre-check threshold without needing registered plugins.
	oversized := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{
			ID:        strings.Repeat("x", maxConfigItemBytes+1024),
			Transport: "t",
		}},
	}

	f := &casFakeDDB{getReturnsVersion: -1}
	l := newCASLoader(f)

	err := l.Save(ctx, oversized)
	if err == nil {
		t.Fatalf("oversized config must be rejected")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error must describe the size limit, got %q", err.Error())
	}
	if f.putCalls != 0 {
		t.Fatalf("oversized config must be rejected before any PutItem; putCalls = %d", f.putCalls)
	}
}
