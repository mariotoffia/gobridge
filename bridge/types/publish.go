package types

import "context"

type BridgePublishOptions struct {
	// Retry indicates whether the publish operation should be retried on failure, thus only
	// permanent errors shall be interpreted as non-retirable.
	Retry bool
}

type BridgePublisher interface {
	// Publish sends a message to the remote server.
	//
	// It returns the following temporary errors:
	// Temporary/recoverable errors:
	// - ErrServerUnavailable: server unavailable - HTTP 503
	// - ErrNetworkUnavailable: network unreachable - HTTP 503
	// - ErrBrokerOverload: broker overloaded - HTTP 503
	// - ErrBackoff: backoff in effect (see RetryAfterSeconds) - HTTP 429
	// - ErrPublishTimeout: publish timed-out - HTTP 504
	//
	// Permanent / non-recoverable errors:
	// - ErrServerNotConnected: server not connected - HTTP 502
	// - ErrTopicDoesNotExist: topic does not exist - HTTP 404
	// - ErrInvalidTopicName: invalid topic name - HTTP 400
	// - ErrQoSNotSupported: specified QoS level not supported - HTTP 400
	// - ErrPayloadTooLarge: payload too large - HTTP 413
	// - ErrInvalidPayload: payload invalid - HTTP 422
	// - ErrAuthNotAllowed: authentication or authorization failed - HTTP 401
	// - ErrPublishDeniedByBroker: publish denied by broker policy - HTTP 403
	// - ErrProtocolMismatch: protocol version or feature not supported - HTTP 400
	// - ErrMessageExpired: message expired before delivery - HTTP 410
	Publish(ctx context.Context, topic string, payload BridgeMessage, opts ...BridgePublishOptions) error
}

// BridgePublisherListener defines callbacks for publish operations.
type BridgePublisherListener interface {
	// OnPublishSuccess is called when a publish operation completes successfully.
	OnPublishSuccess(topic string, payload BridgeMessage)
	// OnPublishFailure is called when a publish operation fails.
	OnPublishFailure(topic string, payload BridgeMessage, err error)
	// OnPublishRequeued is called when a publish operation is re-queued for retry since the error
	// is temporary/recoverable and it was requested by the caller.
	OnPublishRequeued(topic string, payload BridgeMessage, err error)
}
