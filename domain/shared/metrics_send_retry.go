package shared

// In-process send retry metric names. A direct_hold route retries a recoverable
// send inside the bridge, with backoff and the source message still held, until
// its send_retry_budget is spent. Both are tagged route_id and counted only
// while the budget is enabled.
const (
	// MetricSendRetries counts in-process send retries on a direct_hold route:
	// one per retry after a recoverable send failure. A rising value shows
	// destination trouble before any dead-letter alarm fires.
	MetricSendRetries = "SendRetries"
	// MetricSendRetryBudgetExhausted counts held sends whose retries used up
	// send_retry_budget. Each one is then replayed or dead-lettered exactly as
	// it would be without in-process retry.
	MetricSendRetryBudgetExhausted = "SendRetryBudgetExhausted"
)
