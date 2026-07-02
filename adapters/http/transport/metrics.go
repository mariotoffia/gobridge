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
)
