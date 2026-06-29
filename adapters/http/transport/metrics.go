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
)
