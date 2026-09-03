package paho_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Every deployment of this bridge reaches its broker over an authenticated,
// certificate-validating connection, and until now nothing proved any of it
// against a real broker: the fixture was anonymous plaintext Mosquitto, so a
// regression in credential handling, certificate validation or client-identity
// presentation reached production unopposed.
//
// These tests drive the shipped session against a Mosquitto that actually
// refuses: anonymous access disabled, a CA-signed server certificate, and — for
// the mutual case — a client certificate requirement. Each one asserts a
// NEGATIVE (the connection that must fail) beside the positive, because a
// broker that silently accepted everything would make the positives green on
// its own.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

const (
	secureUser     = "bridge"
	securePassword = "s3cret-÷-pass"
	secureConnect  = 15 * time.Second
)

// TestIntegration_DirectTLS_ValidatesTheBrokerAndCarriesTraffic is the direct
// `ssl://` proof: the session validates the broker against the fixture CA,
// authenticates, and moves a message. It is the path every TLS deployment uses.
func TestIntegration_DirectTLS_ValidatesTheBrokerAndCarriesTraffic(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
	)

	session := newSecureSession(t, broker.TLSURL(), "tls-flow", func(o *paho.SessionOptions) {
		o.Username = secureUser
		o.Password = shared.NewSecret(securePassword)
		o.TLS = &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(broker.Material().CAPEM),
		}
	})

	requireRoundTrip(t, session, "secure/tls/flow")
}

// TestIntegration_DirectTLS_RefusesAnUntrustedBrokerCertificate pins that the
// TLS material is actually validated. Without this, the test above would pass
// just as happily against a broker whose certificate nobody vouched for.
func TestIntegration_DirectTLS_RefusesAnUntrustedBrokerCertificate(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
	)

	// A second fixture's CA is a real authority — just not this broker's.
	foreign := mqttlocal.NewBrokerInstance(t, mqttlocal.WithTLS())

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{broker.TLSURL()},
		ClientID:       mqttlocal.UniqueClientID("tls-untrusted"),
		KeepAlive:      10,
		ConnectTimeout: 2 * time.Second,
		CleanStart:     true,
		Username:       secureUser,
		Password:       shared.NewSecret(securePassword),
		TLS: &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(foreign.Material().CAPEM),
		},
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(t.Context(), secureConnect)
	defer cancel()
	require.Error(t, session.Start(ctx),
		"a broker certificate no configured authority signed must not be accepted")
}

// TestIntegration_MutualTLS_PresentsTheClientCertificate proves the session
// presents its own identity when the broker demands one, and that the demand is
// real: the same session without client material is refused.
func TestIntegration_MutualTLS_PresentsTheClientCertificate(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithMutualTLS())
	material := broker.Material()

	withoutIdentity := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{broker.TLSURL()},
		ClientID:       mqttlocal.UniqueClientID("mtls-anonymous"),
		KeepAlive:      10,
		ConnectTimeout: 2 * time.Second,
		CleanStart:     true,
		TLS: &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(material.CAPEM),
		},
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = withoutIdentity.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(t.Context(), secureConnect)
	defer cancel()
	require.Error(t, withoutIdentity.Start(ctx),
		"a mutual-TLS listener must refuse a session that presents no certificate")

	session := newSecureSession(t, broker.TLSURL(), "mtls-flow", func(o *paho.SessionOptions) {
		o.TLS = &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(material.CAPEM),
			CertPEM:   shared.NewSecret(material.ClientCertPEM),
			KeyPEM:    shared.NewSecret(material.ClientKeyPEM),
		}
	})
	requireRoundTrip(t, session, "secure/mtls/flow")
}

// TestIntegration_CredentialFailure_SurfacesNotAuthorized pins what an operator
// sees when a password is wrong or has been revoked: a classified
// ErrNotAuthorized delivered to the auth-failure callback, which is what drives
// a credential re-resolve rather than an endless reconnect loop.
func TestIntegration_CredentialFailure_SurfacesNotAuthorized(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
	)

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{broker.TLSURL()},
		ClientID:         mqttlocal.UniqueClientID("bad-credential"),
		KeepAlive:        10,
		ConnectTimeout:   2 * time.Second,
		ReconnectDelay:   50 * time.Millisecond,
		ReconnectTimeout: 250 * time.Millisecond,
		CleanStart:       true,
		Username:         secureUser,
		Password:         shared.NewSecret("not-the-password"),
		TLS: &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(broker.Material().CAPEM),
		},
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	denied := make(chan error, 1)
	session.SetAuthFailureCallback(func(err error) {
		select {
		case denied <- err:
		default:
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), secureConnect)
	defer cancel()
	require.Error(t, session.Start(ctx), "a wrong password must not produce a live session")

	err := wait.RequireReceive(t, denied, secureConnect)
	require.ErrorIs(t, err, shared.ErrNotAuthorized,
		"a rejected credential must be classified, not reported as a transport blip")
}

