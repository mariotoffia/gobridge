package paho

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
)

func nowPlus(secs int) time.Time { return time.Now().Add(time.Duration(secs) * time.Second) }

// ═══════════════════════════════════════════════════════════════════════════
// Errors mapping — bounds and edge-case coverage.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaErr_MapError_Nil returns nil for nil input.
func TestAnaErr_MapError_Nil(t *testing.T) {
	if MapError(nil) != nil {
		t.Fatal("MapError(nil) must be nil")
	}
}

// TestAnaErr_MapError_DeadlineExceeded maps to ErrTimeout.
func TestAnaErr_MapError_DeadlineExceeded(t *testing.T) {
	be := MapError(context.DeadlineExceeded)
	if be == nil || be.Code != domain.ErrTimeout.Code {
		t.Fatalf("DeadlineExceeded → %v, want ErrTimeout", be)
	}
}

// TestAnaErr_MapError_Canceled maps to ErrUnavailable.
func TestAnaErr_MapError_Canceled(t *testing.T) {
	be := MapError(context.Canceled)
	if be == nil || be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("Canceled → %v, want ErrUnavailable", be)
	}
}

// fakeNetErr implements net.Error with controllable Timeout/Temporary.
type fakeNetErr struct {
	msg     string
	timeout bool
}

func (e *fakeNetErr) Error() string   { return e.msg }
func (e *fakeNetErr) Timeout() bool   { return e.timeout }
func (e *fakeNetErr) Temporary() bool { return false }

// TestAnaErr_MapError_NetTimeout maps net.Error with Timeout()=true to ErrTimeout.
func TestAnaErr_MapError_NetTimeout(t *testing.T) {
	be := MapError(&fakeNetErr{msg: "io timeout", timeout: true})
	if be == nil || be.Code != domain.ErrTimeout.Code {
		t.Fatalf("net timeout → %v, want ErrTimeout", be)
	}
}

// TestAnaErr_MapError_NetNonTimeout maps to ErrConnectionLost.
func TestAnaErr_MapError_NetNonTimeout(t *testing.T) {
	be := MapError(&fakeNetErr{msg: "net error", timeout: false})
	if be == nil || be.Code != domain.ErrConnectionLost.Code {
		t.Fatalf("net non-timeout → %v, want ErrConnectionLost", be)
	}
}

// TestAnaErr_MapError_RealNetError uses a real *net.OpError to ensure
// errors.As traversal works across wrapping.
func TestAnaErr_MapError_RealNetError(t *testing.T) {
	op := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	be := MapError(op)
	if be == nil {
		t.Fatal("MapError returned nil for *net.OpError")
	}
	// Either ErrConnectionLost (from net.Error branch) or
	// ErrConnectionLost (from substring match) is acceptable.
	if be.Code != domain.ErrConnectionLost.Code {
		t.Fatalf("real net error → %v, want ErrConnectionLost", be)
	}
}

// TestAnaErr_MapError_BrokerUnavailableSubstring exercises the substring
// match path.
func TestAnaErr_MapError_BrokerUnavailableSubstring(t *testing.T) {
	be := MapError(errors.New("the broker unavailable for now"))
	if be == nil || be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("broker unavailable string → %v, want ErrUnavailable", be)
	}
}

// TestAnaErr_MapError_UnknownErrorIsErrUnavailable verifies the safe
// default classification for unknown errors.
func TestAnaErr_MapError_UnknownErrorIsErrUnavailable(t *testing.T) {
	be := MapError(errors.New("totally unknown failure mode"))
	if be == nil || be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("unknown → %v, want ErrUnavailable", be)
	}
}

// TestAnaErr_MapPublishReasonCode_AllRecoverableQuotaSets verifies all
// known recoverable rate-limit codes map to ErrThrottled.
func TestAnaErr_MapPublishReasonCode_AllRecoverableQuotaSets(t *testing.T) {
	be := MapPublishReasonCode(0x97)
	if be == nil || be.Code != domain.ErrThrottled.Code {
		t.Fatalf("0x97 → %v, want ErrThrottled", be)
	}
}

// TestAnaErr_MapPublishReasonCode_Success_NoMatching_AcceptedAsNil
// verifies that 0x10 (No matching subscribers) is NOT an error.
func TestAnaErr_MapPublishReasonCode_NoMatching_AcceptedAsNil(t *testing.T) {
	if be := MapPublishReasonCode(0x10); be != nil {
		t.Fatalf("0x10 No matching subscribers → %v, want nil (broker accepted)", be)
	}
}

// TestAnaErr_MapPublishReasonCode_NotAuthorized maps to ErrForbidden.
func TestAnaErr_MapPublishReasonCode_NotAuthorized(t *testing.T) {
	be := MapPublishReasonCode(0x87)
	if be == nil || be.Code != domain.ErrForbidden.Code {
		t.Fatalf("0x87 → %v, want ErrForbidden", be)
	}
}

