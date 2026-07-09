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
// KEYS_ONLY (or under-projected INCLUDE) ClaimIndex. It does NOT contain the
// "specified index" substring the old isMissingIndex matched, so before the fix
// it flowed to mapError and was mis-classified permanent (ErrInvalidPayload),
// wedging every Claim fleet-wide (c13 review HIGH).
const projectionMismatchErr = "ValidationException: Secondary index ClaimIndex does not " +
	"project one or more filter attributes: [status]"

// TestClaim_ProjectionMismatch_FallsBackToScan is the c13-review-HIGH runtime
// backstop: a correctly-KEYED but under-PROJECTED ClaimIndex makes the claim
// Query fail with a projection-mismatch ValidationException. That MUST degrade
// to the exhaustive scan fallback (exactly like a missing index) with one WARN,
// NEVER surface as an error — otherwise mapError classifies it ErrInvalidPayload
// (permanent) and outbox delivery wedges on every partition.
//
// Mutation this kills: narrowing claimIndexUnusableReason back to only matching
// "specified index" (dropping the "does not project" branch) makes the
// projection error surface as a (permanent) claim error → Claim returns non-nil
// → this test FAILs at the "must fall back, not error" assertion.
func TestClaim_ProjectionMismatch_FallsBackToScan(t *testing.T) {
	const partition = "PART#misprojected"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			return nil, errors.New(projectionMismatchErr)
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem(partition, "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	logger, buf := warnBufLogger()
	s := newFakeStore(f)
	s.logger = logger

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("projection-mismatch claim must fall back to scan, not error: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("scan fallback must still claim the record, got %d", len(claimed))
	}
	if !s.claimIndexAbsent.Load() {
		t.Fatalf("a projection-mismatch ClaimIndex must be latched unusable so later Claims skip the GSI")
	}
	if got := f.queryCalls[""]; got == 0 {
		t.Fatalf("Claim must fall back to the base-table scan on a projection mismatch")
	}
	if !strings.Contains(buf.String(), "not Projection: ALL") {
		t.Fatalf("the fallback WARN must name the projection reason, got: %q", buf.String())
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

	t.Run("absent ClaimIndex stays valid (optional)", func(t *testing.T) {
		f := newFakeDDB()
		f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
			return &dynamodb.DescribeTableOutput{
				Table: outboxTableDescription(""), // ClaimIndex omitted
			}, nil
		}
		s := newFakeStore(f)

		if err := s.Preflight(context.Background()); err != nil {
			t.Fatalf("an absent (optional) ClaimIndex must not fail preflight, got %v", err)
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

// TestPersist_CrossSchemeCollision_NotDropped is the c13-review-MEDIUM
// (cross-scheme migration collision) regression. A NEW escaped SK can equal a
// PRE-UPGRADE raw SK when a producer id literally contains "%23"/"%25" — e.g.
// new sortKey("a#b","c") == old raw "OUTBOX#a%23b#c". If an old raw row still
// occupies that key, the new DISTINCT record's attribute_not_exists(SK) fails.
// Persist must NOT blind-count it a duplicate (which would ack and DROP the
// distinct message); it reads the occupant and, on an envelope/binding
// MISMATCH, surfaces a TRANSIENT error so the record is retried, never dropped.
//
// Mutation this kills: reverting Persist's conflict handling to the blind
// `duplicates++; continue` makes the colliding record silently counted a
// duplicate → the batch's other record persists → Persist returns nil (a silent
// DROP) → this test FAILs at "must surface a transient error, not drop".
func TestPersist_CrossSchemeCollision_NotDropped(t *testing.T) {
	f := newFakeDDB()
	var counter int64
	f.updateItemFn = seqCounterUpdateFn(&counter)
	// The occupying row is a DIFFERENT record (a legacy raw-key alias): its
	// envelope/binding differ from the record being persisted.
	f.getItemFn = func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			"envelope_id": &ddbtypes.AttributeValueMemberS{Value: "legacy-raw-env"},
			"binding_id":  &ddbtypes.AttributeValueMemberS{Value: "legacy-raw-bind"},
		}}, nil
	}
	putCalls := 0
	f.putItemFn = func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		putCalls++
		if putCalls == 1 {
			// The new record's escaped SK collides with a legacy raw row.
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
		t.Fatalf("a cross-scheme key collision must surface a transient error, not drop the record")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("collision must be a transient ErrUnavailable (self-heals as the legacy row drains), got %v", err)
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

// TestClaim_OldFormatRawSKRows_StillClaimable pins the c13-review-MEDIUM
// migration constraint: rows written under the PRE-UPGRADE raw-concat SK
// ("OUTBOX#order#eu#prod") must NOT be orphaned. The scan fallback finds them
// via begins_with(SK, skPrefix), and claimOne/unmarshalRecord read the
// authoritative envelope_id/binding_id ATTRIBUTES (never the SK), so a legacy
// raw SK claims cleanly.
//
// Mutation this kills: bumping skPrefix (e.g. to "OUTBOX2#") — the naive
// migration the review forbids — makes the scan's begins_with(:prefix) no
// longer match legacy "OUTBOX#..." rows → the HasPrefix assertion below FAILs,
// proving the record would be orphaned.
func TestClaim_OldFormatRawSKRows_StillClaimable(t *testing.T) {
	const (
		partition = "PART#migrating"
		oldRawSK  = "OUTBOX#order#eu#prod" // pre-upgrade raw concatenation
	)
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		// The scan fallback filters live rows by begins_with(SK, :prefix). The
		// legacy raw SK MUST still match that prefix or it is orphaned.
		prefix := strAttr(in.ExpressionAttributeValues, ":prefix")
		if !strings.HasPrefix(oldRawSK, prefix) {
			t.Fatalf("legacy raw SK %q is orphaned: scan prefix %q no longer matches it", oldRawSK, prefix)
		}
		item := pendingQueryItem(partition, oldRawSK, "rec-legacy")
		// A legacy row predates claim_sort; the scan path does not require it.
		item["envelope_id"] = &ddbtypes.AttributeValueMemberS{Value: "order"}
		item["binding_id"] = &ddbtypes.AttributeValueMemberS{Value: "eu#prod"}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{item}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)
	// Force the scan fallback: legacy rows carry no claim_sort, so they are
	// invisible to the ClaimIndex fast path (documented migration step).
	s.claimIndexAbsent.Store(true)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("claiming a legacy raw-SK row must not error: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID() != "rec-legacy" {
		t.Fatalf("legacy raw-SK pending row must be claimable, got %v", claimed)
	}
	if claimed[0].EnvelopeID() != "order" || claimed[0].BindingID() != "eu#prod" {
		t.Fatalf("legacy row identity must come from its attributes, got env=%q bind=%q",
			claimed[0].EnvelopeID(), claimed[0].BindingID())
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
