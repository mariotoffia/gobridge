package dynamodboutbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// projectionMismatchErr is the exact ValidationException DynamoDB raises when a
// claim Query filters on the non-projected `status` attribute against a
// KEYS_ONLY (or under-projected INCLUDE) ClaimIndex.
const projectionMismatchErr = "ValidationException: Secondary index ClaimIndex does not " +
	"project one or more filter attributes: [status]"

// TestClaim_ProjectionMismatch_SurfacesTheProvisioningFault: a correctly-KEYED
// but under-PROJECTED ClaimIndex fails every claim Query, because the filter
// reads the non-projected `status` attribute. Preflight rejects that table at
// startup; if a deployment ever reaches a claim with it, the failure must be
// SURFACED, naming the index, not swallowed into a silent whole-partition scan
// that turns an O(limit) drain into O(backlog) fleet-wide.
//
// Mutation this kills: re-introducing an "index unusable → scan" fallback makes
// Claim return nil → this test FAILs.
func TestClaim_ProjectionMismatch_SurfacesTheProvisioningFault(t *testing.T) {
	const partition = "PART#misprojected"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			return nil, errors.New(projectionMismatchErr)
		}
		t.Fatal("an under-projected ClaimIndex must NOT silently fall back to the base-table scan")
		return nil, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err == nil {
		t.Fatal("an under-projected ClaimIndex must surface as an error")
	}
	if claimed != nil {
		t.Fatalf("no records may be returned when the index query failed, got %d", len(claimed))
	}
	if !strings.Contains(err.Error(), claimIndexName) {
		t.Fatalf("the error must name the offending index, got %v", err)
	}
}

// outboxTableDescription builds a DescribeTable result whose primary key and
// ExpiryIndex/RecordIDIndex are always valid, so a preflight test isolates the
// ClaimIndex assertion. claimProjection selects the ClaimIndex projection type;
// an empty string omits ClaimIndex entirely (the OPTIONAL/absent case).
func outboxTableDescription(claimProjection ddbtypes.ProjectionType) *ddbtypes.TableDescription {
	gsis := []ddbtypes.GlobalSecondaryIndexDescription{
		{
			IndexName: aws.String(expiryIndexName),
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: aws.String(attrHasExpiry), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: aws.String("expires_at"), KeyType: ddbtypes.KeyTypeRange},
			},
			Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
		},
		{
			IndexName: aws.String(recordIDIndexName),
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: aws.String("record_id"), KeyType: ddbtypes.KeyTypeHash},
			},
			Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
		},
	}
	if claimProjection != "" {
		gsis = append(gsis, ddbtypes.GlobalSecondaryIndexDescription{
			IndexName: aws.String(claimIndexName),
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: aws.String("PK"), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: aws.String(attrClaimSort), KeyType: ddbtypes.KeyTypeRange},
			},
			Projection: &ddbtypes.Projection{ProjectionType: claimProjection},
		})
	}
	return &ddbtypes.TableDescription{
		TableName: aws.String("outbox-test"),
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrHasExpiry), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("expires_at"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			{AttributeName: aws.String("record_id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrClaimSort), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: gsis,
	}
}

// TestPreflight_ClaimIndexProjection is the c13-review-HIGH durable guard: a
// present ClaimIndex whose KEY schema is correct but whose PROJECTION omits
// `status` (KEYS_ONLY or INCLUDE-without-status) passes a key-only check yet
// fails EVERY claim query at runtime. Preflight must REJECT it at startup with a
// clear "Projection: ALL" message. A Projection: ALL ClaimIndex passes, and an
// ABSENT ClaimIndex stays valid (it is OPTIONAL — Claim degrades to scan).
//
// Mutation this kills: dropping the wantProjectionAll assertion in
// validateTableSchema (or the wantProjectionAll:true flag on the ClaimIndex
// expectedIndex) makes a KEYS_ONLY index PASS preflight → the "reject" subtest
// FAILs at "must reject a KEYS_ONLY ClaimIndex".
func TestPreflight_ClaimIndexProjection(t *testing.T) {
	t.Run("KEYS_ONLY ClaimIndex is rejected at startup", func(t *testing.T) {
		f := newFakeDDB()
		f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
			return &dynamodb.DescribeTableOutput{
				Table: outboxTableDescription(ddbtypes.ProjectionTypeKeysOnly),
			}, nil
		}
		s := newFakeStore(f)

		err := s.Preflight(context.Background())
		if err == nil {
			t.Fatalf("preflight must reject a KEYS_ONLY ClaimIndex, got nil")
		}
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("rejection must be ErrInvalidConfig, got %v", err)
		}
		if !strings.Contains(err.Error(), "Projection: ALL") {
			t.Fatalf("rejection message must demand Projection: ALL, got %q", err.Error())
		}
	})

	t.Run("INCLUDE-without-status ClaimIndex is rejected", func(t *testing.T) {
		f := newFakeDDB()
		f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
			return &dynamodb.DescribeTableOutput{
				Table: outboxTableDescription(ddbtypes.ProjectionTypeInclude),
			}, nil
		}
		s := newFakeStore(f)

		err := s.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Projection: ALL") {
			t.Fatalf("preflight must reject an INCLUDE ClaimIndex demanding Projection: ALL, got %v", err)
		}
	})

	t.Run("Projection ALL ClaimIndex passes", func(t *testing.T) {
		f := newFakeDDB()
		f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
			return &dynamodb.DescribeTableOutput{
				Table: outboxTableDescription(ddbtypes.ProjectionTypeAll),
			}, nil
		}
		s := newFakeStore(f)

		if err := s.Preflight(context.Background()); err != nil {
			t.Fatalf("a Projection: ALL ClaimIndex must pass preflight, got %v", err)
		}
	})

	t.Run("absent ClaimIndex is rejected", func(t *testing.T) {
		f := newFakeDDB()
		f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
			return &dynamodb.DescribeTableOutput{
				Table: outboxTableDescription(""), // ClaimIndex omitted
			}, nil
		}
		s := newFakeStore(f)

		err := s.Preflight(context.Background())
		if err == nil {
			t.Fatal("a table without ClaimIndex must fail preflight: the index is required, " +
				"and tolerating its absence silently turns every claim into a whole-partition scan")
		}
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("a missing required index is a provisioning fault, got %v", err)
		}
		if !strings.Contains(err.Error(), claimIndexName) {
			t.Fatalf("the rejection must name the missing index, got %v", err)
		}
	})
}

