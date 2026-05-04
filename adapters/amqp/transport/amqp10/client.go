package amqp10

import (
	"context"
	"fmt"

	"github.com/Azure/go-amqp"
)

// amqpConn is the subset of *amqp.Conn used by Session.
// It enables test-double injection.
type amqpConn interface {
	NewSession(ctx context.Context, opts *amqp.SessionOptions) (*amqp.Session, error)
	Close() error
}

var _ amqpConn = (*amqp.Conn)(nil)

// amqpSenderLink is the subset of *amqp.Sender used by Sender. It exists
// so unit tests can inject a fake link to verify concurrency properties
// of Send (which releases the Sender mutex before invoking link.Send).
//
// Per the go-amqp documentation, *amqp.Sender.Send is safe for
// concurrent use, so the contract for any implementation of this
// interface is the same: Send may be invoked from many goroutines at
// once. Close may be invoked at most once and runs concurrently with
// in-flight Send calls — implementations should make in-flight Sends
// either complete or return an error.
type amqpSenderLink interface {
	Send(ctx context.Context, msg *amqp.Message, opts *amqp.SendOptions) error
	Close(ctx context.Context) error
}

var _ amqpSenderLink = (*amqp.Sender)(nil)

// dialFunc abstracts the AMQP 1.0 dial operation for test-double injection.
type dialFunc func(ctx context.Context, addr string, opts *amqp.ConnOptions) (amqpConn, error)

// defaultDial wraps amqp.Dial to satisfy dialFunc. The dial error is
// wrapped with package context so wrapcheck is satisfied at this
// adapter boundary; MapError at the call site uses errors.Is/As, which
// traverse the %w chain unchanged.
func defaultDial(ctx context.Context, addr string, opts *amqp.ConnOptions) (amqpConn, error) {
	conn, err := amqp.Dial(ctx, addr, opts)
	if err != nil {
		return nil, fmt.Errorf("amqp10: dial: %w", err)
	}
	return conn, nil
}