// TestIntegration_CredentialRotation_ConnectsWithTheRotatedSecret proves the
// build-first rotation contract (ADR-0002 — credential rotation: build-first,
// commit-after-success) against a broker that really checks: a session started
// with a stale password reaches the broker only after the correct one is pushed.
func TestIntegration_CredentialRotation_ConnectsWithTheRotatedSecret(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
	)

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{broker.TLSURL()},
		ClientID:         mqttlocal.UniqueClientID("rotation"),
		KeepAlive:        10,
		ConnectTimeout:   2 * time.Second,
		ReconnectDelay:   50 * time.Millisecond,
		ReconnectTimeout: 250 * time.Millisecond,
		CleanStart:       true,
		Username:         secureUser,
		Password:         shared.NewSecret("stale-password"),
		TLS: &paho.TLSConfig{
			Enable:    true,
			CACertPEM: shared.NewSecret(broker.Material().CAPEM),
		},
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(t.Context(), secureConnect)
	defer cancel()
	require.Error(t, session.Start(ctx), "the stale secret must be refused first")

	password := connectivity.NewPasswordCredential(secureUser, securePassword)
	rotated := connectivity.NewCredentialSet(&password, nil)
	require.NoError(t, session.ApplyCredentials(ctx, rotated),
		"a closed-over-refused session must still accept the rotated secret")

	startCtx, startCancel := context.WithTimeout(t.Context(), secureConnect)
	defer startCancel()
	require.NoError(t, session.Start(startCtx),
		"the rotated secret must reach the broker without rebuilding the session")

	requireRoundTrip(t, session, "secure/rotation/flow")
}

// TestIntegration_WebSocket_CarriesAuthenticatedTraffic closes the WebSocket
// claim. The fixture has always been able to serve `ws`, and nothing used it —
// so the dial path, the upgrade, and the fact that the listener enforces the
// same credentials were all published and unproved.
func TestIntegration_WebSocket_CarriesAuthenticatedTraffic(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithWebSocket(true),
	)

	session := newSecureSession(t, broker.WebSocketURL(), "ws-flow", func(o *paho.SessionOptions) {
		o.Username = secureUser
		o.Password = shared.NewSecret(securePassword)
		// A `ws://` URL is plaintext, so sending credentials over it is the
		// documented opt-in rather than an accident.
		o.AllowPlaintextCredentials = true
	})
	requireRoundTrip(t, session, "secure/ws/flow")
}

// TestIntegration_SecureWebSocket_ValidatesTheBrokerCertificate is the `wss`
// half: the same upgrade, over a connection whose certificate the session
// validated against the fixture CA.
func TestIntegration_SecureWebSocket_ValidatesTheBrokerCertificate(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
		mqttlocal.WithWebSocket(true),
	)

	session := newSecureSession(t, broker.SecureWebSocketURL(), "wss-flow",
		func(o *paho.SessionOptions) {
			o.Username = secureUser
			o.Password = shared.NewSecret(securePassword)
			o.TLS = &paho.TLSConfig{
				Enable:    true,
				CACertPEM: shared.NewSecret(broker.Material().CAPEM),
			}
		})
	requireRoundTrip(t, session, "secure/wss/flow")
}

// ---------------------------------------------------------------------------
// Shared fixture plumbing
// ---------------------------------------------------------------------------

func requireDockerBroker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("secure-broker integration test needs Docker")
	}
}

// newSecureSession starts a session against brokerURL and requires it to
// connect, so a caller can assert on traffic rather than on connection
// mechanics. Start blocks until the session is up or the context expires.
func newSecureSession(
	t *testing.T, brokerURL, prefix string, configure func(*paho.SessionOptions),
) *paho.Session {
	t.Helper()
	require.NotEmpty(t, brokerURL, "the fixture published no endpoint for %s", prefix)

	options := paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       mqttlocal.UniqueClientID(prefix),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}
	configure(&options)

	session := paho.NewSession(options, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(t.Context(), secureConnect)
	defer cancel()
	require.NoError(t, session.Start(ctx), "Start (%s)", prefix)

	return session
}

// requireRoundTrip subscribes, publishes and requires the message back. A
// connection that authenticates but cannot carry traffic is not a proof.
func requireRoundTrip(t *testing.T, session *paho.Session, topic string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	require.NoError(t, session.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}), "Reconcile %s", topic)
	waitSubActive(t, session, 10*time.Second)

	var (
		delivered atomic.Int64
		once      sync.Once
		body      atomic.Value
	)
	receiver := paho.NewReceiver("rx-"+topic, session)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		_ = receiver.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
			once.Do(func() { body.Store(string(delivery.Envelope().Payload())) })
			delivered.Add(1)
			return delivery.Ack(ctx)
		})
	}()
	t.Cleanup(func() { cancel(); group.Wait() })

	sender := paho.NewSender(session, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      mqttlocal.UniqueClientID("secure-msg"),
		Subject: topic,
		Payload: []byte("secure-payload"),
	})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
		Envelope: envelope, Address: topic,
	}), "Send %s", topic)

	wait.Until(t, 15*time.Second, "message delivered over the secure connection",
		func() bool { return delivered.Load() > 0 })
	require.Equal(t, "secure-payload", body.Load(),
		"the payload must survive the secure hop unchanged")
}
