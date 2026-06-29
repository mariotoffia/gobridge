package messaging

import (
	"encoding/json"
	"fmt"
	"time"
)

// envelopeJSON is the on-the-wire shape used by MarshalJSON /
// UnmarshalJSON. The schema must remain stable across releases because
// adapters (sqlitedlq, dynamodbdlq, etc.) serialise Envelope payloads
// to durable storage. Keys mirror the historical exported field names
// to keep persisted records readable across the un-export change.
type envelopeJSON struct {
	ID        string         `json:"ID,omitempty"`
	Subject   string         `json:"Subject,omitempty"`
	Payload   []byte         `json:"Payload,omitempty"`
	Headers   map[string]any `json:"Headers,omitempty"`
	CreatedAt time.Time      `json:"CreatedAt,omitempty"`
	ExpiresAt time.Time      `json:"ExpiresAt,omitempty"`
}

// MarshalJSON serialises the envelope using the stable historical JSON
// schema (capitalised field names matching the pre-un-export struct).
// This is what storage adapters wrote before the un-export change;
// keeping the keys identical preserves on-disk compatibility.
//
// ARCHITECTURAL EXCEPTION (M-10): this marshaller is the SINGLE SOURCE
// OF TRUTH for the durable Envelope wire format. Every store adapter
// (sqlitedlq, dynamodbdlq, sqliteoutbox, dynamodboutbox, …) persists
// envelopes through it, so the schema lives here, in the domain, on
// purpose -- scattering it across adapters would invite silent
// cross-backend schema drift. The encoding/json dependency this
// requires is a sanctioned, documented exception to domain/messaging's
// stdlib-only rule (see .go-arch-lint.yml and LINT.md), not an oversight.
func (e Envelope) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(envelopeJSON{
		ID:        e.id,
		Subject:   e.subject,
		Payload:   e.payload,
		Headers:   e.headers,
		CreatedAt: e.createdAt,
		ExpiresAt: e.expiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: marshal envelope: %w", err)
	}
	return b, nil
}

// UnmarshalJSON populates the envelope from the stable JSON schema.
// Note: this is a rehydration path, not a fresh construction — it
// deliberately bypasses NewEnvelope's reserved-header strip so that
// previously-stamped reserved headers (correlation id, route id, …)
// survive a save/load cycle.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	var dto envelopeJSON
	if err := json.Unmarshal(data, &dto); err != nil {
		return fmt.Errorf("messaging: unmarshal envelope: %w", err)
	}
	e.id = dto.ID
	e.subject = dto.Subject
	e.payload = clonePayload(dto.Payload)
	// Rehydration path: bypass reserved-header strip so previously-
	// stamped bridge metadata survives a save/load round-trip.
	e.headers = headersFromTrustedMap(dto.Headers)
	e.createdAt = dto.CreatedAt
	e.expiresAt = dto.ExpiresAt
	return nil
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
