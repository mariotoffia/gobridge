package paho

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// v5IdentityGoldens is DurableSessionIdentity of goldenIdentityConfig on MQTT 5,
// recorded before MQTT 3.1.1 support existed. The fingerprint keys stored
// managed-subscription history and the duplicate-identity preflight, so adding
// a protocol version must not change any MQTT 5 fingerprint (ADR 0022).
var v5IdentityGoldens = map[connectivity.SessionMode]string{
	connectivity.SessionPersistent: "d837f4bebd266e0507d060ee8628f1eefc18f9e5ff99742eb671d5772a064f9d",
	connectivity.SessionExclusive:  "2ea4d0828c30b8f7f5b6e1db613465b84c92e35c6e774a4409f15efb14926fa1",
}

func goldenIdentityConfig(version string) Config {
	cfg := DefaultConfig()
	cfg.Session.ClientID = "golden"
	cfg.Session.BrokerURLs = []string{"ssl://broker.example:8883"}
	cfg.Session.ProtocolVersion = version
	return cfg
}

func TestDurableSessionIdentity_V5IsUnchanged(t *testing.T) {
	for mode, golden := range v5IdentityGoldens {
		for _, version := range []string{"", ProtocolVersion5} {
			t.Run(string(mode)+"/"+version, func(t *testing.T) {
				cfg := goldenIdentityConfig(version)
				id, err := cfg.DurableSessionIdentity(mode)
				require.NoError(t, err)
				assert.Equal(t, golden, id)
			})
		}
	}
}

func TestDurableSessionIdentity_ProtocolSwitchIsAnIdentityChange(t *testing.T) {
	for mode, golden := range v5IdentityGoldens {
		t.Run(string(mode), func(t *testing.T) {
			v311 := goldenIdentityConfig(ProtocolVersion311)
			id, err := v311.DurableSessionIdentity(mode)
			require.NoError(t, err)
			assert.NotEqual(t, golden, id, "a broker need not resume one protocol's session from the other")

			v311Domains, err := v311.DurableSessionIdentityDomains(mode)
			require.NoError(t, err)
			v5 := goldenIdentityConfig("")
			v5Domains, err := v5.DurableSessionIdentityDomains(mode)
			require.NoError(t, err)
			assert.Equal(t, v5Domains, v311Domains, "a client id collides on a broker whatever the protocol")
		})
	}
}

// countingDial installs a Start dial that counts its calls and hands back a
// live fake connection.
func countingDial(s *Session, dials, disconnects *atomic.Int32) {
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials.Add(1)
		s.mu.Lock()
		s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
		s.mu.Unlock()
		return &fakeLiveConn{disconnects: disconnects}, func() {}, nil
	}
}

func TestNewSession_StartRefusesOptionsMQTT311CannotExpress(t *testing.T) {
	cases := []struct {
		name string
		opts SessionOptions
		mode connectivity.SessionMode
	}{
		{"unknown version", SessionOptions{ProtocolVersion: "v3"}, connectivity.SessionEphemeral},
		{"no_local", SessionOptions{ProtocolVersion: ProtocolVersion311, NoLocal: true}, connectivity.SessionEphemeral},
		{"session expiry", SessionOptions{ProtocolVersion: ProtocolVersion311, SessionExpiryInterval: 3600}, connectivity.SessionPersistent},
		{"password without username", SessionOptions{ProtocolVersion: ProtocolVersion311, Password: shared.NewSecret("p")}, connectivity.SessionEphemeral},
		{"persistent clean start", SessionOptions{ProtocolVersion: ProtocolVersion311, CleanStart: true}, connectivity.SessionPersistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.BrokerURLs = []string{"ssl://192.0.2.1:8883"}
			tc.opts.ClientID = "refused"
			s := NewSession(tc.opts, tc.mode, nil)
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			var dials atomic.Int32
			countingDial(s, &dials, nil)

			require.ErrorIs(t, s.Start(t.Context()), shared.ErrInvalidConfig)
			require.ErrorIs(t, s.Start(t.Context()), shared.ErrInvalidConfig, "the refusal is not a one-off")
			assert.Zero(t, dials.Load(), "a session its protocol version cannot express never dials")
		})
	}
}

