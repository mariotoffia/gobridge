package shared

// Broker state metric names (ADR 0024). A live reload that removes a durable
// subscription, or changes its broker identity, ends the state the broker keeps
// for the old identity: an MQTT persistent or exclusive session, or an AMQP 1.0
// durable topic subscription.
const (
	// MetricBrokerStateEndFailures counts the times GoBridge could not end that
	// state, tagged session_id with the configured session id: the broker
	// refused it (access denied), could not be reached within the transport's
	// connect timeout, or rejected the closing detach. The reload goes on, and
	// the state stays on the broker: an MQTT session until it expires, an AMQP
	// 1.0 durable subscription until an operator deletes it.
	MetricBrokerStateEndFailures = "BrokerStateEndFailures"
)
