package paho

// Regression pins for the PROD_READY_ISSUES MQTT review fixes that live at
// the session/receiver/factory level: MQTT-L3 (emit-error stranded delivery
// requests recovery), MQTT-L6 (persistent + clean_start warning), MQTT-F3
// (hostname suffix + persistent warning), MQTT-O3 (generation-safe breaker
// admission preferred by the CircuitBreakerSender).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// MQTT-L3: an emit error strands its un-acked delivery — MQTT brokers do not
// redeliver on a live connection — so a durable receiver must request the
// same bounded session recovery a Delivery.Retry uses. The test holds the
// reload gate so the queued recovery cannot start, making recoveryPending a
// stable observable.
func TestReceiver_EmitErrorRequestsSessionRecoveryForDurableDelivery(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "l3-emit-error",
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { sess.router.shutdown() })
	// The released recovery attempt re-Starts the session; fail that dial
	// immediately through the test seam so the unit test never touches the
	// network (the recovery outcome itself is not under test here).
	sess.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return nil, nil, errors.New("unit test: no broker")
	}

	// Hold the reload gate: the recovery goroutine blocks at serialization,
	// so recoveryPending stays observable until the test releases it.
	require.NoError(t, sess.acquireReload(t.Context()))

	receiver := NewReceiver("l3", sess, WithTopicFilters("l3/topic"))
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(context.Context, ports.Delivery) error {
			return errors.New("downstream unavailable")
		})
	}()
	select {
	case <-receiver.Started():
	case <-t.Context().Done():
		t.Fatal("receiver did not start")
	}

	acked := 0
	sess.Router().dispatch(&pahov5.Publish{Topic: "l3/topic", QoS: 1, Payload: []byte("x")},
		func() error { acked++; return nil })

	select {
	case err := <-runDone:
		require.Error(t, err, "emit error terminates the Run")
	case <-t.Context().Done():
		t.Fatal("receiver Run did not return after emit error")
	}
	assert.Zero(t, acked, "the failed delivery must stay un-acked (broker redelivery is the recovery)")

	sess.mu.Lock()
	pending := sess.recoveryPending
	sess.mu.Unlock()
	assert.True(t, pending,
		"an emit error on a durable delivery must request bounded session recovery (MQTT-L3)")

	// Release the gate so the queued recovery goroutine can run to its
	// (failing, broker-less) completion instead of leaking; the terminal
	// transition closes the events channel, which is the completion signal.
	sess.releaseReload()
	require.Eventually(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.eventsClosed
	}, 5*time.Second, 10*time.Millisecond, "queued recovery must complete after the gate is released")
}

// MQTT-L3 boundary: an ephemeral session has no broker redelivery contract —
// the emit error must not request recovery (requestRecovery is unsupported
// there) and must not panic or wedge the Run exit.
func TestReceiver_EmitErrorOnEphemeralSessionDoesNotRequestRecovery(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "l3-ephemeral",
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { sess.router.shutdown() })

	receiver := NewReceiver("l3e", sess, WithTopicFilters("l3/topic"))
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(context.Context, ports.Delivery) error {
			return errors.New("downstream unavailable")
		})
	}()
	select {
	case <-receiver.Started():
	case <-t.Context().Done():
		t.Fatal("receiver did not start")
	}

	sess.Router().dispatch(&pahov5.Publish{Topic: "l3/topic", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	select {
	case err := <-runDone:
		require.Error(t, err)
	case <-t.Context().Done():
		t.Fatal("receiver Run did not return after emit error")
	}
	sess.mu.Lock()
	pending := sess.recoveryPending
	sess.mu.Unlock()
	assert.False(t, pending, "ephemeral sessions have nothing to recover; no recovery must be queued")
}

// MQTT-L6: persistent + clean_start=true silently wipes the broker-side
// offline backlog on every restart; NewSession must warn like the analogous
// session_expiry_interval=0 misconfiguration does.
func TestNewSession_WarnsOnPersistentCleanStart(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "l6-clean-start",
		CleanStart: true,
	}, connectivity.SessionPersistent, logger)
	t.Cleanup(func() { sess.router.shutdown() })

	assert.Contains(t, buf.String(), "clean_start=true on a persistent session",
		"the misconfiguration must warn at construction (MQTT-L6)")

	buf.Reset()
	quiet := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "l6-default",
	}, connectivity.SessionPersistent, logger)
	t.Cleanup(func() { quiet.router.shutdown() })
	assert.NotContains(t, buf.String(), "clean_start=true on a persistent session")
}