func TestNewSession_DurableMQTT311KeepsTheLocalExpiryDefault(t *testing.T) {
	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		t.Run(string(mode), func(t *testing.T) {
			logs := &recordingLogHandler{}
			s := NewSession(SessionOptions{
				ProtocolVersion: ProtocolVersion311,
				BrokerURLs:      []string{"tcp://192.0.2.1:1883"},
				ClientID:        "local-expiry",
			}, mode, slog.New(logs))
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			var dials atomic.Int32
			countingDial(s, &dials, nil)

			assert.Equal(t, uint32(DefaultPersistentSessionExpiry), s.opts.SessionExpiryInterval,
				"the CONNECT still carries an expiry, which the translator turns into Clean Session 0")
			assert.Zero(t, logs.warnCountContaining("session_expiry_interval"),
				"an expiry that is never sent is nothing to warn about")
			require.NoError(t, s.Start(t.Context()), "an omitted expiry is not a configured one")
			assert.Equal(t, int32(1), dials.Load())
		})
	}
}

func TestNewSession_WarnsOnceAboutWhatMQTT311Degrades(t *testing.T) {
	const degradeWarning = "protocol_version v3.1.1"

	logs := &recordingLogHandler{}
	s := NewSession(SessionOptions{
		ProtocolVersion: ProtocolVersion311,
		BrokerURLs:      []string{"tcp://192.0.2.1:1883"},
		ClientID:        "degraded",
	}, connectivity.SessionPersistent, slog.New(logs))
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	assert.Equal(t, 1, logs.warnCountContaining(degradeWarning))

	v5Logs := &recordingLogHandler{}
	v5 := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "full",
	}, connectivity.SessionPersistent, slog.New(v5Logs))
	t.Cleanup(func() { _ = v5.Close(context.Background()) })
	assert.Zero(t, v5Logs.warnCountContaining(degradeWarning))
	assert.Equal(t, 1, v5Logs.warnCountContaining("session_expiry_interval"),
		"MQTT 5 still warns about the expiry it sends")
}

func TestApplyCredentials_RotationToAPasswordWithoutUsernameIsRefusedOnMQTT311(t *testing.T) {
	origTLS := &TLSConfig{Enable: true, CertPEM: shared.NewSecret("cert-old"), KeyPEM: shared.NewSecret("key-old")}
	s := NewSession(SessionOptions{
		ProtocolVersion: ProtocolVersion311,
		BrokerURLs:      []string{"ssl://192.0.2.1:8883"},
		ClientID:        "rotation-311",
		Username:        "u",
		Password:        shared.NewSecret("p"),
		TLS:             origTLS,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	var dials, disconnects atomic.Int32
	countingDial(s, &dials, &disconnects)
	require.NoError(t, s.Start(t.Context()))

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(
		pwCred("", "x"), tlsMat("cert-new", "key-new", nil, false)))
	require.ErrorIs(t, err, shared.ErrInvalidConfig)

	s.mu.Lock()
	live, username, password, tlsOpts := s.liveCreds, s.opts.Username, s.opts.Password.Reveal(), s.opts.TLS
	s.mu.Unlock()
	assert.Equal(t, mqttCredentials{Username: "u", Password: "p"}, live)
	assert.Equal(t, "u", username)
	assert.Equal(t, "p", password)
	assert.Same(t, origTLS, tlsOpts, "a refused rotation leaves the TLS material alone too")
	assert.Equal(t, int32(1), dials.Load(), "a refused rotation does not reconnect")
	assert.Zero(t, disconnects.Load())

	require.NoError(t, s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)),
		"a username and password is a rotation MQTT 3.1.1 can express")
	assert.Equal(t, int32(2), dials.Load())
}

func TestApplyCredentials_RotationToAPasswordWithoutUsernameIsAllowedOnV5(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"ssl://192.0.2.1:8883"},
		ClientID:   "rotation-v5",
		Username:   "u",
		Password:   shared.NewSecret("p"),
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	var dials atomic.Int32
	countingDial(s, &dials, nil)
	require.NoError(t, s.Start(t.Context()))

	require.NoError(t, s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("", "x"), nil)))
	s.mu.Lock()
	live := s.liveCreds
	s.mu.Unlock()
	assert.Equal(t, mqttCredentials{Password: "x"}, live)
	assert.Equal(t, int32(2), dials.Load())
}

