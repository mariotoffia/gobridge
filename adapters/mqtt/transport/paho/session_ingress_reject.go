package paho

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// rejectPredecodeIngress records a packet the pre-decode guard refused. The
// guard sends DISCONNECT and closes the socket after this returns, so Paho's
// reader fails, OnConnectionDown fires and autopaho reconnects. The packet
// never reached a route, so nothing of the old connection can act on it.
// It runs on Paho's read goroutine and must return promptly.
func (s *Session) rejectPredecodeIngress(cause error) {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	if s.ingressRejectSettledLocked(now) {
		s.ingressRejectStreak = 0
	}
	s.ingressRejectStreak++
	s.lastIngressRejectAt = now
	s.ingressRejectErr = cause
	streak := s.ingressRejectStreak
	s.mu.Unlock()
	s.metrics.Counter(MetricMQTTIngressRejected, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if s.logger != nil {
		s.logger.Error("mqtt: rejected inbound packet before Paho decoding; dropping the connection",
			"client_id", s.opts.ClientID, "error", cause, "streak", streak)
	}
}

// ingressRejectSettledLocked reports whether the current connection came up
// after the last pre-decode reject and has stayed up for
// connectionStabilityWindow. A connection that is down, or one that came up
// before the reject, settles nothing. Callers hold s.mu.
func (s *Session) ingressRejectSettledLocked(now int64) bool {
	return s.connected && s.connUpAt > s.lastIngressRejectAt && now-s.connUpAt >= int64(connectionStabilityWindow)
}

// clearSettledIngressRejectLocked forgets a pre-decode reject once a
// replacement connection has proven stable. Every path that ends an
// established connection calls it before clearing s.connected. Callers hold
// s.mu.
func (s *Session) clearSettledIngressRejectLocked(now int64) {
	if s.ingressRejectErr != nil && s.ingressRejectSettledLocked(now) {
		s.ingressRejectErr = nil
		s.ingressRejectStreak = 0
		s.lastIngressRejectAt = 0
	}
}

// ingressRejectPenalty returns the extra reconnect delay while pre-decode
// rejects are still arriving: the reconnect backoff for the current streak, or
// 0 once no reject has occurred for connectionStabilityWindow. autopaho
// restarts at attempt 0, which has no delay, after a connection that came up,
// so without this a broker that re-sends the packet on every resume would be
// redialled in a tight loop.
func (s *Session) ingressRejectPenalty(base, maxDelay time.Duration, randFloat func() float64) time.Duration {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	streak := s.ingressRejectStreak
	last := s.lastIngressRejectAt
	s.mu.Unlock()
	if streak == 0 || last == 0 || now-last >= int64(connectionStabilityWindow) {
		return 0
	}
	return reconnectBackoff(streak, base, maxDelay, reconnectBackoffFactor, randFloat)
}
