package domain

import "time"

// Envelope is the normalized message being moved through the bridge.
type Envelope struct {
	ID        string
	Subject   string
	Payload   []byte
	Headers   map[string]any
	CreatedAt time.Time
	ExpiresAt time.Time
}

// HasExpiry returns true if the envelope has a non-zero expiry timestamp.
func (e *Envelope) HasExpiry() bool {
	return !e.ExpiresAt.IsZero()
}

// IsExpired returns true if the envelope has expired according to clk.
func (e *Envelope) IsExpired(clk interface{ Now() time.Time }) bool {
	return e.HasExpiry() && clk.Now().After(e.ExpiresAt)
}

// RemainingTTL returns the time remaining before expiry according to clk.
// Returns 0 if the envelope has no expiry or is already expired.
func (e *Envelope) RemainingTTL(clk interface{ Now() time.Time }) time.Duration {
	if !e.HasExpiry() {
		return 0
	}
	now := clk.Now()
	if !e.ExpiresAt.After(now) {
		return 0
	}
	return e.ExpiresAt.Sub(now)
}

// Clone returns a deep copy of the envelope, including a recursively
// cloned Headers map so reference-type values (slices, maps) are not
// shared between original and clone.
func (e *Envelope) Clone() *Envelope {
	c := *e
	if e.Payload != nil {
		c.Payload = make([]byte, len(e.Payload))
		copy(c.Payload, e.Payload)
	}
	if e.Headers != nil {
		c.Headers = deepCopyHeaders(e.Headers)
	}
	return &c
}

func deepCopyHeaders(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = deepCopyValue(v)
	}
	return cp
}

func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return deepCopyHeaders(val)
	case map[string]string:
		cp := make(map[string]string, len(val))
		for k, v := range val {
			cp[k] = v
		}
		return cp
	case []any:
		s := make([]any, len(val))
		for i, elem := range val {
			s[i] = deepCopyValue(elem)
		}
		return s
	case []string:
		s := make([]string, len(val))
		copy(s, val)
		return s
	case []byte:
		if val == nil {
			return val
		}
		s := make([]byte, len(val))
		copy(s, val)
		return s
	case []int:
		s := make([]int, len(val))
		copy(s, val)
		return s
	case []int64:
		s := make([]int64, len(val))
		copy(s, val)
		return s
	case []float64:
		s := make([]float64, len(val))
		copy(s, val)
		return s
	case []float32:
		s := make([]float32, len(val))
		copy(s, val)
		return s
	default:
		return v
	}
}

// OutboxStatus represents the state of an outbox record in the state machine.
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxClaimed   OutboxStatus = "claimed"
	OutboxCompleted OutboxStatus = "completed"
	OutboxExpired   OutboxStatus = "expired"
)

// OutboxPartitionKey computes the outbox partition key from a record's
// session or binding identity. This is the canonical key used by
// OutboxStore.Persist, Claim, and QueryPending.
func OutboxPartitionKey(sessionID, bindingID string) string {
	if sessionID != "" {
		return "SESSION#" + sessionID
	}
	return "BINDING#" + bindingID
}

// OutboxRecord represents a durable outbox entry for reliable egress.
type OutboxRecord struct {
	ID              string
	RouteID         string
	EnvelopeID      string
	BindingID       string
	SessionID       string
	Address         string
	Envelope        Envelope
	DispatchHeaders map[string]any
	Status          OutboxStatus
	ClaimedBy       string
	ClaimedAt       time.Time
	ClaimVersion    uint64
	ReplayCount     int
	CreatedAt       time.Time
	ExpiresAt       time.Time
	CompletedAt     time.Time
}
