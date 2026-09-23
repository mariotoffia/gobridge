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
	// MetricSendRetryBudgetExhausted counts held sends whose next wait would not
	// fit inside send_retry_budget. That usually means the retries used the
	// budget up, but it also fires when the budget never covered even the FIRST
	// wait — a destination's RetryAfter hint longer than the budget, or a budget
	// shorter than the first backoff wait, which is never under 100 ms — because
	// the send did not fit the budget either way. Each one is then replayed or
	// dead-lettered exactly as it would be without in-process retry.
	MetricSendRetryBudgetExhausted = "SendRetryBudgetExhausted"
)
