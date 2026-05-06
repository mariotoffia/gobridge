package sqlitedlq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMapError_PreservesContextErrors asserts policy Rule 1
// (`_design/error-wrapping-policy.adoc:100-104`): canonical context
// sentinels are returned identity-equal and never reclassified as
// shared.ErrTimeout / shared.ErrUnavailable.
func TestMapError_PreservesContextErrors(t *testing.T) {
	wrappedDeadline := fmt.Errorf("sqlite call: %w", context.DeadlineExceeded)
	wrappedCanceled := fmt.Errorf("sqlite call: %w", context.Canceled)

	tests := []struct {
		name  string
		input error
		check func(t *testing.T, in, out error)
	}{
		{
			name:  "direct-deadline-exceeded",
			input: context.DeadlineExceeded,
			check: func(t *testing.T, _, out error) {
				if out != context.DeadlineExceeded {
					t.Fatalf("want identity-equal context.DeadlineExceeded, got %v (%T)", out, out)
				}
				if !errors.Is(out, context.DeadlineExceeded) {
					t.Fatalf("errors.Is(out, context.DeadlineExceeded) = false")
				}
				if errors.Is(out, shared.ErrTimeout) {
					t.Fatalf("ctx error must not be classified as shared.ErrTimeout")
				}
			},
		},
		{
			name:  "direct-canceled",
			input: context.Canceled,
			check: func(t *testing.T, _, out error) {
				if out != context.Canceled {
					t.Fatalf("want identity-equal context.Canceled, got %v (%T)", out, out)
				}
				if !errors.Is(out, context.Canceled) {
					t.Fatalf("errors.Is(out, context.Canceled) = false")
				}
				if errors.Is(out, shared.ErrUnavailable) {
					t.Fatalf("ctx error must not be classified as shared.ErrUnavailable")
				}
			},
		},
		{
			name:  "wrapped-deadline-exceeded",
			input: wrappedDeadline,
			check: func(t *testing.T, in, out error) {
				if out != in {
					t.Fatalf("want identity-equal wrapped input, got %v", out)
				}
				if !errors.Is(out, context.DeadlineExceeded) {
					t.Fatalf("errors.Is(out, context.DeadlineExceeded) = false")
				}
				if errors.Is(out, shared.ErrTimeout) {
					t.Fatalf("wrapped ctx error must not be classified as shared.ErrTimeout")
				}
			},
		},
		{
			name:  "wrapped-canceled",
			input: wrappedCanceled,
			check: func(t *testing.T, in, out error) {
				if out != in {
					t.Fatalf("want identity-equal wrapped input, got %v", out)
				}
				if !errors.Is(out, context.Canceled) {
					t.Fatalf("errors.Is(out, context.Canceled) = false")
				}
				if errors.Is(out, shared.ErrUnavailable) {
					t.Fatalf("wrapped ctx error must not be classified as shared.ErrUnavailable")
				}
			},
		},
		{
			name:  "no-rows",
			input: sql.ErrNoRows,
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, shared.ErrNotFound) {
					t.Fatalf("sql.ErrNoRows must classify as shared.ErrNotFound, got %v", out)
				}
			},
		},
		{
			name:  "unique-violation-string-match",
			input: errors.New("constraint failed: UNIQUE constraint failed: dlq.id"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, shared.ErrDuplicateRecord) {
					t.Fatalf("UNIQUE constraint must classify as shared.ErrDuplicateRecord, got %v", out)
				}
			},
		},
		{
			name:  "busy-locked-string-match",
			input: errors.New("database is locked"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, shared.ErrThrottled) {
					t.Fatalf("database is locked must classify as shared.ErrThrottled, got %v", out)
				}
			},
		},
		{
			name:  "io-eof",
			input: io.EOF,
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, shared.ErrConnectionLost) {
					t.Fatalf("io.EOF must classify as shared.ErrConnectionLost, got %v", out)
				}
			},
		},
		{
			name:  "generic-error-fallback",
			input: errors.New("some random sqlite error"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, shared.ErrUnavailable) {
					t.Fatalf("unclassified error must default to shared.ErrUnavailable, got %v", out)
				}
			},
		},
		{
			name:  "nil-input",
			input: nil,
			check: func(t *testing.T, _, out error) {
				if out != nil {
					t.Fatalf("mapError(nil) want nil, got %v", out)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := mapError(tc.input)
			tc.check(t, tc.input, out)
		})
	}
}

// TestWrapErr_ContextSentinelPassThrough asserts that wrapErr does not
// wrap canonical ctx sentinels and never produces a *shared.BridgeError
// from them.
func TestWrapErr_ContextSentinelPassThrough(t *testing.T) {
	out := wrapErr(context.Canceled, "sqlitedlq: write", "entryID", "abc")
	if out != context.Canceled {
		t.Fatalf("wrapErr must pass ctx.Canceled through unchanged, got %v (%T)", out, out)
	}
	if _, ok := out.(*shared.BridgeError); ok {
		t.Fatalf("wrapErr must not produce a *shared.BridgeError from ctx sentinel")
	}
}

// TestWrapErr_AttachesKVs asserts wrapErr classifies and decorates a
// non-ctx error with the supplied kvs.
func TestWrapErr_AttachesKVs(t *testing.T) {
	out := wrapErr(sql.ErrNoRows, "sqlitedlq: get", "entryID", "abc")
	if !errors.Is(out, shared.ErrNotFound) {
		t.Fatalf("want errors.Is(out, ErrNotFound), got %v", out)
	}
	be, ok := shared.AsBridgeError(out)
	if !ok {
		t.Fatalf("want *shared.BridgeError, got %T", out)
	}
	if be.Message == "" {
		t.Fatalf("want message attached, got empty")
	}
}