// seqCounterUpdateFn returns an updateItemFn that models the FENCE row's atomic
// seq_counter, advancing a shared counter by :n and returning the new value.
func seqCounterUpdateFn(counter *int64) func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
	return func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		n := numAttrI64(in.ExpressionAttributeValues, ":n")
		*counter += n
		return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
			attrSeqCounter: &ddbtypes.AttributeValueMemberN{Value: i64(*counter)},
		}}, nil
	}
}

// TestPersist_ForeignSortKeyOccupant_NotDropped pins the silent-loss guard on
// Persist. attribute_not_exists(SK) fails for BOTH a genuine redelivery and a
// distinct record whose key is already occupied, and sortKey is injective, so
// this store can only ever produce the first case. The second means a writer
// that is not this store owns that key — blind-counting it a duplicate would ack
// and DROP a distinct message. Persist reads the occupant strongly-consistently
// and, on an envelope/binding MISMATCH, returns a TRANSIENT error so the record
// is retried rather than lost.
//
// Mutation this kills: removing the verify-on-conflict readback and counting
// every conflict a duplicate → Persist returns ErrDuplicateRecord → this test
// FAILs on the transient-error assertion.
func TestPersist_ForeignSortKeyOccupant_NotDropped(t *testing.T) {
	f := newFakeDDB()
	var counter int64
	f.updateItemFn = seqCounterUpdateFn(&counter)
	// The occupying row is a DIFFERENT record — a foreign writer owns the key: its
	// envelope/binding differ from the record being persisted.
	f.getItemFn = func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			"envelope_id": &ddbtypes.AttributeValueMemberS{Value: "foreign-env"},
			"binding_id":  &ddbtypes.AttributeValueMemberS{Value: "foreign-bind"},
		}}, nil
	}
	putCalls := 0
	f.putItemFn = func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		putCalls++
		if putCalls == 1 {
			// This record's SK is already occupied by a row it does not own.
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
		return &dynamodb.PutItemOutput{}, nil
	}
	s := newFakeStore(f)

	records := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "c-1", RouteID: "r", EnvelopeID: "a#b", BindingID: "c", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a#b", Subject: "t"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "c-2", RouteID: "r", EnvelopeID: "e2", BindingID: "c", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Subject: "t"}),
		}),
	}
	err := s.Persist(context.Background(), records)
	if err == nil {
		t.Fatalf("a foreign occupant of this sort key must surface a transient error, not drop the record")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("a foreign sort-key occupant must be a transient ErrUnavailable so the record is retried, got %v", err)
	}
	be, ok := shared.AsBridgeError(err)
	if !ok || be.Class != shared.ErrorTransient {
		t.Fatalf("collision error must be transient so the drainer retries, got %+v", err)
	}
}