// IDENTITY-1 (supersedes MQTT-F3): persistent mode + client_id_suffix=hostname
// strands broker-side queues on every Deployment/ECS rollout (new hostname → new
// client_id → orphaned session). A startup warning is not an admission boundary,
// so the factory now REJECTS the combination unless the operator explicitly
// asserts a stable-host profile via assert_stable_client_identity.
func TestFactory_RejectsPersistentHostnameSuffixWithoutAssertion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	factory := NewFactory(logger)

	// Rejected without the assertion.
	_, err := factory.NewSession(t.Context(), ports.SessionSpec{
		ID:          "f3-session",
		SessionMode: connectivity.SessionPersistent,
		Config: &Config{Session: SessionOptions{
			BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
			ClientID:       "f3",
			ClientIDSuffix: ClientIDSuffixHostname,
		}},
	})
	require.Error(t, err, "persistent + hostname must be rejected without an explicit stable-identity assertion (IDENTITY-1)")
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)

	// Admitted (with a warning) once the operator asserts a stable-host profile.
	buf.Reset()
	session, err := factory.NewSession(t.Context(), ports.SessionSpec{
		ID:          "f3-asserted",
		SessionMode: connectivity.SessionPersistent,
		Config: &Config{Session: SessionOptions{
			BrokerURLs:                 []string{"tcp://192.0.2.1:1883"},
			ClientID:                   "f3a",
			ClientIDSuffix:             ClientIDSuffixHostname,
			AssertStableClientIdentity: true,
		}},
	})
	require.NoError(t, err, "the explicit assertion must admit the combination")
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	assert.Contains(t, buf.String(), "assert_stable_client_identity=true",
		"an admitted persistent+hostname session must still warn it is unsafe on Deployment/ECS")

	// Ephemeral never resumes broker state, so it is unaffected (no rejection).
	ephemeral, err := factory.NewSession(t.Context(), ports.SessionSpec{
		ID:          "f3-ephemeral",
		SessionMode: connectivity.SessionEphemeral,
		Config: &Config{Session: SessionOptions{
			BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
			ClientID:       "f3e",
			ClientIDSuffix: ClientIDSuffixHostname,
		}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ephemeral.Close(context.Background()) })
}

// admitRecordingBreaker provides BOTH the legacy pair and the
// generation-safe AdmitRequest so the test can prove the sender prefers the
// admission surface (MQTT-O3).
type admitRecordingBreaker struct {
	admitted     int
	settled      []error
	legacyBefore int
	legacyAfter  int
}

func (b *admitRecordingBreaker) BeforeRequest() error { b.legacyBefore++; return nil }
func (b *admitRecordingBreaker) AfterRequest(error)   { b.legacyAfter++ }
func (b *admitRecordingBreaker) AdmitRequest() (func(error), error) {
	b.admitted++
	return func(err error) { b.settled = append(b.settled, err) }, nil
}

var _ ports.CircuitBreaker = (*admitRecordingBreaker)(nil)
var _ ports.CircuitBreakerAdmitter = (*admitRecordingBreaker)(nil)

// MQTT-O3: the CircuitBreakerSender must route admission through the
// generation-safe AdmitRequest when the breaker provides it — route
// max_in_flight drives concurrent Sends, and the token-less pair
// mis-accounts outcomes that land after a breaker state transition.
func TestCircuitBreakerSender_PrefersGenerationSafeAdmission(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "o3-admit",
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { sess.router.shutdown() })
	breaker := &admitRecordingBreaker{}
	cbs := NewCircuitBreakerSender(NewSender(sess, SenderOptions{QoS: 1}), breaker)

	// Session not connected: the inner Send fails fast, which is all the
	// admission accounting needs to observe.
	err := cbs.Send(t.Context(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte("p")}),
		Address:  "o3/topic",
	})
	require.Error(t, err)

	assert.Equal(t, 1, breaker.admitted, "AdmitRequest must be the preferred admission surface")
	require.Len(t, breaker.settled, 1, "the settle callback must fire exactly once with the outcome")
	assert.Error(t, breaker.settled[0])
	assert.Zero(t, breaker.legacyBefore, "the token-less legacy pair must not be used when AdmitRequest exists")
	assert.Zero(t, breaker.legacyAfter)
}
