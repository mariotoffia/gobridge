package types

import "context"

// TopicSubscriptionConfig is a configuration for a topic subscription.
type TopicSubscriptionConfig struct {
	// Custom is optional parameters for the subscription.
	Custom map[string]any
}

// TopicSubscriber is a function callback that the `BridgeServer` calls when a message is received and
// it is registered in the `TopicSubscriber` interface.
//
// When the `BridgeServer` supports re-sends, it can retry if the `TopicSubscriber` returns:
//
// - ErrBackoff: backoff in effect (see RetryAfterSeconds) - HTTP 429
//
// All other errors, will result in the _payload_ being dropped. When `nil` is returned, the message is considered
// successfully processed.
//
// When the `BridgeServer` do not support re-sends, all errors returned by the `TopicSubscriber` are ignored and
// dropped.
type TopicSubscriber func(ctx context.Context, topic string, payload Message) error

// TopicSubscriberManager specifies that the `BridgeServer` can manage subscribers and
// call them when messages are received.
type TopicSubscriberManager interface {
	// AddTopicSubscriber registers a consumer callback for the specified topics.
	//
	// NOTE: The topics can be concrete topic string or contain wildcards, depending on the
	// server capabilities.
	//
	// It returns an error if the subscription could not be created.
	//
	// A _topic_ may have multiple subscribers registered, if a already registered _callback_ on _topic_
	// it is overwritten.
	//
	// The optional _config_ can be used to provide additional subscription configuration when registering
	// the subscriber. Depending on the `BridgeServer` implementation, it may ignore the configuration.
	AddTopicSubscriber(topic string, callback TopicSubscriber, config ...TopicSubscriptionConfig) error
	// RemoveTopicSubscriber removes a previously registered consumer callback for the specified topic.
	//
	// It returns an error if the un-subscribe could not be performed.
	//
	// It will use the _callback_ pointer to identify the correct subscription to remove. If not found,
	// it returns a `ErrNotFound` (404) error.
	RemoveTopicSubscriber(topic string, callback TopicSubscriber) error
}

// ChainableSubscriber is an interface that allows chaining of subscribers to create middleware-like behavior.
type ChainableSubscriber interface {
	Next(ctx context.Context, topic string, payload Message, next ChainableSubscriber) error
}

// SubscriberMiddleware is a function type that defines middleware for the `TopicSubscriber` interface.
type SubscriberMiddleware func(next TopicSubscriber) TopicSubscriber

// ChainSubscriber is a helper to chain multiple `SubscriberMiddleware` into a single `TopicSubscriber` where
// the last one is the actual subscriber.
func ChainSubscriber(s TopicSubscriber, middlewares ...SubscriberMiddleware) TopicSubscriber {
	for i := len(middlewares) - 1; i >= 0; i-- {
		s = middlewares[i](s)
	}

	return s
}
