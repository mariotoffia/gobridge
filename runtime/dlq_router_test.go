package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies routing with a nil DLQ store is a no-op and returns no error.
func TestDLQRouter_NilStore(t *testing.T) {
	dlq := runtime.NewDLQRouter(nil)
	env := &messaging.Envelope{ID: "msg-1"}

	err := dlq.Route(context.Background(), env, "route-1", "", "", "", "", shared.ErrNotFound, 1)
	if err != nil {
		t.Fatalf("nil store should be a no-op, got %v", err)
	}
}

// Verifies Route persists one entry with route, correlation, permanent category, and attempt count.
func TestDLQRouter_WritesEntry(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	env := &messaging.Envelope{
		ID:      "msg-1",
		Headers: map[string]any{messaging.HeaderCorrelationID: "corr-abc"},
	}

	err := dlq.Route(context.Background(), env, "route-1", "bind-1", "", "sess-1", "src-1", shared.ErrNotFound, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	if entry.RouteID != "route-1" {
		t.Fatalf("expected route 'route-1', got %q", entry.RouteID)
	}
	if entry.CorrelationID != "corr-abc" {
		t.Fatalf("expected correlation ID 'corr-abc', got %q", entry.CorrelationID)
	}
	if entry.Category != string(shared.ErrorPermanent) {
		t.Fatalf("expected category 'permanent', got %q", entry.Category)
	}
	if entry.Attempts != 3 {
		t.Fatalf("expected attempts 3, got %d", entry.Attempts)
	}
}

// Verifies non-domain errors are categorized as unknown in the DLQ entry.
func TestDLQRouter_UnknownError(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	env := &messaging.Envelope{ID: "msg-1"}
	_ = dlq.Route(context.Background(), env, "r", "", "", "", "", context.DeadlineExceeded, 0)

	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}
	if store.Entries[0].Category != "unknown" {
		t.Fatalf("expected category 'unknown', got %q", store.Entries[0].Category)
	}
}

// Verifies HasStore returns true when a store is configured.
func TestDLQRouter_HasStore_True(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	if !dlq.HasStore() {
		t.Fatal("HasStore() should return true when store is non-nil")
	}
}

// Verifies HasStore returns false when store is nil.
func TestDLQRouter_HasStore_False(t *testing.T) {
	dlq := runtime.NewDLQRouter(nil)

	if dlq.HasStore() {
		t.Fatal("HasStore() should return false when store is nil")
	}
}