// TestAnaErr_MapPublishReasonCode_UnknownReturnsUnavailable verifies
// the default branch for unknown reason codes.
func TestAnaErr_MapPublishReasonCode_UnknownReturnsUnavailable(t *testing.T) {
	be := MapPublishReasonCode(0xFE)
	if be == nil || be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("0xFE → %v, want ErrUnavailable", be)
	}
}

// TestAnaErr_MapDisconnectReasonCode_KnownCodes spot-checks several
// recoverable and permanent reason code mappings.
func TestAnaErr_MapDisconnectReasonCode_KnownCodes(t *testing.T) {
	cases := []struct {
		code byte
		want domain.ErrorCode
	}{
		{0x00, ""}, // success → nil
		{0x89, domain.ErrBrokerBusy.Code},
		{0x8E, domain.ErrTimeout.Code},
		{0x8F, domain.ErrConnectionLost.Code},
		{0x97, domain.ErrThrottled.Code},
		{0x86, domain.ErrNotAuthorized.Code},
		{0x87, domain.ErrNotAuthorized.Code},
		{0x88, domain.ErrUnavailable.Code},
		{0x91, domain.ErrInvalidTopic.Code},
		{0x95, domain.ErrPayloadTooLarge.Code},
		{0x9C, domain.ErrNotFound.Code},
	}
	for _, tc := range cases {
		be := MapDisconnectReasonCode(tc.code)
		if tc.want == "" {
			if be != nil {
				t.Errorf("0x%02X → %v, want nil", tc.code, be)
			}
			continue
		}
		if be == nil || be.Code != tc.want {
			t.Errorf("0x%02X → %v, want code %s", tc.code, be, tc.want)
		}
	}
}

// TestAnaErr_MapDisconnectReasonCode_Unknown maps to ErrUnavailable.
func TestAnaErr_MapDisconnectReasonCode_Unknown(t *testing.T) {
	be := MapDisconnectReasonCode(0xFE)
	if be == nil || be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("0xFE disconnect → %v, want ErrUnavailable", be)
	}
}

// TestAnaErr_MapSubscribeReasonCode_GrantedQoS verifies all granted
// QoS codes (0x00-0x02) return nil.
func TestAnaErr_MapSubscribeReasonCode_GrantedQoS(t *testing.T) {
	for _, c := range []byte{0x00, 0x01, 0x02} {
		if be := MapSubscribeReasonCode(c); be != nil {
			t.Errorf("granted QoS 0x%02X → %v, want nil", c, be)
		}
	}
}

// TestAnaErr_MapSubscribeReasonCode_TopicFilterInvalid maps to
// ErrInvalidTopic.
func TestAnaErr_MapSubscribeReasonCode_TopicFilterInvalid(t *testing.T) {
	be := MapSubscribeReasonCode(0x8F)
	if be == nil || be.Code != domain.ErrInvalidTopic.Code {
		t.Fatalf("0x8F → %v, want ErrInvalidTopic", be)
	}
}

// TestAnaErr_ContainsAnyEmptySubstrs is a defensive test for the helper
// — passing no substrings should return false.
func TestAnaErr_ContainsAnyEmptySubstrs(t *testing.T) {
	if got := containsAny("anything"); got {
		t.Fatal("containsAny with no substrings should return false")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Headers — additional bounds, edge cases and round-trip invariants.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaHdr_PublishFromEnvelope_NilEnvelopeHeaders_NoCrash validates
// the helper against nil headers (common case).
func TestAnaHdr_PublishFromEnvelope_NilEnvelopeHeaders_NoCrash(t *testing.T) {
	env := &domain.Envelope{Subject: "t", Payload: []byte("p")}
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("PublishFromEnvelope panicked: %v", rv)
		}
	}()
	pub := PublishFromEnvelope(env, SenderOptions{QoS: 0}, nil)
	if pub == nil || pub.Topic != "t" {
		t.Fatalf("expected pub with topic t, got %+v", pub)
	}
}

// TestAnaHdr_PublishFromEnvelope_NonStringHeaderValueIsSkipped pins the
// behaviour: only string-valued user headers are mapped to MQTT user
// properties; non-string values (int, struct, ...) are silently
// dropped.
func TestAnaHdr_PublishFromEnvelope_NonStringHeaderValueIsSkipped(t *testing.T) {
	env := &domain.Envelope{
		Subject: "t",
		Payload: []byte("p"),
		Headers: map[string]any{
			"int-key":    42,
			"struct-key": struct{ A int }{A: 1},
			"good-key":   "ok",
		},
	}
	pub := PublishFromEnvelope(env, SenderOptions{QoS: 1}, nil)

	if pub.Properties == nil {
		t.Fatal("properties should be set when at least one mappable header is present")
	}
	saw := map[string]string{}
	for _, u := range pub.Properties.User {
		saw[u.Key] = u.Value
	}
	if saw["int-key"] != "" {
		t.Error("non-string int-key must NOT be mapped")
	}
	if saw["struct-key"] != "" {
		t.Error("non-string struct-key must NOT be mapped")
	}
	if saw["good-key"] != "ok" {
		t.Errorf("good-key = %q, want %q", saw["good-key"], "ok")
	}
}

