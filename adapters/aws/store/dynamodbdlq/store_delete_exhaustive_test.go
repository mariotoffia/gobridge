package dynamodbdlq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/routing"
)

func dlqWarnLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

func scanItem(id string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK: &ddbtypes.AttributeValueMemberS{Value: dlqKey(id)},
	}
}

// echoDelete counts every DeleteItem as a real removal (ALL_OLD echo present).
func echoDelete(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
		attrPK: in.Key[attrPK],
	}}, nil
}

// TestDeleteByScanExhaustive_UnboundedWarnsButPagesToExhaustion is the FIX 4
// regression for the truly-unbounded delete-all path (Limit<=0, no index): it
// must page the WHOLE table to exhaustion (NOT stop at maxScanPages) so no
// matching entry survives — the contract — while emitting ONE loud WARN once the
// scan crosses maxScanPages so a runaway delete-all is observable, not silent.
func TestDeleteByScanExhaustive_UnboundedWarnsButPagesToExhaustion(t *testing.T) {
	f := &fakeDLQClient{}
	const totalPages = 5
	const perPage = 2
	f.scanFn = func(call int, _ *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		items := make([]map[string]ddbtypes.AttributeValue, perPage)
		for i := range items {
			items[i] = scanItem(fmt.Sprintf("p%d-i%d", call, i))
		}
		out := &dynamodb.ScanOutput{Items: items}
		if call < totalPages {
			out.LastEvaluatedKey = map[string]ddbtypes.AttributeValue{
				attrPK: &ddbtypes.AttributeValueMemberS{Value: fmt.Sprintf("cursor-%d", call)},
			}
		}
		return out, nil
	}
	f.deleteFn = echoDelete

	logger, buf := dlqWarnLogger()
	s := newDLQStore(f, WithMaxScanPages(3))
	s.logger = logger

	// Limit<=0 + no route/category dispatches to deleteByScanExhaustive.
	n, err := s.DeleteByFilter(context.Background(), routing.DLQFilter{})
	if err != nil {
		t.Fatalf("delete-all: %v", err)
	}
	// Paged to EXHAUSTION: all 5 pages scanned even though maxScanPages is 3.
	if f.scanCalls != totalPages {
		t.Fatalf("unbounded delete-all must page to exhaustion (%d pages), got %d", totalPages, f.scanCalls)
	}
	if n != totalPages*perPage {
		t.Fatalf("every matching entry must be deleted: got %d, want %d", n, totalPages*perPage)
	}
	// Exactly ONE throttled WARN, naming the table and the running count.
	out := buf.String()
	if c := strings.Count(out, "unbounded delete-all exceeded max_scan_pages"); c != 1 {
		t.Fatalf("expected exactly ONE unbounded-delete WARN, got %d in: %q", c, out)
	}
	if !strings.Contains(out, s.tableName) {
		t.Fatalf("WARN must name the table %q, got: %q", s.tableName, out)
	}
}

// TestDeleteByScanExhaustive_HonoursContextPerItem is the FIX 4 regression for
// per-ITEM cancellation: a ~1MB scan page can hold thousands of items, and
// before this fix ctx was only checked per PAGE — a cancelled purge kept issuing
// DeleteItem for every item in the current page. Now cancellation stops promptly.
//
// Counterfactual: with only the per-page check, cancelling during the first
// item's delete would still delete all `perPage` items of the page (deleteCalls
// == perPage) before the next page's top-of-loop check tripped. The per-item
// check makes it stop after the single in-flight delete.
func TestDeleteByScanExhaustive_HonoursContextPerItem(t *testing.T) {
	const perPage = 6
	f := &fakeDLQClient{}
	f.scanFn = func(_ int, _ *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		items := make([]map[string]ddbtypes.AttributeValue, perPage)
		for i := range items {
			items[i] = scanItem(fmt.Sprintf("i%d", i))
		}
		// Non-nil LastEvaluatedKey: the scan WOULD continue if not cancelled.
		return &dynamodb.ScanOutput{
			Items:            items,
			LastEvaluatedKey: map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: "cursor"}},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.deleteFn = func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
		cancel() // cancel while the first item's delete is in flight
		return echoDelete(in)
	}
	s := newDLQStore(f)

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled delete-all must return context.Canceled, got %v", err)
	}
	if f.deleteCalls != 1 {
		t.Fatalf("per-item ctx check must stop after the single in-flight delete, got %d deletes (whole page = %d)",
			f.deleteCalls, perPage)
	}
	if n != 1 {
		t.Fatalf("count must reflect only the one delete that ran, got %d", n)
	}
	if f.scanCalls != 1 {
		t.Fatalf("cancellation must stop before the second page scan, got %d scans", f.scanCalls)
	}
}
