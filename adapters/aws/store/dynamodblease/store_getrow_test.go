package dynamodblease_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// TestGetRowSurfacesCorruptVersion is the MINOR regression: a lease row whose
// fencing `version` attribute is PRESENT but unparseable must surface a read
// error rather than being silently coerced to 0.
//
// Lease rows ARE the fencing counter of record. Silently reading a corrupt
// fence as 0 resets the counter below the outbox high-water mark, so every
// subsequent claim fails with ErrStaleFencingToken — a partition-wide stall.
// Both read paths are covered: Current (direct) and Acquire's takeover getRow.
//
// Counterfactual: reverting getRow/Current to `numAttr(...)` with a discarded
// error makes Current return (LeaseInfo{Version:0}, nil) — no error — and this
// test fails.
func TestGetRowSurfacesCorruptVersion(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-corrupt")
	store := dynamodblease.NewStore(client, dynamodblease.WithTableName(table))
	ctx := context.Background()

	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	const leaseID = "corrupt-fence"
	// A 30-digit value is a valid DynamoDB Number (the write succeeds) but
	// overflows uint64, so ParseUint fails — exactly the "unparsable numeric"
	// the pre-fix getRow/Current silently zeroed.
	const corruptVersion = "111111111111111111111111111111"
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &table,
		Item: map[string]ddbtypes.AttributeValue{
			"PK":         &ddbtypes.AttributeValueMemberS{Value: "LEASE#" + leaseID},
			"owner":      &ddbtypes.AttributeValueMemberS{Value: "owner-1"},
			"version":    &ddbtypes.AttributeValueMemberN{Value: corruptVersion},
			"expires_at": &ddbtypes.AttributeValueMemberN{Value: "9999999999999"},
			"renewed_at": &ddbtypes.AttributeValueMemberN{Value: "1700000000000"},
		},
	}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}

	// Current (direct read) must surface the corrupt fence, not return Version 0.
	info, err := store.Current(ctx, leaseID)
	if err == nil {
		t.Fatalf("Current must surface a corrupt fence, got version=%d (silent fence reset)", info.Version)
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "corrupt lease row") {
		t.Fatalf("Current error must identify the unparseable fence attribute, got: %v", err)
	}

	// Acquire's takeover path reads via getRow; a corrupt fence there must also
	// surface (not be mistaken for a version-0/fresh lease). On the pre-fix code
	// getRow returns version 0 and Acquire proceeds down the observation path,
	// so asserting the specific parse error — not merely "an error" — is what
	// gives this leg teeth.
	tok, err := store.Acquire(ctx, leaseID, "owner-2", 30*time.Second, nil)
	if err == nil {
		t.Fatalf("Acquire takeover must surface a corrupt fence, got token version=%d (silent fence reset)", tok.Version)
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "corrupt lease row") {
		t.Fatalf("Acquire error must identify the unparseable fence attribute, got: %v", err)
	}
}
