package awsstore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// stubPreflighter simulates a store's Preflight outcome without any DynamoDB
// call. It counts invocations so the nil-client skip can be proven.
type stubPreflighter struct {
	err   error
	calls int
}

func (s *stubPreflighter) Preflight(context.Context) error {
	s.calls++
	return s.err
}

// warnLogger returns a logger that writes WARN+ records into buf so a test can
// assert whether (and with what attributes) the fail-open branch logged.
func warnLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return l, &buf
}

// TestPreflightPosture pins the centralized factory preflight() posture: FATAL
// for a genuine schema mismatch (shared.ErrInvalidConfig) AND fail CLOSED for a
// COULD-NOT-VERIFY error — a DescribeTable throttle, an AccessDenied on a
// least-privilege role, or DescribeTable being unsupported by an emulator. An
// inability to verify the schema is not evidence the table is valid, so it must
// block boot rather than be swallowed as success (c13-preflight-failopen). The
// only escape is the explicit WithSchemaPreflightAdvisory dev/emulator opt-out,
// which downgrades an unverifiable table to a loud WARN + fail-open.
//
// Mutation reasoning: the FATAL schema-mismatch case and the COULD-NOT-VERIFY
// throttle/AccessDenied cases enter the wrapper through the SAME seam (the
// preflighter's returned error) and diverge only on classification. Before the
// fix, every non-ErrInvalidConfig error was swallowed and returned nil; the
// default-posture throttle/AccessDenied subtests below assert a NON-nil return,
// so they fail on the unfixed (fail-open) code and pass only once the swallow is
// closed. The advisory subtests re-assert the (now opt-in) fail-open path.
func TestPreflightPosture(t *testing.T) {
	ctx := context.Background()
	// A non-nil client is required to pass the wrapper's nil-client guard; the
	// stub preflighter never touches it, so an unconfigured client is fine.
	client := dynamodb.New(dynamodb.Options{})

	t.Run("schema_mismatch_is_fatal_no_warn", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger}
		mismatch := shared.ErrInvalidConfig.WithMessage(
			`dynamodboutbox: table "outbox-prod" schema mismatch: missing GSI ExpiryIndex`)
		err := f.preflight(ctx, &stubPreflighter{err: mismatch})
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("schema mismatch must stay FATAL as shared.ErrInvalidConfig, got: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("fatal path must not WARN, got: %q", buf.String())
		}
	})

	t.Run("throttle_fails_closed_preserving_sentinel", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger}
		throttle := shared.ErrThrottled.
			WithMessage("outbox preflight: describe table failed").
			With("table", "outbox-prod")
		err := f.preflight(ctx, &stubPreflighter{err: throttle})
		if err == nil {
			t.Fatal("an unverifiable (throttled) DescribeTable must fail CLOSED, got nil " +
				"(pre-fix behaviour: swallowed as success → silent shredder on a mis-shaped table)")
		}
		if !errors.Is(err, shared.ErrThrottled) {
			t.Fatalf("fail-closed error must preserve the classified sentinel, got: %v", err)
		}
		if errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("a could-not-verify error must NOT masquerade as a schema mismatch, got: %v", err)
		}
		if !strings.Contains(err.Error(), "could not verify") {
			t.Fatalf("fail-closed error must explain it could not verify the schema, got: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("fail-closed path must not emit the advisory WARN, got: %q", buf.String())
		}
	})

	t.Run("access_denied_fails_closed", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger}
		denied := shared.ErrNotAuthorized.
			WithMessage("lease preflight: describe table failed").
			With("table", "lease-prod")
		err := f.preflight(ctx, &stubPreflighter{err: denied})
		if err == nil {
			t.Fatal("AccessDenied on DescribeTable must fail CLOSED, got nil")
		}
		if !errors.Is(err, shared.ErrNotAuthorized) {
			t.Fatalf("fail-closed error must preserve shared.ErrNotAuthorized, got: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("fail-closed path must not WARN, got: %q", buf.String())
		}
	})

	t.Run("advisory_throttle_fails_open_with_warn_naming_table", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger, preflightAdvisory: true}
		throttle := shared.ErrThrottled.
			WithMessage("outbox preflight: describe table failed").
			With("table", "outbox-prod")
		if err := f.preflight(ctx, &stubPreflighter{err: throttle}); err != nil {
			t.Fatalf("under WithSchemaPreflightAdvisory a throttle must fail OPEN (nil), got: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "schema preflight skipped") {
			t.Fatalf("advisory fail-open must emit the skip WARN, got: %q", out)
		}
		if !strings.Contains(out, "dynamodb:DescribeTable") {
			t.Fatalf("WARN must name the required IAM action, got: %q", out)
		}
		if !strings.Contains(out, "outbox-prod") {
			t.Fatalf("WARN must name the offending table, got: %q", out)
		}
	})

	t.Run("advisory_access_denied_fails_open_with_warn", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger, preflightAdvisory: true}
		denied := shared.ErrNotAuthorized.
			WithMessage("lease preflight: describe table failed").
			With("table", "lease-prod")
		if err := f.preflight(ctx, &stubPreflighter{err: denied}); err != nil {
			t.Fatalf("under WithSchemaPreflightAdvisory AccessDenied must fail OPEN (nil), got: %v", err)
		}
		if !strings.Contains(buf.String(), "schema preflight skipped") {
			t.Fatalf("advisory fail-open must emit the skip WARN, got: %q", buf.String())
		}
	})

	t.Run("clean_preflight_is_silent", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: client, logger: logger}
		if err := f.preflight(ctx, &stubPreflighter{err: nil}); err != nil {
			t.Fatalf("clean preflight must return nil, got: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("clean preflight must not WARN, got: %q", buf.String())
		}
	})

	t.Run("nil_client_skips_preflight_entirely", func(t *testing.T) {
		logger, buf := warnLogger()
		f := &DynamoDBStoreFactory{client: nil, logger: logger}
		stub := &stubPreflighter{err: shared.ErrInvalidConfig}
		if err := f.preflight(ctx, stub); err != nil {
			t.Fatalf("nil-client factory (wiring unit tests) must skip preflight, got: %v", err)
		}
		if stub.calls != 0 {
			t.Fatalf("nil-client guard must short-circuit BEFORE calling Preflight, calls=%d", stub.calls)
		}
		if buf.Len() != 0 {
			t.Fatalf("skipped preflight must not WARN, got: %q", buf.String())
		}
	})

	t.Run("fail_closed_without_logger_returns_error", func(t *testing.T) {
		f := &DynamoDBStoreFactory{client: client, logger: nil}
		err := f.preflight(ctx, &stubPreflighter{err: shared.ErrThrottled})
		if err == nil {
			t.Fatal("fail-closed with a nil logger must still return the error, got nil")
		}
		if !errors.Is(err, shared.ErrThrottled) {
			t.Fatalf("fail-closed error must preserve shared.ErrThrottled, got: %v", err)
		}
	})

	t.Run("advisory_fail_open_without_logger_does_not_panic", func(t *testing.T) {
		f := &DynamoDBStoreFactory{client: client, logger: nil, preflightAdvisory: true}
		if err := f.preflight(ctx, &stubPreflighter{err: shared.ErrThrottled}); err != nil {
			t.Fatalf("advisory fail-open with a nil logger must return nil, got: %v", err)
		}
	})
}
