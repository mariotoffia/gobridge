package amqp10

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/Azure/go-amqp"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// --- SASL EXTERNAL cert-material check is deferred while a
// credentials_uri is pending and re-run post-resolution in
// Config.ApplyCredentials, so a URI-supplied client certificate is no
// longer wrongly rejected at parse time (the URI path). ---

func TestConfig_ExternalSASL_DeferredCredentials(t *testing.T) {
	t.Run("inline cert passes parse-time validate", func(t *testing.T) {
		c := &Config{Session: SessionOptions{
			Address:       "amqps://broker:5671",
			SASLMechanism: "external",
			TLS:           &TLSConfig{Enable: true, CertFile: "/c.pem", KeyFile: "/k.pem"},
		}}
		require.NoError(t, c.Validate())
	})

	t.Run("deferred: validate passes, ApplyCredentials with cert succeeds", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqps://broker:5671", SASLMechanism: "external"},
			CredentialsURIRef: "vault://amqp/mtls",
		}
		// Parse-time: the pending credentials_uri defers the cert check.
		require.NoError(t, c.Validate())
		// Resolution supplies the client key-pair -> re-check passes.
		set := connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", nil, false))
		require.NoError(t, c.ApplyCredentials(set))
		require.Empty(t, c.CredentialsURIRef)
		require.True(t, hasClientCertMaterial(c.Session.TLS))
	})

	t.Run("deferred: ApplyCredentials without cert restores teeth", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqps://broker:5671", SASLMechanism: "external"},
			CredentialsURIRef: "vault://amqp/mtls",
		}
		require.NoError(t, c.Validate())
		// Resolution supplies only a password -> no client certificate.
		set := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
		err := c.ApplyCredentials(set)
		require.Error(t, err)
		be, ok := shared.AsBridgeError(err)
		require.True(t, ok)
		require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
		require.Contains(t, err.Error(), "client certificate material")
	})

	t.Run("counterfactual: strict validation still rejects cert-less EXTERNAL", func(t *testing.T) {
		// validate(false) is the strict path the un-threaded code always
		// took; without threading, case (b) above would FAIL parse-time.
		strict := SessionOptions{Address: "amqps://broker:5671", SASLMechanism: "external"}
		require.Error(t, strict.validate(false),
			"strict validation must reject cert-less EXTERNAL (pre-FIX behavior)")
		require.NoError(t, strict.validate(true),
			"threading credentialsPending=true is what defers the check")
	})
}

// --- application-property values are rendered to domain-safe
// stdlib types so no go-amqp SDK type crosses the ACL into envelope
// headers (the application-property path). ---

func TestMessageToHeaders_AppPropertiesRenderStdlib(t *testing.T) {
	uuid := amqp.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	bin := []byte{0xde, 0xad, 0xbe, 0xef}
	msg := &amqp.Message{ApplicationProperties: map[string]any{
		"uuid": uuid,
		"sym":  amqp.Symbol("a-symbol"),
		"bin":  bin,
		"str":  "plain",
		"num":  42,
		// Reserved + module-prefixed keys must still be filtered out.
		messaging.HeaderCorrelationID: "should-not-appear",
		headerPrefix + "internal":     "should-not-appear",
	}}

	h := messageToHeaders(msg)

	// ACL purity: no go-amqp SDK type or raw []byte may reach domain headers.
	for k, v := range h {
		switch v.(type) {
		case amqp.UUID, amqp.Symbol, []byte:
			t.Fatalf("header %q leaked SDK/binary type %T into domain headers", k, v)
		}
	}
	require.Equal(t, uuid.String(), h["uuid"])
	require.Equal(t, "a-symbol", h["sym"])
	require.Equal(t, "deadbeef", h["bin"])
	require.Equal(t, "plain", h["str"])
	require.Equal(t, 42, h["num"])

	// The reserved-header skip still holds after rendering.
	_, hasReserved := h[messaging.HeaderCorrelationID]
	require.False(t, hasReserved, "reserved headers must still be filtered")
	_, hasPrefixed := h[headerPrefix+"internal"]
	require.False(t, hasPrefixed, "amqp10.* headers must still be filtered")
}

// --- a context.Canceled settlement cause is a deliberate route
// teardown, not a broker-health signal, so it must not increment the
// failure counter, emit the failure metric, or trigger a rebuild. ---

