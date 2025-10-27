package types

import "context"

type PublishOptions struct {
	// Retry indicates whether the publish operation should be retried on failure, thus only
	// permanent errors shall be interpreted as non-retirable.
	Retry bool
}

type Publisher interface {
	// Publish sends a message to the remote server.
	//
	// It returns the following temporary errors:
	// Temporary/recoverable errors:
	//
	// - ErrServerUnavailable: server unavailable - HTTP 503
	//
	// - ErrNetworkUnavailable: network unreachable - HTTP 503
	//
	// - ErrBrokerOverload: broker overloaded - HTTP 503
	//
	// - ErrBackoff: backoff in effect (see RetryAfterSeconds) - HTTP 429
	//
	// - ErrPublishTimeout: publish timed-out - HTTP 504
	//
	// - ErrTemporaryAuthFailed: authentication or authorization failed - HTTP 401
	//
	// Permanent / non-recoverable errors:
	//
	// - ErrServerNotConnected: server not connected - HTTP 502
	//
	// - ErrTopicDoesNotExist: topic does not exist - HTTP 404
	//
	// - ErrInvalidTopicName: invalid topic name - HTTP 400
	//
	// - ErrQoSNotSupported: specified QoS level not supported - HTTP 400
	//
	// - ErrPayloadTooLarge: payload too large - HTTP 413
	//
	// - ErrInvalidPayload: payload invalid - HTTP 422
	//
	// - ErrPublishDeniedByBroker: publish denied by broker policy - HTTP 403
	//
	// - ErrProtocolMismatch: protocol version or feature not supported - HTTP 400
	//
	// - ErrMessageExpired: message expired before delivery - HTTP 410
	Publish(ctx context.Context, topic string, payload Message, opts PublishOptions) error
}

// PublisherAdapter is an adapter to allow the use of ordinary functions as `Publisher` interfaces.
type PublisherAdapter func(ctx context.Context, topic string, payload Message, opts PublishOptions) error

// Publish calls the underlying function of the PublisherFunc.
func (f PublisherAdapter) Publish(ctx context.Context, topic string, payload Message, opts PublishOptions) error {
	return f(ctx, topic, payload, opts)
}

// ChainablePublisher is an interface that allows chaining of publishers to create middleware-like behavior.
//
// The last one should be the actual publisher that sends the message to the remote server.
type ChainablePublisher interface {
	Next(ctx context.Context, topic string, payload Message, next ChainablePublisher, opts PublishOptions) error
}

// PublisherMiddleware is a function type that defines middleware for the `Publisher` interface.
type PublisherMiddleware func(next Publisher) Publisher

// ChainPublisher is a helper to chain multiple `PublisherMiddleware` into a single `Publisher` where
// the last one is the actual publisher.
func ChainPublisher(p Publisher, middlewares ...PublisherMiddleware) Publisher {
	for i := len(middlewares) - 1; i >= 0; i-- {
		p = middlewares[i](p)
	}
	return p
}
