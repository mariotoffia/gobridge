package amqp10

import (
	"context"

	"github.com/Azure/go-amqp"
)

// amqpConn is the subset of *amqp.Conn used by Session.
// It enables test-double injection.
type amqpConn interface {
	NewSession(ctx context.Context, opts *amqp.SessionOptions) (*amqp.Session, error)
	Close() error
}

var _ amqpConn = (*amqp.Conn)(nil)

// dialFunc abstracts the AMQP 1.0 dial operation for test-double injection.
type dialFunc func(ctx context.Context, addr string, opts *amqp.ConnOptions) (amqpConn, error)

// defaultDial wraps amqp.Dial to satisfy dialFunc.
func defaultDial(ctx context.Context, addr string, opts *amqp.ConnOptions) (amqpConn, error) {
	return amqp.Dial(ctx, addr, opts)
}