func TestReceiver_SettlementFailed_ContextCanceled_NoRebuild(t *testing.T) {
	rec := &ports.RecordingExporter{}
	// LinkCredit 2 -> threshold 1: absent the guard the FIRST
	// canceled settlement would already trip a rebuild.
	r, err := NewReceiver(ReceiverConfig{
		Address:    "queue/cancel",
		LinkCredit: 2,
		Metrics:    rec,
	}, nil) // nil session: a rebuild would close the link directly (observable)
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	settler := newMockSettler()
	settler.acceptErr = context.Canceled // deliberate teardown/reconfig

	for i := 0; i < 5; i++ { // well past the threshold
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m", Subject: "t", Payload: []byte("d")})
		del := NewDelivery(env, &amqp.Message{}, settler, nil, rec, nil)
		r.trackDelivery(del)
		_ = del.Ack(context.Background())
	}

	require.Equal(t, 0, fl.closeCalls,
		"context.Canceled settlements are deliberate teardown, never a rebuild trigger")
	r.settleFailMu.Lock()
	got := r.settleFailures
	r.settleFailMu.Unlock()
	require.Equal(t, 0, got, "canceled settlements must not increment the failure counter")
	require.Empty(t, rec.FindEntries(MetricAMQP10SettleFailed),
		"a deliberate teardown is not a health signal and emits no failure metric")
}

// --- a stale in-flight settlement from a superseded link
// generation must not be counted against the freshly rebuilt link
// (which would trip a spurious second rebuild). ---

func TestReceiver_SettlementFailed_StaleGeneration_NoIncrement(t *testing.T) {
	rec := &ports.RecordingExporter{}
	// LinkCredit 2 -> threshold 1: the first COUNTED failure trips a rebuild.
	r, err := NewReceiver(ReceiverConfig{
		Address:    "queue/stale",
		LinkCredit: 2,
		Metrics:    rec,
	}, nil)
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	settler := newMockSettler()
	settler.acceptErr = errors.New("settle boom")

	// Track a delivery against the CURRENT (generation 0) link.
	stale := NewDelivery(
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t", Payload: []byte("d")}),
		&amqp.Message{}, settler, nil, rec, nil)
	r.trackDelivery(stale) // captures generation 0

	// Simulate a link rebuild (createLink bumps the generation under lock).
	r.settleFailMu.Lock()
	r.linkGeneration++
	r.settleFailMu.Unlock()

	// The stale in-flight delivery now settles (failure) AFTER the rebuild.
	_ = stale.Ack(context.Background())

	require.Equal(t, 0, fl.closeCalls,
		"a stale-generation settlement must not trigger a rebuild on the new link")
	r.settleFailMu.Lock()
	got := r.settleFailures
	r.settleFailMu.Unlock()
	require.Equal(t, 0, got, "a stale-generation settlement must not increment the current counter")
	require.Len(t, rec.FindEntries(MetricAMQP10SettleFailed), 1,
		"a stale settle failure stays observable via the metric")

	// Counterfactual: a delivery tracked on the CURRENT generation still
	// counts and trips the rebuild — proving only stale ones are ignored.
	live := NewDelivery(
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m2", Subject: "t", Payload: []byte("d")}),
		&amqp.Message{}, settler, nil, rec, nil)
	r.trackDelivery(live) // captures the current generation
	_ = live.Ack(context.Background())
	require.Equal(t, 1, fl.closeCalls,
		"a current-generation settlement failure still forces the rebuild")
}

// --- the group-sequence header accepts the numeric carriers a
// header can arrive as (notably float64 from JSON), bounded to a valid
// uint32, instead of silently dropping non-int values. ---

func TestHeadersToMessage_GroupSequence_BroadNumeric(t *testing.T) {
	// float64 (the JSON integer carrier) must be applied, not dropped.
	msg := headersToMessage(map[string]any{headerGroupSequence: float64(1234)})
	require.NotNil(t, msg.Properties)
	require.NotNil(t, msg.Properties.GroupSequence)
	require.Equal(t, uint32(1234), *msg.Properties.GroupSequence)

	// int64 / uint / uint64 carriers are accepted too.
	for _, v := range []any{int64(7), uint(8), uint64(9)} {
		m := headersToMessage(map[string]any{headerGroupSequence: v})
		require.NotNil(t, m.Properties, "value %T must yield properties", v)
		require.NotNil(t, m.Properties.GroupSequence, "value %T must be applied", v)
	}

	// A fractional float64 is dropped (never truncated to a wrong sequence).
	frac := headersToMessage(map[string]any{headerGroupSequence: float64(3.5)})
	if frac.Properties != nil && frac.Properties.GroupSequence != nil {
		t.Fatalf("a fractional group-sequence must be dropped, got %d", *frac.Properties.GroupSequence)
	}

	// An out-of-range float64 (> MaxUint32) is dropped.
	over := headersToMessage(map[string]any{headerGroupSequence: float64(math.MaxUint32) + 1})
	if over.Properties != nil && over.Properties.GroupSequence != nil {
		t.Fatalf("an out-of-range group-sequence must be dropped, got %d", *over.Properties.GroupSequence)
	}
}