func TestBuildPublish_MQTT311PublishesWhatV5WouldRefuseForAHeader(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		headers map[string]any
		v5Err   error
	}{
		{"oversized header", "", map[string]any{"x-app-note": strings.Repeat("h", mqttFieldLimitOverflow)}, shared.ErrPayloadTooLarge},
		{"header that is not UTF-8", "", map[string]any{"x-app-note": "\xff"}, shared.ErrInvalidPayload},
		{"oversized subject", strings.Repeat("s", mqttFieldLimitOverflow), nil, shared.ErrPayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: tc.subject, Payload: []byte("body")})
			if tc.headers != nil {
				env.StampHeaders(tc.headers)
			}
			opts := SenderOptions{QoS: 2, Retain: true}

			_, err := (&pahoConn{}).buildPublish(env, "t/out", opts, nil)
			require.ErrorIs(t, err, tc.v5Err)

			pub, err := (&pahoConn{mqtt311: true}).buildPublish(env, "t/out", opts, nil)
			require.NoError(t, err)
			assert.Nil(t, pub.Properties)
			assert.Equal(t, "t/out", pub.Topic)
			assert.Equal(t, byte(2), pub.QoS)
			assert.True(t, pub.Retain)
			assert.Equal(t, []byte("body"), pub.Payload)
		})
	}
}

func TestBuildPublish_MQTT311StillValidatesTheTopic(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Payload: []byte("body")})
	v311 := &pahoConn{mqtt311: true}

	_, err := v311.buildPublish(env, strings.Repeat("t", mqttFieldLimitOverflow), SenderOptions{QoS: 1}, nil)
	require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
	_, err = v311.buildPublish(env, "t/\xff", SenderOptions{QoS: 1}, nil)
	require.ErrorIs(t, err, shared.ErrInvalidPayload)
}

// TestPublishEnvelope_MQTT311HandsTheBarePublishToTheSDK pins that egress goes
// through buildPublish: on MQTT 3.1.1 a header MQTT 5 would refuse reaches the
// SDK, here a connection manager with no connection, instead of being
// rejected.
func TestPublishEnvelope_MQTT311HandsTheBarePublishToTheSDK(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Payload: []byte("body")})
	env.StampHeaders(map[string]any{"x-app-note": strings.Repeat("h", mqttFieldLimitOverflow)})

	v5Metrics := &ports.RecordingExporter{}
	v5 := &pahoConn{cm: &autopaho.ConnectionManager{}, metrics: v5Metrics}
	_, err := v5.PublishEnvelope(t.Context(), env, "t/out", SenderOptions{QoS: 1}, nil)
	require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
	assert.Len(t, v5Metrics.FindEntries(MetricMQTTEgressRejected), 1)

	v311Metrics := &ports.RecordingExporter{}
	v311 := &pahoConn{cm: &autopaho.ConnectionManager{}, metrics: v311Metrics, mqtt311: true}
	_, err = v311.PublishEnvelope(t.Context(), env, "t/out", SenderOptions{QoS: 1}, nil)
	require.ErrorIs(t, err, autopaho.ConnectionDownError, "the publish reached the SDK")
	assert.Empty(t, v311Metrics.FindEntries(MetricMQTTEgressRejected))
}

const shortLivedDropLog = "keeps dropping soon after it connects"

// newConnectionLifetimeTestSession builds a persistent session on a fake clock
// with a live fake connection, as the ingress-reject tests do.
func newConnectionLifetimeTestSession(
	t *testing.T,
	version string,
) (*Session, *clocktest.Fake, *ports.RecordingExporter, *recordingLogHandler) {
	t.Helper()
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	rec := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}
	s := NewSession(SessionOptions{
		ProtocolVersion: version,
		BrokerURLs:      []string{"tcp://192.0.2.1:1883"},
		ClientID:        "connection-lifetime",
		Clock:           clk,
	}, connectivity.SessionPersistent, slog.New(logs), rec)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.mu.Unlock()
	return s, clk, rec, logs
}

// connectionLives drives the edges autopaho raises for one connection: up, then
// down after lifetime. beforeDown runs just before the down edge.
func connectionLives(t *testing.T, s *Session, clk *clocktest.Fake, lifetime time.Duration, beforeDown func()) {
	t.Helper()
	gen := connectionGenerationOf(s)
	s.handleConnectionUpGeneration(gen)
	clk.Advance(lifetime)
	if beforeDown != nil {
		beforeDown()
	}
	require.True(t, s.handleConnectionDownGeneration(gen), "the dropped connection must report down")
}

func takeoverStreakOf(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeoverStreak
}

