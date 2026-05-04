package runtime

import (
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// retryDelay returns the next retry delay for a recoverable send
// error: the broker-provided RetryAfter when present, otherwise an
// exponentially-backed-off interval derived from policy.Backoff,
// capped by Backoff.MaxInterval.
func retryDelay(policy domain.RoutePolicy, attempt int, sendErr error) time.Duration {
	if d := domain.GetRetryAfter(sendErr); d > 0 {
		return d
	}
	bp := policy.Backoff
	if bp.InitialInterval <= 0 || bp.Multiplier <= 0 {
		bp = domain.NewDefaultBackoffPolicy()
	}
	if attempt < 1 {
		attempt = 1
	}
	d := float64(bp.InitialInterval)
	for i := 1; i < attempt; i++ {
		d *= bp.Multiplier
		if bp.MaxInterval > 0 && time.Duration(d) >= bp.MaxInterval {
			return bp.MaxInterval
		}
	}
	return time.Duration(d)
}
