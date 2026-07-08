package sqs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// newSenderForTest builds a Sender with a valid queue URL and the given
// options for direct buildAttributes exercises.
func newSenderForTest(t *testing.T, opts ...SenderOption) *Sender {
	t.Helper()
	s, err := NewSender(SenderConfig{QueueURL: "https://sqs.test/q"}, opts...)
	require.NoError(t, err)
	return s
}

// TestBuildAttributes_SubjectBytesChargedBeforeSelection is the regression for
// Finding 4(b). The Subject attribute is written AFTER the size-budget loop,
// so its bytes must be pre-charged into the size accumulator BEFORE header
// selection — otherwise a request just under the ceiling can be pushed over
// the real broker limit by the un-counted Subject bytes.
//
// A small configurable ceiling makes the arithmetic explicit: body(80) +
// Subject(16) leaves no room for the 9-byte header, but the identical body
// without a Subject does.
func TestBuildAttributes_SubjectBytesChargedBeforeSelection(t *testing.T) {
	t.Parallel()

	const ceiling = 100
	body := make([]byte, 80)
	s := newSenderForTest(t, WithMaxMessageBytes(ceiling))

	// With a Subject its ~16 bytes are charged against the shared ceiling
	// before selection, so the 9-byte header no longer fits and is dropped.
	withSub := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "s1",
		Subject: "sub",
		Payload: body,
		Headers: map[string]any{"h": "vv"},
	})
	attrsWith := s.buildAttributes(withSub)
	_, hasHeaderWith := attrsWith["h"]
	assert.False(t, hasHeaderWith,
		"header must drop once Subject bytes are charged against the ceiling (Finding 4b)")
	_, hasSubject := attrsWith[sqsSubjectAttributeName]
	assert.True(t, hasSubject, "the reserved Subject attribute is always written")

	// Without a Subject the identical body+header fits, proving it was the
	// Subject bytes (not the body alone) that forced the drop above.
	noSub := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "s2",
		Payload: body,
		Headers: map[string]any{"h": "vv"},
	})
	attrsNo := s.buildAttributes(noSub)
	_, hasHeaderNo := attrsNo["h"]
	assert.True(t, hasHeaderNo, "the same header fits when no Subject consumes the ceiling")
}

// TestWithMaxMessageBytes_OversizedBodyKeepsRank0AttributeAtRaisedCeiling is
// the regression for Finding 4(a). On a queue configured with a larger
// MaximumMessageSize, a body over the stale hardcoded 256 KiB ceiling dropped
// ALL attributes — including the rank-0 idempotency key — while the send still
// succeeded, silently losing bridge identity. Making the ceiling configurable
// via WithMaxMessageBytes lets an operator fit the real queue limit and keep
// the attribute.
func TestWithMaxMessageBytes_OversizedBodyKeepsRank0AttributeAtRaisedCeiling(t *testing.T) {
	t.Parallel()

	body := make([]byte, sqsMaxMessageBytes+1000) // > default 256 KiB ceiling
	newEnv := func() *messaging.Envelope {
		return messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
			ID:      "big",
			Payload: body,
			Headers: map[string]any{messaging.HeaderIdempotencyKey: "idem-123"},
		})
	}

	// Default ceiling: the oversized body consumes the whole budget, so even
	// the rank-0 idempotency-key attribute is dropped.
	sDefault := newSenderForTest(t)
	_, keptDefault := sDefault.buildAttributes(newEnv())[messaging.HeaderIdempotencyKey]
	assert.False(t, keptDefault,
		"at the default 256 KiB ceiling an oversized body drops the rank-0 idempotency key (Finding 4a)")

	// Raised ceiling (fits body + attribute): the identity attribute survives.
	sRaised := newSenderForTest(t, WithMaxMessageBytes(len(body)+4096))
	_, keptRaised := sRaised.buildAttributes(newEnv())[messaging.HeaderIdempotencyKey]
	assert.True(t, keptRaised,
		"raising the ceiling to the queue's real limit keeps the idempotency key for an oversized body")
}

// TestBuildAttributes_RelaySubjectDoesNotDropRealHeader is the regression for
// Finding 7. A SQS->SQS relay keeps a plain "Subject" header (ingress) AND the
// sender reserves a Subject slot from env.Subject(). The stray "Subject" header
// must NOT also compete for one of the 10 attribute slots — otherwise a relay
// carrying >=10 application headers drops a real header for a duplicate that
// the reserved write overwrites anyway.
func TestBuildAttributes_RelaySubjectDoesNotDropRealHeader(t *testing.T) {
	t.Parallel()

	s := newSenderForTest(t)

	headers := map[string]any{"Subject": "evt"} // leftover header from ingress
	for i := 0; i < 10; i++ {
		headers[fmt.Sprintf("app-%02d", i)] = "v"
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "relay",
		Subject: "evt",
		Payload: []byte("b"),
		Headers: headers,
	})

	attrs := s.buildAttributes(env)

	// 1 reserved Subject + 9 real application headers = the full cap; the
	// "Subject" header consumed no slot of its own.
	require.Len(t, attrs, sqsMaxMessageAttributes)
	_, hasSubject := attrs[sqsSubjectAttributeName]
	assert.True(t, hasSubject, "the reserved Subject attribute must be present")

	appCount := 0
	for k := range attrs {
		if strings.HasPrefix(k, "app-") {
			appCount++
		}
	}
	assert.Equal(t, sqsMaxMessageAttributes-1, appCount,
		"all non-Subject slots must carry real application headers, none wasted on a duplicate Subject (Finding 7)")
}
