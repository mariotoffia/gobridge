package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// captureAudit records every audit event emitted by the server. The
// admin Inject handler must stamp envelope_id into both success and
// failure paths so operators can correlate an injected message with
// downstream delivery / DLQ records.
type captureAudit struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (c *captureAudit) Log(_ context.Context, ev ports.AuditEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *captureAudit) last() ports.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return ports.AuditEvent{}
	}
	return c.events[len(c.events)-1]
}

func newInjectTestServer(t *testing.T, opts ...Option) (*Server, *stubSender, *captureAudit) {
	t.Helper()
	rt, sender := injectRuntime(t)
	audit := &captureAudit{}
	all := append([]Option{WithAuditLogger(audit)}, opts...)
	s := New(rt, testConfig(), all...)
	return s, sender, audit
}

func newInjectReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	req.SetPathValue("routeID", "test-route")
	return req
}

// TestInjectEnvelope_NoID_GeneratesNonSentinel verifies that omitting
// the "id" field triggers server-side generation. The generated ID
// MUST NOT collapse to the historical "test-envelope" sentinel that
// hid the root regression — every call gets a unique value.
func TestInjectEnvelope_NoID_GeneratesNonSentinel(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 5; i++ {
		s, _, audit := newInjectTestServer(t)
		rec := httptest.NewRecorder()
		s.handleInject(rec, newInjectReq(`{"subject":"s"}`))

		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var resp map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		envID := resp["envelope_id"]
		require.NotEmpty(t, envID, "envelope_id must be returned")
		require.NotEqual(t, "test-envelope", envID, "must not regress to literal sentinel")
		require.NotContains(t, seen, envID, "generator must produce unique IDs")
		seen[envID] = struct{}{}

		assert.Equal(t, envID, audit.last().Detail["envelope_id"])
	}
}

// TestInjectEnvelope_CreatedAtFromClock pins NewEnvelope to s.clk. The
// fake clock's value MUST appear on the envelope routed to the sender.
func TestInjectEnvelope_CreatedAtFromClock(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, sender, _ := newInjectTestServer(t, WithClock(clocktest.NewAt(fixed)))

	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"id":"clock-1","subject":"s"}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	require.Eventually(t, func() bool { return sender.sentCount() >= 1 },
		2*time.Second, 10*time.Millisecond)
	sender.mu.Lock()
	got := sender.sent[0].CreatedAt()
	sender.mu.Unlock()
	assert.Equal(t, fixed, got, "CreatedAt must come from the injected clock")
}

// TestInjectEnvelope_ReservedHeadersStripped verifies the constructor
// chokepoint: caller-supplied x-bridge.* headers MUST be removed
// because the bridge owns that namespace.
func TestInjectEnvelope_ReservedHeadersStripped(t *testing.T) {
	s, sender, _ := newInjectTestServer(t)
	body := `{"id":"hdr-1","subject":"s","headers":{"x-bridge.route-id":"forged","tenant":"acme"}}`

	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	require.Eventually(t, func() bool { return sender.sentCount() >= 1 },
		2*time.Second, 10*time.Millisecond)
	sender.mu.Lock()
	headers := sender.sent[0].Headers()
	sender.mu.Unlock()
	got, _ := headers[messaging.HeaderRouteID].(string)
	assert.NotEqual(t, "forged", got, "caller-supplied reserved header must be stripped before runtime stamping")
	assert.Equal(t, "acme", headers["tenant"], "non-reserved header preserved")
}

// TestInjectEnvelope_CallerIDPreserved verifies that an explicit
// non-empty "id" is honoured.
func TestInjectEnvelope_CallerIDPreserved(t *testing.T) {
	s, _, audit := newInjectTestServer(t)
	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"id":"caller-supplied-9","subject":"s"}`))

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "caller-supplied-9", resp["envelope_id"])
	assert.Equal(t, "caller-supplied-9", audit.last().Detail["envelope_id"])
}

// TestInjectEnvelope_ExplicitEmptyID_Returns400 pins the "explicit
// empty string" contract: the server MUST NOT silently substitute a
// generated ID — that hides client bugs and the server-side guarantee
// is "send id=null/omit to generate, or send a real id".
func TestInjectEnvelope_ExplicitEmptyID_Returns400(t *testing.T) {
	s, _, audit := newInjectTestServer(t)
	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"id":"","subject":"s"}`))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp["error"], "envelope id")
	assert.Equal(t, string(shared.ErrCodeInvalidPayload), audit.last().Detail["error"])
}

// TestInjectEnvelope_AuditLogsEnvelopeID guards the audit-trail
// requirement: every outcome (success, route-not-found, internal
// failure) MUST emit envelope_id.
func TestInjectEnvelope_AuditLogsEnvelopeID(t *testing.T) {
	s, _, audit := newInjectTestServer(t)

	// success
	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"id":"audit-ok","subject":"s"}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "audit-ok", audit.last().Detail["envelope_id"])
	require.Equal(t, "success", audit.last().Outcome)

	// not-found
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/missing/inject",
		strings.NewReader(`{"id":"audit-nf","subject":"s"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	req.SetPathValue("routeID", "missing")
	rec = httptest.NewRecorder()
	s.handleInject(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "audit-nf", audit.last().Detail["envelope_id"])
	require.Equal(t, "failure", audit.last().Outcome)
}

// TestInjectEnvelope_StaticGuard_ProducesUUIDLike pins the default
// generator's output shape so nobody silently swaps in an unsafe
// short-ID generator. The contract is "32 hex characters" (matches
// the AMQP/SQS adapters).
func TestInjectEnvelope_StaticGuard_ProducesUUIDLike(t *testing.T) {
	id := defaultIDGen()
	require.Len(t, id, 32, "default ID generator must produce 32 hex chars")
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		require.True(t, isHex, "default ID generator must be hex; got %q", id)
	}
	id2 := defaultIDGen()
	require.NotEqual(t, id, id2, "default ID generator must be non-deterministic")
}
