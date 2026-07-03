package transport

// Metric names emitted by the HTTP transport adapter (ingress, cluster
// forwarding, SSE). Relocated from domain/shared as part of shared-kernel
// slimming; the string values are the wire identities reported to
// CloudWatch/OTel and MUST NOT change.
const (
	MetricHTTPIngressLatency  = "HTTPIngressLatency"
	MetricHTTPForwardLatency  = "HTTPForwardLatency"
	MetricSSEClients          = "SSEClients"
	MetricSSEBroadcastLatency = "SSEBroadcastLatency"
	MetricClusterForwards     = "ClusterForwards"
	MetricSSEDroppedEvents    = "SSEDroppedEvents"
	// MetricHTTPForwardLoopRefused counts requests refused because they
	// already carried an X-Bridge-Forwarded marker yet resolved to a remote
	// route — a routing-disagreement loop that this node breaks instead of
	// re-forwarding.
	MetricHTTPForwardLoopRefused = "HTTPForwardLoopRefused"
	// MetricSSEDeadlineUnsupported counts SSE streams whose ResponseWriter
	// chain does not support per-write deadlines (http.ResponseController
	// could not set one). While non-zero, slow-client eviction is inert for
	// those streams and a stalled reader can pin a goroutine — alert on it.
	MetricSSEDeadlineUnsupported = "SSEDeadlineUnsupported"
	// MetricSSENoSubscribers counts Send calls that completed with ZERO
	// connected SSE clients. SSE egress is at-most-once: the send still
	// reports success (so the source is acked) but the event reached
	// nobody. Alert on it when subscribers are expected to be attached.
	MetricSSENoSubscribers = "SSENoSubscribers"
	// MetricSSEAllDropped counts Send calls where EVERY connected client's
	// buffer was full, so the event was dropped for 100% of subscribers.
	// Like the no-subscriber case the send still reports success — this
	// counter is the only signal that the broadcast delivered to nobody.
	MetricSSEAllDropped = "SSEAllDropped"
	// MetricHTTPIngressDuplicates counts ingress requests short-circuited
	// by the receiver's bounded idempotency window: the presented
	// Idempotency-Key / X-Dedup-Id was already processed successfully, so
	// the request is acknowledged without re-emitting the delivery.
	MetricHTTPIngressDuplicates = "HTTPIngressDuplicates"
	// MetricHTTPForwardBreakerOpen counts cluster forwards rejected by the
	// forwarder's circuit breaker without any network attempt (the peer is
	// considered down; the breaker is cooling down).
	MetricHTTPForwardBreakerOpen = "HTTPForwardBreakerOpen"
)