// TestAnaHdr_GenerateEnvelopeID_UniqueAcrossManyCalls verifies that
// generated envelope IDs do not collide.
func TestAnaHdr_GenerateEnvelopeID_UniqueAcrossManyCalls(t *testing.T) {
	const n = 1024
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := generateEnvelopeID()
		if len(id) == 0 {
			t.Fatal("empty id generated")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestAnaHdr_EnvelopeFromPublish_PreservesPayload verifies the payload
// reference is forwarded directly (router upstream is responsible for
// the deep-copy).
func TestAnaHdr_EnvelopeFromPublish_PreservesPayload(t *testing.T) {
	pub := &pahov5.Publish{Topic: "t", Payload: []byte("data")}
	env := EnvelopeFromPublish(pub, nil)
	if string(env.Payload) != "data" {
		t.Fatalf("payload = %q, want %q", env.Payload, "data")
	}
}

// TestAnaHdr_EnvelopeFromPublish_OversizedResponseTopic_Dropped verifies
// the size guard for ResponseTopic.
func TestAnaHdr_EnvelopeFromPublish_OversizedResponseTopic_Dropped(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			ResponseTopic: strings.Repeat("a", maxHeaderValueLen+1),
		},
	}
	env := EnvelopeFromPublish(pub, nil)
	if _, ok := env.Headers[headerMQTTResponseTopic]; ok {
		t.Error("oversized response topic must be dropped")
	}
}

// TestAnaHdr_EnvelopeFromPublish_AcceptsEmptyUserPropertyValue verifies
// the printable-ASCII check correctly accepts empty strings (no
// non-printable runes).
func TestAnaHdr_EnvelopeFromPublish_AcceptsEmptyUserPropertyValue(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{{Key: "k", Value: ""}},
		},
	}
	env := EnvelopeFromPublish(pub, nil)
	if v, ok := domain.GetHeaderString(env.Headers, "k"); !ok || v != "" {
		t.Fatalf("empty user property value should be accepted, got ok=%v v=%q", ok, v)
	}
}

// TestAnaHdr_PublishFromEnvelope_EmptyEnvelope_NoProperties verifies
// the publish has nil Properties when no header-derived data exists.
func TestAnaHdr_PublishFromEnvelope_EmptyEnvelope_NoProperties(t *testing.T) {
	env := &domain.Envelope{Subject: "t", Payload: []byte{}}
	pub := PublishFromEnvelope(env, SenderOptions{QoS: 0}, nil)
	if pub.Properties != nil {
		t.Errorf("expected nil Properties for empty envelope, got %+v", pub.Properties)
	}
}

// TestAnaHdr_EnvelopeFromPublish_EmptyTopicAccepted verifies that an
// empty topic on the publish is preserved (do not invent data).
func TestAnaHdr_EnvelopeFromPublish_EmptyTopicAccepted(t *testing.T) {
	pub := &pahov5.Publish{Topic: "", Payload: []byte("p")}
	env := EnvelopeFromPublish(pub, nil)
	if env.Subject != "" {
		t.Errorf("Subject = %q, want empty", env.Subject)
	}
}

// TestAnaHdr_PublishFromEnvelope_OnlyMessageExpiry_HasProperties
// verifies that an envelope with only ExpiresAt produces a publish
// with Properties set (and only MessageExpiry populated).
func TestAnaHdr_PublishFromEnvelope_OnlyMessageExpiry_HasProperties(t *testing.T) {
	env := &domain.Envelope{
		Subject:   "t",
		Payload:   []byte("p"),
		ExpiresAt: nowPlus(60),
	}
	pub := PublishFromEnvelope(env, SenderOptions{QoS: 1}, nil)
	if pub.Properties == nil {
		t.Fatal("expected Properties because ExpiresAt was set")
	}
	if pub.Properties.MessageExpiry == nil {
		t.Fatal("expected MessageExpiry to be populated")
	}
}

// TestAnaHdr_EnvelopeFromPublish_MessageExpiryZeroValue_NoExpiresAt
// verifies that a zero MessageExpiry pointer is still applied (sets
// ExpiresAt to ~now). The pointer presence — not value — is what
// matters; this guards against accidental nil-vs-zero confusion.
func TestAnaHdr_EnvelopeFromPublish_MessageExpiryZeroValue_NoExpiresAt(t *testing.T) {
	expiry := uint32(0)
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			MessageExpiry: &expiry,
		},
	}
	env := EnvelopeFromPublish(pub, nil)
	// An expiry of 0 seconds means "expired immediately"; the header
	// translation just adds 0 → ExpiresAt is set but equals CreatedAt.
	if env.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt must be set when MessageExpiry pointer is non-nil")
	}
	if env.ExpiresAt.Before(env.CreatedAt) {
		t.Fatalf("ExpiresAt %v should not predate CreatedAt %v", env.ExpiresAt, env.CreatedAt)
	}
}
