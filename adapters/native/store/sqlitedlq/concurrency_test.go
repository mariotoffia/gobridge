package sqlitedlq_test

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// Compile-time assertion that the SQLite DLQ store exposes io.Closer so a
// lifecycle-aware composition root can release its file handle on stop/reload.
var _ io.Closer = (*sqlitedlq.Store)(nil)

// TestConcurrentWrites_NoBusyNoLoss is the regression for the DLQ: many
// goroutines writing distinct entries to a single file-backed store must all
// succeed (no SQLITE_BUSY) and every entry must land. The single writer
// connection plus busy_timeout are what make this hold.
func TestConcurrentWrites_NoBusyNoLoss(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitedlq.NewStore(filepath.Join(dir, "dlq.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	failedAt := time.Unix(1_700_000_000, 0)
	const total = 200

	errCh := make(chan error, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			entry := routing.NewDLQEntry(routing.DLQEntrySpec{
				ID:        "dlq-" + strconv.Itoa(id),
				RouteID:   "route-1",
				BindingID: "bind",
				Reason:    "concurrent",
				Category:  "timeout",
				ErrorCode: "TEST",
				LastError: "boom",
				FailedAt:  failedAt,
				Attempts:  1,
			})
			if err := s.Write(ctx, entry); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent write error: %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != total {
		t.Fatalf("expected %d entries, got %d", total, len(got))
	}
}