// TestPersist_GenuineDuplicate_StillCollapses proves the verify-on-conflict path
// does NOT over-trigger: when the occupying row is the SAME logical record
// (identical envelope_id AND binding_id), the conflict is a real idempotent
// redelivery and is collapsed (duplicates++), exactly as before the fix.
func TestPersist_GenuineDuplicate_StillCollapses(t *testing.T) {
	f := newFakeDDB()
	var counter int64
	f.updateItemFn = seqCounterUpdateFn(&counter)
	f.getItemFn = dupRowGetItem() // occupant == same record (env/binding from SK)
	f.putItemFn = func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	s := newFakeStore(f)

	records := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "d-1", RouteID: "r", EnvelopeID: "e1", BindingID: "b1", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t"}),
		}),
	}
	err := s.Persist(context.Background(), records)
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("a genuine same-record redelivery must collapse to ErrDuplicateRecord, got %v", err)
	}
}

// TestClaim_StaleIndexEntry_SkipsCandidate is the c13-review-MEDIUM
// (GSI eventual-consistency) test-gap closer. DynamoDB-Local GSIs are
// synchronously consistent, so conformance cannot exercise a stale ClaimIndex
// entry. Here the ClaimIndex Query returns two candidates in arrival order; the
// FIRST is stale (its base row is already terminal/claimed → the per-record
// claim transaction fails with a record-level ConditionalCheckFailed). The
// re-validation in claimOne must SKIP that candidate (not error, not
// double-claim) and still claim the remaining candidate in arrival order.
//
// Mutation this kills: making claimOne surface a record-level
// ConditionalCheckFailed as an error (replacing the benign `return nil, nil`
// tail with a wrapErr) makes the stale candidate ERROR the whole Claim → this
// test FAILs at "stale index entry must be skipped, not error".
func TestClaim_StaleIndexEntry_SkipsCandidate(t *testing.T) {
	const partition = "PART#stalegsi"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName == nil || *in.IndexName != claimIndexName {
			t.Fatalf("fast path must query the %s GSI", claimIndexName)
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem(partition, "OUTBOX#env-stale#bind", "rec-stale"),
			pendingQueryItem(partition, "OUTBOX#env-live#bind", "rec-live"),
		}}, nil
	}
	f.transactFn = func(in *dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		// The stale candidate's base row is already terminal: item 0 (fence)
		// passes, item 1 (record update condition) fails.
		if strAttr(in.TransactItems[1].Update.Key, "SK") == "OUTBOX#env-stale#bind" {
			return nil, transactCanceled("None", "ConditionalCheckFailed")
		}
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("a stale index entry must be skipped, not error: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID() != "rec-live" {
		t.Fatalf("only the live candidate must claim (stale skipped, arrival order), got %v", claimed)
	}
}

// TestClaim_EmptyCancellationReasons_NotSilentlySkipped is the c13-review-LOW
// defensive branch: a TransactionCanceledException carrying an EMPTY
// CancellationReasons array is not a recognisable lost race. It must surface a
// (transient) error via wrapErr, NOT fall through to the benign (nil, nil) skip
// that would silently DROP the record.
//
// Mutation this kills: removing the `if len(reasons) == 0` branch in claimOne
// lets an empty-reasons cancellation fall through to `return nil, nil` → Claim
// returns no error and no record → this test FAILs at "must not be a silent
// skip".
func TestClaim_EmptyCancellationReasons_NotSilentlySkipped(t *testing.T) {
	const partition = "PART#emptyreasons"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem(partition, "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		// Canceled, but with NO per-item reason codes.
		return nil, &ddbtypes.TransactionCanceledException{}
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err == nil {
		t.Fatalf("an empty-reasons cancellation must surface an error, not a silent skip (claimed=%v)", claimed)
	}
	if claimed != nil {
		t.Fatalf("no records may be returned alongside the error, got %v", claimed)
	}
}
