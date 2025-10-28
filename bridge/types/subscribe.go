package types

import "context"

// AddSubscriberOptions is a configuration for a topic subscription.
type AddSubscriberOptions struct {
	// Custom is optional parameters for the subscription.
	Custom map[string]any
}

// SubscriberSource is a source "server" that listens on a remote server for messages
// and forwards them to registered `Subscriber` instances.
//
// Often, this is equal to a "connection" to the remote server (if support of multiplex topics on same connection).
//
// For each received message, it calls the registered `Subscriber` for the topic (and invokes `Subscriber.Process`).
type SubscriberSource interface {
	// AddSubscriber registers a consumer callback for the specified topics.
	//
	// NOTE: The topics can be concrete topic string or contain wildcards, depending on the
	// server capabilities.
	//
	// It returns an error if the subscription could not be created.
	//
	// A _topic_ may have multiple subscribers registered, if a already registered _id_ on _topic_
	// an `ErrSubscriptionAlreadyExists` is returned.
	//
	// The optional _opts_ can be used to provide additional subscription configuration when registering
	// the subscriber. Depending on the `Connection` implementation, it may ignore the options.
	//
	// The _id_ is a unique identifier for the _subscriber_.
	//
	// NOTE: Topics are managed separately, thus when a subscription is made and topics has been added, it may be
	// that the subscription will get from more topics due to e.g. wildcards. The implementation will not check
	// if the topic exists when the `AddSubscriber` is called.
	//
	// Errors that may be returned:
	//
	// - ErrSubscriptionAlreadyExists: if the subscriber with the same _id_ is already registered for the specified _topic_.
	//
	// - ErrSubscriptionInvalidTopicName: if the specified _topic_ is invalid.
	AddSubscriber(id, topic string, subscriber Subscriber, opts ...AddSubscriberOptions) error
	// RemoveSubscriber removes a previously registered consumer _id_ for the specified _topic_.
	//
	// It returns an error if the un-subscribe could not be performed.
	//
	// It will use the _id_ pointer to identify the correct `Subscriber` for the specified _topic_ to remove.
	// If _id_ and _topic_ combination is not found, it returns a `ErrNotFound` (_404_) error.
	//
	// Topic addition and removal are managed separately, thus removing a subscriber does not
	// unsubscribe from the remote server.
	//
	// Errors that may be returned:
	//
	// - ErrNotFound: if the subscriber with the specified _id_ on _topic_ is not found.
	RemoveSubscriber(id, topic string) error
}

// Subscriber is a callback interface that processes messages received from a `SubscriberSource`.
type Subscriber interface {
	// Subscriber is a  callback that the `SubscriberSource` (or `SubscriberRouter`) calls when a message is received and
	// it is registered in the `Subscriber` interface.
	//
	// When the `Connection` supports re-sends, it can retry if the `Subscriber` returns:
	//
	// - ErrBackoff: backoff in effect (see RetryAfterSeconds) - HTTP 429
	//
	// All other errors, will result in the _payload_ being dropped. When `nil` is returned, the message is considered
	// successfully processed.
	//
	// When the `Connection` do not support re-sends, all errors returned by the `Subscriber` are ignored and
	// dropped.
	Process(ctx context.Context, topic string, payload Message) error
}

// SubscriberConfigSource is a `SubscriberConfigSource` that can accept configuration changes during runtime.
//
// If a `SubscriberSource` do not implement this interface, it means that the `SubscriberSource` is only accepts
// a initial `ConnectionConfig` at creation time (`Connection.Start`).
type SubscriberConfigSource interface {
	// GetSubscriberConfig returns the current configuration of the subscriber.
	GetSubscriberConfig() any
}

// SubscriberAdapter is an adapter to allow the use of ordinary functions as `Subscriber` interfaces.
type SubscriberAdapter func(ctx context.Context, topic string, payload Message) error

// Publish calls the underlying function of the SubscriberAdapter.
func (f SubscriberAdapter) Process(ctx context.Context, topic string, payload Message) error {
	return f(ctx, topic, payload)
}

// ChainableSubscriber is an interface that allows chaining of subscribers to create middleware-like behavior.
type ChainableSubscriber interface {
	Next(ctx context.Context, topic string, payload Message, next ChainableSubscriber) error
}

// SubscriberMiddleware is a function type that defines middleware for the `Subscriber` interface.
type SubscriberMiddleware func(next Subscriber) Subscriber

// ChainSubscriber is a helper to chain multiple `SubscriberMiddleware` into a single `Subscriber` where
// the last one is the actual subscriber.
func ChainSubscriber(s Subscriber, middlewares ...SubscriberMiddleware) Subscriber {
	for i := len(middlewares) - 1; i >= 0; i-- {
		s = middlewares[i](s)
	}

	return s
}
