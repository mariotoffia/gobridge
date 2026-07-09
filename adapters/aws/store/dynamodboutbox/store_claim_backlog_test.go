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

// pagedQueryFn returns a queryFn that yields totalPages QueryOutputs: every page
// but the last carries a non-nil LastEvaluatedKey so Claim keeps paging. Items
// are empty so the paging cost is isolated from claim mechanics — the deep-
// backlog WARN and MetricClaimScanPages depend only on the page count, not on
// which records get claimed.
func pagedQueryFn(totalPages int) func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
	page := 0
	return func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
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

// TestClaim_DeepBacklog_IsObservable is the FIX 2 regression. H1 made Claim page
// the WHOLE partition to guarantee oldest-first delivery, which is O(backlog): a
// deep backlog (e.g. draining after an egress outage on an exclusive session)
// must not drain quadratically and SILENTLY. Crossing deepBacklogPageWarn emits
// ONE loud WARN (throttled to once per Claim) and increments MetricClaimScanPages
// tagged by partition; a shallow backlog does neither. This also gives the
// deep-backlog path a scale test (the H1 ordering test only used backlog=60).
//
// Counterfactual: pre-FIX-2 the same deep scan happened with no WARN and no
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
		// Deep-backlog observability is a property of the SCAN fallback path
		// (c13-claim-quadratic): force it, since the default ClaimIndex fast
		// path stops at `limit` and never pages the whole partition.
		s.claimIndexAbsent.Store(true)

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
		// Force the SCAN fallback path (see the deep-backlog subtest).
		s.claimIndexAbsent.Store(true)

		if _, err := s.Claim(context.Background(), partition,
			persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got := f.queryCalls[""]; got != pages {
			t.Fatalf("expected %d pages, got %d", pages, got)
		}
		if buf.Len() != 0 {
			t.Fatalf("a shallow backlog must not WARN, got: %q", buf.String())
		}
		if entries := rec.FindEntries(MetricClaimScanPages); len(entries) != 0 {
			t.Fatalf("a shallow backlog must not emit %s, got %d", MetricClaimScanPages, len(entries))
		}
	})
}
