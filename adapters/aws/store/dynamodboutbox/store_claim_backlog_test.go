package dynamodboutbox

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// pagedQueryFn returns a queryFn that routes a claim onto the base-table SCAN
// path and then yields totalPages QueryOutputs from it: every page but the last
// carries a non-nil LastEvaluatedKey so the scan keeps paging. Base-table items
// are empty so the paging cost is isolated from claim mechanics — the
// deep-backlog WARN and MetricClaimScanPages depend only on the page count, not
// on which records get claimed.
//
// The ClaimIndex query answers with a single ORDERING-KEYED candidate, which is
// what hands the claim to the strongly consistent scan: an eventually consistent
// index cannot prove a keyed record has no older unseen sibling. That is the
// only route to the scan path now that ClaimIndex is a required index rather
// than an optional one Claim could degrade away from.
func pagedQueryFn(totalPages int) func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
	page := 0
	return func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
				keyedQueryItem("PART#deep", "OUTBOX#env-k#bind", "rec-k", "deep-key", 1_700_000_000_000, 1),
			}}, nil
		}
		page++
		out := &dynamodb.QueryOutput{}
		if page < totalPages {
			out.LastEvaluatedKey = map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: "PART#deep"},
				"SK": &ddbtypes.AttributeValueMemberS{Value: fmt.Sprintf("OUTBOX#page-%d", page)},
			}
		}
		return out, nil
	}
}

func warnBufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// TestClaim_DeepBacklog_IsObservable guards the claim cost. On the strongly
// consistent scan path — which every ordering-keyed partition uses — Claim pages
// the WHOLE partition to guarantee oldest-first delivery, which is O(backlog): a
// deep backlog (e.g. draining after an egress outage on an exclusive session)
// must not drain quadratically and SILENTLY. Crossing deepBacklogPageWarn emits
// ONE loud WARN (throttled to once per Claim) and increments MetricClaimScanPages
// tagged by partition; a shallow backlog does neither. This also gives the
// deep-backlog path a scale test (the ordering test only used backlog=60).
//
// Counterfactual: before the fix the same deep scan happened with no WARN and no
// counter, so an operator recovering from an outage had no signal that each
// Claim was re-reading the whole partition.
func TestClaim_DeepBacklog_IsObservable(t *testing.T) {
	const partition = "PART#deep"

	t.Run("deep backlog warns once and counts pages", func(t *testing.T) {
		f := newFakeDDB()
		f.getItemFn = fenceGetItem("0")
		const pages = deepBacklogPageWarn + 4
		f.queryFn = pagedQueryFn(pages)

		rec := &ports.RecordingExporter{}
		logger, buf := warnBufLogger()
		s := newFakeStore(f)
		s.metrics = rec
		s.logger = logger

		claimed, err := s.Claim(context.Background(), partition,
			persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
		if err != nil {
			t.Fatalf("claim over a deep backlog must not error: %v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("empty pages yield no claim; got %d", len(claimed))
		}
		// Proof it paged the WHOLE partition to exhaustion.
		if got := f.queryCalls[""]; got != pages {
			t.Fatalf("expected Claim to page the whole partition (%d pages), got %d", pages, got)
		}

		// Exactly ONE WARN (throttled), naming the partition.
		out := buf.String()
		if n := strings.Count(out, "deep outbox backlog"); n != 1 {
			t.Fatalf("expected exactly ONE throttled deep-backlog WARN, got %d in: %q", n, out)
		}
		if !strings.Contains(out, partition) {
			t.Fatalf("WARN must name the partition %q, got: %q", partition, out)
		}

		// Counter emitted once with the page count, tagged by partition.
		entries := rec.FindEntries(MetricClaimScanPages)
		if len(entries) != 1 {
			t.Fatalf("expected exactly 1 %s counter, got %d", MetricClaimScanPages, len(entries))
		}
		if entries[0].Kind != "counter" || entries[0].IValue != int64(pages) {
			t.Fatalf("counter must record the page count (%d), got %+v", pages, entries[0])
		}
		var tagged bool
		for _, tag := range entries[0].Tags {
			if tag.Key == shared.TagKeyPartition && tag.Value == partition {
				tagged = true
			}
		}
		if !tagged {
			t.Fatalf("counter must carry partition tag %q=%s, tags=%v",
				shared.TagKeyPartition, partition, entries[0].Tags)
		}
	})

	t.Run("shallow backlog is silent", func(t *testing.T) {
		f := newFakeDDB()
		f.getItemFn = fenceGetItem("0")
		const pages = deepBacklogPageWarn - 5
		f.queryFn = pagedQueryFn(pages)

		rec := &ports.RecordingExporter{}
		logger, buf := warnBufLogger()
		s := newFakeStore(f)
		s.metrics = rec
		s.logger = logger

		if _, err := s.Claim(context.Background(), partition,
			persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got := f.queryCalls[""]; got != pages {
			t.Fatalf("expected %d pages, got %d", pages, got)
		}
		// The ordering-key bypass WARN is expected here (it is what routed the
		// claim to the scan); what a shallow backlog must NOT produce is the
		// deep-backlog WARN.
		if out := buf.String(); strings.Contains(out, "deep outbox backlog") {
			t.Fatalf("a shallow backlog must not emit the deep-backlog WARN, got: %q", out)
		}
		if entries := rec.FindEntries(MetricClaimScanPages); len(entries) != 0 {
			t.Fatalf("a shallow backlog must not emit %s, got %d", MetricClaimScanPages, len(entries))
		}
	})
}