// Documents RISK: Reason and LastError contain raw err.Error() strings,
// which may include infrastructure details such as hostnames and ports.
// TestDLQRouter_Route_RedactsErrorDetails verifies that DLQ entries use
// sanitized error reasons rather than raw err.Error() to prevent information
// disclosure of internal infrastructure details.
func TestDLQRouter_Route_RedactsErrorDetails(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)
	env := &messaging.Envelope{ID: "msg-redact"}

	rawErr := shared.ErrConnectionLost.Wrap(
		fmt.Errorf("connection to db-prod.internal:5432 refused"),
	)

	err := dlq.Route(context.Background(), env, "route-redact", "bind-1", "", "sess-1", "src-1", rawErr, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	const sensitiveSubstring = "db-prod.internal:5432"
	if contains(entry.Reason, sensitiveSubstring) {
		t.Fatalf("Reason should NOT contain sensitive details %q, got %q", sensitiveSubstring, entry.Reason)
	}
	if contains(entry.LastError, sensitiveSubstring) {
		t.Fatalf("LastError should NOT contain sensitive details %q, got %q", sensitiveSubstring, entry.LastError)
	}
	if entry.Reason != "connection lost" {
		t.Fatalf("Reason should be the BridgeError message, got %q", entry.Reason)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Verifies that when the DLQ store returns a write error, Route propagates it.
func TestDLQRouter_Route_StoreWriteError_Propagated(t *testing.T) {
	store := NewFakeDLQStore()
	store.WriteErr = fmt.Errorf("disk full")
	dlq := runtime.NewDLQRouter(store)
	env := &messaging.Envelope{ID: "msg-fail"}

	err := dlq.Route(context.Background(), env, "r", "", "", "", "", shared.ErrNotFound, 1)
	if err == nil {
		t.Fatal("expected store write error to be propagated")
	}
	if err.Error() != "disk full" {
		t.Fatalf("expected error 'disk full', got %q", err.Error())
	}
}

// Verifies all DLQEntry fields are correctly populated from the Route call.
func TestDLQRouter_Route_AllFieldsPopulated(t *testing.T) {
	store := NewFakeDLQStore()
	failedAt := time.Date(2026, 5, 4, 12, 34, 56, 789, time.UTC)
	clk := clocktest.NewAt(failedAt)
	dlq := runtime.NewDLQRouterFromConfig(runtime.DLQRouterConfig{
		Store: store,
		Clock: clk,
	})

	env := &messaging.Envelope{
		ID:      "msg-all",
		Subject: "test/topic",
		Payload: []byte("payload-data"),
		Headers: map[string]any{messaging.HeaderCorrelationID: "corr-xyz"},
	}

	routeErr := shared.ErrThrottled
	err := dlq.Route(context.Background(), env, "route-all", "bind-all", "", "sess-all", "src-all", routeErr, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	if entry.ID == "" {
		t.Fatal("entry ID should be generated and non-empty")
	}
	if entry.Envelope.ID != "msg-all" {
		t.Fatalf("expected envelope ID 'msg-all', got %q", entry.Envelope.ID)
	}
	if entry.RouteID != "route-all" {
		t.Fatalf("expected RouteID 'route-all', got %q", entry.RouteID)
	}
	if entry.BindingID != "bind-all" {
		t.Fatalf("expected BindingID 'bind-all', got %q", entry.BindingID)
	}
	if entry.SessionID != "sess-all" {
		t.Fatalf("expected SessionID 'sess-all', got %q", entry.SessionID)
	}
	if entry.SourceID != "src-all" {
		t.Fatalf("expected SourceID 'src-all', got %q", entry.SourceID)
	}
	if entry.CorrelationID != "corr-xyz" {
		t.Fatalf("expected CorrelationID 'corr-xyz', got %q", entry.CorrelationID)
	}
	if entry.Reason == "" {
		t.Fatal("Reason should not be empty")
	}
	if entry.Category != string(shared.ErrorTransient) {
		t.Fatalf("expected category %q, got %q", shared.ErrorTransient, entry.Category)
	}
	if entry.ErrorCode != string(shared.ErrCodeThrottled) {
		t.Fatalf("expected error code %q, got %q", shared.ErrCodeThrottled, entry.ErrorCode)
	}
	if entry.LastError == "" {
		t.Fatal("LastError should not be empty")
	}
	if !entry.FailedAt.Equal(failedAt) {
		t.Fatalf("expected FailedAt from injected clock %s, got %s", failedAt, entry.FailedAt)
	}
	if entry.Attempts != 7 {
		t.Fatalf("expected Attempts 7, got %d", entry.Attempts)
	}
}

// Verifies that when the envelope has no correlation ID header, CorrelationID is empty.
func TestDLQRouter_Route_NoCorrelationID(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	env := &messaging.Envelope{ID: "msg-nocorr"}

	err := dlq.Route(context.Background(), env, "route-nocorr", "", "", "", "", shared.ErrNotFound, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}
	if store.Entries[0].CorrelationID != "" {
		t.Fatalf("expected empty CorrelationID, got %q", store.Entries[0].CorrelationID)
	}
}

// Verifies classifyError with a BridgeError returns the correct class and code.
func TestDLQRouter_ClassifyError_BridgeError(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)
	env := &messaging.Envelope{ID: "msg-bridge"}

	testCases := []struct {
		name         string
		err          error
		wantCategory string
		wantCode     string
	}{
		{
			name:         "transient/throttled",
			err:          shared.ErrThrottled,
			wantCategory: string(shared.ErrorTransient),
			wantCode:     string(shared.ErrCodeThrottled),
		},
		{
			name:         "permanent/not_found",
			err:          shared.ErrNotFound,
			wantCategory: string(shared.ErrorPermanent),
			wantCode:     string(shared.ErrCodeNotFound),
		},
		{
			name:         "rejected/invalid_payload",
			err:          shared.ErrInvalidPayload,
			wantCategory: string(shared.ErrorRejected),
			wantCode:     string(shared.ErrCodeInvalidPayload),
		},
		{
			name:         "expired/message_expired",
			err:          shared.ErrMessageExpired,
			wantCategory: string(shared.ErrorExpired),
			wantCode:     string(shared.ErrCodeMessageExpired),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store.Entries = nil
			err := dlq.Route(context.Background(), env, "route-classify", "", "", "", "", tc.err, 1)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if store.Count() != 1 {
				t.Fatalf("expected 1 entry, got %d", store.Count())
			}
			entry := store.Entries[0]
			if entry.Category != tc.wantCategory {
				t.Fatalf("expected category %q, got %q", tc.wantCategory, entry.Category)
			}
			if entry.ErrorCode != tc.wantCode {
				t.Fatalf("expected error code %q, got %q", tc.wantCode, entry.ErrorCode)
			}
		})
	}
}