func TestShortLivedMQTT311Connections_GrowTheTakeoverPenalty(t *testing.T) {
	s, clk, rec, logs := newConnectionLifetimeTestSession(t, ProtocolVersion311)

	wantPenalties := []time.Duration{
		0,           // one drop is an ordinary failover or network blip
		time.Second, // then the penalty starts
		2 * time.Second,
	}
	for i, want := range wantPenalties {
		connectionLives(t, s, clk, time.Second, nil)
		assert.Equal(t, want, s.takeoverPenalty(), "penalty after short-lived connection #%d", i+1)
		wantErrorLogs := 0
		if i+1 >= 3 {
			wantErrorLogs = 1
		}
		assert.Equal(t, wantErrorLogs, logs.messageCountContaining(slog.LevelError, shortLivedDropLog),
			"the client-id collision is named from the third drop, after drop #%d", i+1)
	}
	assert.Empty(t, rec.FindEntries(MetricMQTTSessionTakeover),
		"an inferred collision is not a takeover the broker reported")

	connectionLives(t, s, clk, connectionStabilityWindow, nil)
	assert.Zero(t, takeoverStreakOf(s), "a stable connection ends the storm")
	assert.Zero(t, s.takeoverPenalty())
}

func TestShortLivedConnection_IgnoresV5AndOwnIngressRejects(t *testing.T) {
	t.Run("v5", func(t *testing.T) {
		s, clk, _, logs := newConnectionLifetimeTestSession(t, "")
		for range 3 {
			connectionLives(t, s, clk, time.Second, nil)
		}
		assert.Zero(t, takeoverStreakOf(s), "MQTT 5 reports a takeover itself (0x8E)")
		assert.Zero(t, s.takeoverPenalty())
		assert.Zero(t, logs.messageCountContaining(slog.LevelError, shortLivedDropLog))
	})

	t.Run("v3.1.1 dropped by the guard's reject", func(t *testing.T) {
		s, clk, _, _ := newConnectionLifetimeTestSession(t, ProtocolVersion311)
		for range 3 {
			connectionLives(t, s, clk, time.Second, func() {
				s.rejectPredecodeIngress(newMQTTMalformedError())
			})
		}
		assert.Zero(t, takeoverStreakOf(s), "the session dropped the connection itself")
		assert.Zero(t, s.takeoverPenalty())
	})

	t.Run("v3.1.1 dropped by the translator's violation", func(t *testing.T) {
		s, clk, _, _ := newConnectionLifetimeTestSession(t, ProtocolVersion311)
		for range 3 {
			connectionLives(t, s, clk, time.Second, func() {
				translator, err := s.translateMQTT311(newTestNetConn([]byte{0x40, 0x03, 0x00, 0x01, 0x87}, 0))
				require.NoError(t, err)
				_, err = translator.Read(make([]byte, 16))
				require.Error(t, err, "a PUBACK with a reason code is malformed MQTT 3.1.1")
			})
		}
		assert.Zero(t, takeoverStreakOf(s), "the session dropped the connection itself")
		assert.Zero(t, s.takeoverPenalty())
	})
}

func TestTranslateMQTT311_UsesTheSessionLimitsAndReportsViolationsAsIngressRejects(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		ProtocolVersion: ProtocolVersion311,
		BrokerURLs:      []string{"tcp://192.0.2.1:1883"},
		ClientID:        "translator-limits",
		MaxPayloadBytes: 2048,
		ReceiveMaximum:  7,
	}, connectivity.SessionEphemeral, nil, rec)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	raw := newTestNetConn([]byte{0x40, 0x03, 0x00, 0x01, 0x87}, 0)
	translator, err := s.translateMQTT311(raw)
	require.NoError(t, err)
	wantPacketSize, err := wirePacketSizeFor(2048)
	require.NoError(t, err)
	assert.Equal(t, uint32(2048), translator.maxPayloadBytes)
	assert.Equal(t, wantPacketSize, translator.maximumPacketSize)
	assert.Equal(t, 7, translator.receiveMaximum)

	_, err = translator.Read(make([]byte, 16))
	var ingressErr *mqttIngressError
	require.ErrorAs(t, err, &ingressErr)
	assert.Len(t, rec.FindEntries(MetricMQTTIngressRejected), 1)
	s.mu.Lock()
	rejectErr := s.ingressRejectErr
	s.mu.Unlock()
	assert.ErrorAs(t, rejectErr, &ingressErr, "health names the violation as an ingress reject")
	assert.Empty(t, raw.Written(), "a violation writes no DISCONNECT, so the broker publishes the Last Will")
}

