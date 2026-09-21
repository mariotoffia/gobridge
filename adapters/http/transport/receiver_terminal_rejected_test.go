package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// terminalFailureRecorder mirrors the optional capability the runtime uses
// to report a terminal, non-delivering settle before it acks.
type terminalFailureRecorder interface {
	RecordTerminalFailure(cause error)
}

// runTerminalReceiver runs recv with an emit that, on the FIRST delivery
// only, reports cause as a terminal failure before acking — the runtime's
// DLQ/drop settle. Later deliveries ack normally.
func runTerminalReceiver(t *testing.T, recv ports.Receiver, cause error, emits *atomic.Int64) {
	t.Helper()
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(ctx context.Context, d ports.Delivery) error {
			if emits.Add(1) == 1 && cause != nil {
				tf, ok := d.(terminalFailureRecorder)
				if !ok {
					t.Errorf("http delivery %T does not implement RecordTerminalFailure", d)
				} else {
					tf.RecordTerminalFailure(cause)
				}
			}
			return d.Ack(ctx)
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)
}

func TestReceiver_TerminalRejectedCause_Answers400AndDoesNotRecordKey(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "term-rejected"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	var emits atomic.Int64
	cause := shared.ErrAddressTemplate.WithMessage(`binding "b1": address template error: missing tenant`)
	runTerminalReceiver(t, recv, cause, &emits)

	url := "/transport/http/receivers/term-rejected/messages"
	headers := map[string]string{"Idempotency-Key": "rejected-key-1"}
	body := map[string]any{"subject": "t.rejected", "payload": json.RawMessage(`{}`)}

	res := postJSON(t, factory.Handler(), url, body, headers)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("rejected terminal cause: expected 400, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "missing tenant") {
		t.Fatalf("400 body must carry the cause's message, got %s", res.Body.String())
	}
	// The key must NOT be recorded: a corrected resend is processed, not
	// absorbed as a duplicate.
	if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
		t.Fatalf("resend after 400: expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if n := emits.Load(); n != 2 {
		t.Fatalf("expected 2 emits (rejected key not recorded), got %d", n)
	}
}

func TestReceiver_TerminalCauseNotBadRequest_Answers200(t *testing.T) {
	cases := []struct {
		name  string
		cause error
	}{
		{"success", nil},
		{"permanent_terminal_cause", shared.ErrNotFound.WithMessage("no destination")},
		// A filter drop is rejected-class but is operator policy, not a bad
		// request: it keeps 200 and the recorded key.
		{"filtered_terminal_cause", shared.ErrMessageFiltered.WithMessage("dropped by allow-list")},
		{"plain_error_terminal_cause", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := transport.NewFactory()
			id := "term-" + strings.ReplaceAll(tc.name, "_", "-")
			recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: id}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}
			var emits atomic.Int64
			runTerminalReceiver(t, recv, tc.cause, &emits)

			url := "/transport/http/receivers/" + id + "/messages"
			headers := map[string]string{"Idempotency-Key": "key-" + tc.name}
			body := map[string]any{"subject": "t.ok", "payload": json.RawMessage(`{}`)}

			if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
			}
			// Unchanged behaviour: the key is recorded, so a resend is absorbed.
			if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
				t.Fatalf("resend: expected 200, got %d", res.Code)
			}
			if n := emits.Load(); n != 1 {
				t.Fatalf("expected 1 emit (key recorded), got %d", n)
			}
		})
	}
}
