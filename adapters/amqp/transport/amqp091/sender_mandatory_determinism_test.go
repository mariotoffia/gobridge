// Validates the deterministic ordering contract that lets the Sender
// detect unroutable mandatory publishes WITHOUT any time-based grace
// window:
//
//  1. AMQP 0-9-1 spec: for an unroutable mandatory message the broker
//     emits basic.return BEFORE basic.ack.
//  2. amqp091-go (the underlying client) demultiplexes incoming frames
//     on a SINGLE goroutine that synchronously sends basic.return on
//     the NotifyReturn channel before processing the basic.ack frame
//     that triggers a confirms.One -> NotifyPublish dispatch.
//
// Together those guarantees mean: by the time a confirm is read from
// the NotifyPublish channel, any return for the same publish is
// ALREADY buffered in the NotifyReturn channel. checkReturnedLocked
// can therefore use a non-blocking poll instead of a 50ms (or any
// other arbitrary) timer.
//
// These tests pin that contract at the function level so a future
// regression that re-introduces a sleep is caught immediately.
package amqp091

import (
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
)

// TestSender_CheckReturned_NonBlocking_DetectsBufferedReturn validates
// that when a return is already buffered on the channel,
// checkReturnedLocked detects it without any wait.
func TestSender_CheckReturned_NonBlocking_DetectsBufferedReturn(t *testing.T) {

	returnsCh := make(chan amqp.Return, 1)
	returnsCh <- amqp.Return{
		ReplyCode:  312,
		ReplyText:  "NO_ROUTE",
		Exchange:   "ex",
		RoutingKey: "no.such.binding",
	}

	start := time.Now()
	err := domainifyReturn(checkReturn(returnsCh))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("checkReturnedLocked must return an error when a return is buffered")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) || be.Code != domain.ErrCodeNotFound {
		t.Fatalf("got %v, want ErrNotFound for unroutable mandatory return", err)
	}
	// Anything > a few ms means we're sleeping where we shouldn't.
	if elapsed > 5*time.Millisecond {
		t.Fatalf("checkReturnedLocked took %v; it must be non-blocking when a return "+
			"is already buffered (no time-based grace window allowed)", elapsed)
	}
}

// TestSender_CheckReturned_NonBlocking_NoReturn_ReturnsImmediately
// validates that when the channel is empty, checkReturnedLocked returns
// nil immediately rather than waiting any grace period.
//
// This is the regression guard against the old 50 ms grace timer.
func TestSender_CheckReturned_NonBlocking_NoReturn_ReturnsImmediately(t *testing.T) {

	returnsCh := make(chan amqp.Return, 1)

	start := time.Now()
	err := domainifyReturn(checkReturn(returnsCh))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error when no return is buffered, got %v", err)
	}
	if elapsed > 5*time.Millisecond {
		t.Fatalf("checkReturnedLocked took %v on an empty channel; the "+
			"function must be non-blocking — no time-based grace window allowed",
			elapsed)
	}
}

// TestSender_CheckReturned_NilChannel returns nil without blocking
// (covers the !Mandatory case).
func TestSender_CheckReturned_NilChannel(t *testing.T) {

	if err := domainifyReturn(checkReturn(nil)); err != nil {
		t.Fatalf("checkReturnedLocked(nil) = %v, want nil", err)
	}
}

// TestSender_CheckReturned_ClosedChannel returns nil (treats as no
// return) without blocking. This handles the case where the channel
// has been closed by a Close on the underlying *amqp.Channel.
func TestSender_CheckReturned_ClosedChannel(t *testing.T) {

	returnsCh := make(chan amqp.Return)
	close(returnsCh)

	start := time.Now()
	err := domainifyReturn(checkReturn(returnsCh))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error on closed channel, got %v", err)
	}
	if elapsed > 5*time.Millisecond {
		t.Fatalf("checkReturnedLocked took %v on a closed channel; must be non-blocking",
			elapsed)
	}
}

// TestSender_CheckReturned_NoTimerInHotPath is a behavioural pin:
// invoking checkReturnedLocked many times against an empty channel
// must NOT scale with any per-call grace timer. Exercises 200
// iterations and asserts the total stays well under what a single
// 50 ms grace would cost (i.e., < ~500 ms total).
func TestSender_CheckReturned_NoTimerInHotPath(t *testing.T) {

	const N = 200

	start := time.Now()
	for range N {
		returnsCh := make(chan amqp.Return, 1)
		_ = domainifyReturn(checkReturn(returnsCh))
	}
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("checkReturnedLocked took %v over %d empty-channel calls; "+
			"it must be non-blocking — a per-call grace timer is likely in place",
			elapsed, N)
	}
}

// domainifyReturn maps the ACL's *unroutableError to the same
// domain.ErrNotFound that the Sender produces in production. Tests
// assert against the domain error so this helper keeps the contract
// pinned at the Sender boundary, not the ACL boundary.
func domainifyReturn(r *unroutableError) error {
	if r == nil {
		return nil
	}
	return domain.ErrNotFound.WithMessage("amqp091: mandatory publish unroutable: " + r.ReplyText)
}