// TestAttemptGuardedConnection_MQTT311PutsTheTranslatorBelowTheGuard pins the
// stream ADR 0022 describes: Paho writes MQTT 5 to the guard, and only on
// MQTT 3.1.1 does a translator between the guard and the socket turn it into
// MQTT 3.1.1.
func TestAttemptGuardedConnection_MQTT311PutsTheTranslatorBelowTheGuard(t *testing.T) {
	cases := []struct {
		version       string
		protocolLevel byte
	}{
		{"", 5},
		{ProtocolVersion5, 5},
		{ProtocolVersion311, mqtt311ProtocolLevel},
	}
	for _, tc := range cases {
		t.Run("version "+tc.version, func(t *testing.T) {
			isolateProxyEnv(t, nil) // the shell's ALL_PROXY must not route this loopback dial
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				accepted <- conn
			}()

			s := NewSession(SessionOptions{ProtocolVersion: tc.version, ClientID: "guarded-dial"},
				connectivity.SessionEphemeral, nil)
			t.Cleanup(func() { _ = s.Close(context.Background()) })

			conn, err := s.attemptGuardedConnection(boundedDialContext(t), autopaho.ClientConfig{},
				&url.URL{Scheme: "mqtt", Host: listener.Addr().String()})
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			server := wait.RequireReceive(t, accepted, 5*time.Second)
			t.Cleanup(func() { _ = server.Close() })

			guard, ok := conn.(*mqttIngressConn)
			require.True(t, ok, "the guard stays the connection Paho writes through")
			_, translated := guard.Conn.(*mqtt311Conn)
			assert.Equal(t, tc.version == ProtocolVersion311, translated)

			received := make(chan []byte, 1)
			go func() {
				head := make([]byte, 9) // fixed header, Remaining Length, "MQTT", protocol level
				if _, readErr := io.ReadFull(server, head); readErr == nil {
					received <- head
				}
			}()
			cp := packets.NewControlPacket(packets.CONNECT)
			connect, ok := cp.Content.(*packets.Connect)
			require.True(t, ok)
			connect.ClientID = "c"
			_, err = cp.WriteTo(conn)
			require.NoError(t, err)
			head := wait.RequireReceive(t, received, 5*time.Second)
			assert.Equal(t, []byte{0, 4, 'M', 'Q', 'T', 'T'}, head[2:8])
			assert.Equal(t, tc.protocolLevel, head[8], "the protocol level the broker receives")
		})
	}
}

// serveOneConnect plays a broker on a loopback listener for one connection: it
// reports the protocol level of the CONNECT it receives, answers with connack
// and then reads until the client goes away.
func serveOneConnect(t *testing.T, connack []byte) (brokerURL string, protocolLevel <-chan byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	levels := make(chan byte, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var fixedHeader [1]byte
		var scratch [4]byte
		if _, readErr := io.ReadFull(conn, fixedHeader[:]); readErr != nil {
			return
		}
		remaining, _, readErr := readMQTTVBI(conn, scratch[:])
		if readErr != nil {
			return
		}
		body := make([]byte, remaining)
		if _, readErr = io.ReadFull(conn, body); readErr != nil || len(body) < 7 {
			return
		}
		levels <- body[6] // after the protocol name "MQTT"
		if _, writeErr := conn.Write(connack); writeErr != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-served
	})
	return "tcp://" + listener.Addr().String(), levels
}

// TestStart_MQTT311ConnectsThroughTheTranslatorAndPublishesBare drives a real
// Start against a loopback broker: on MQTT 3.1.1 the broker receives a 3.1.1
// CONNECT, its 3.1.1 CONNACK brings the session up, and the connection the
// sender publishes through builds bare publishes.
func TestStart_MQTT311ConnectsThroughTheTranslatorAndPublishesBare(t *testing.T) {
	cases := []struct {
		version       string
		connack       []byte
		protocolLevel byte
	}{
		{"", []byte{0x20, 0x03, 0x00, 0x00, 0x00}, 5},
		{ProtocolVersion311, []byte{0x20, 0x02, 0x00, 0x00}, mqtt311ProtocolLevel},
	}
	for _, tc := range cases {
		t.Run("version "+tc.version, func(t *testing.T) {
			isolateProxyEnv(t, nil) // the shell's ALL_PROXY must not route this loopback dial
			brokerURL, levels := serveOneConnect(t, tc.connack)
			s := NewSession(SessionOptions{
				ProtocolVersion: tc.version,
				BrokerURLs:      []string{brokerURL},
				ClientID:        "loopback",
				ConnectTimeout:  10 * time.Second,
			}, connectivity.SessionEphemeral, nil)
			t.Cleanup(func() { _ = s.Close(context.Background()) })

			require.NoError(t, s.Start(boundedDialContext(t)))
			assert.Equal(t, tc.protocolLevel, wait.RequireReceive(t, levels, 5*time.Second))
			conn, ok := s.connection().(*pahoConn)
			require.True(t, ok)
			assert.Equal(t, tc.version == ProtocolVersion311, conn.mqtt311)
		})
	}
}
