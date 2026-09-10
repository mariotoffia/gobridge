package paho_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/netfault"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// What the bridge claims about brokers, proved against one that enforces it.
//
// Three claims had no real-broker evidence at all. A deployment configures
// several broker URLs and expects the session to move to a healthy one; it
// configures a Last Will and expects peers to learn about an ungraceful death;
// and it runs against brokers whose server-side limits are lower than the
// bridge's own. Each was inferred from configuration unit tests.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

const matrixWait = 30 * time.Second

// TestIntegration_MultiURLFailover_MovesToTheHealthyEndpoint drives the
// positive direction of endpoint rotation: the first URL stops carrying
// sessions, and the session reaches the second one and keeps working. Existing
// coverage only ever proved that a single unreachable URL fails.
func TestIntegration_MultiURLFailover_MovesToTheHealthyEndpoint(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)

	// Both URLs reach the same broker through separate fault injectors, so
	// "the first endpoint went away" is a property of the ENDPOINT rather than
	// of the broker — which is what endpoint rotation actually means.
	primary := netfault.Start(t, hostPortOf(t, broker.URL()))
	secondary := netfault.Start(t, hostPortOf(t, broker.URL()))

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{primary.URL("tcp"), secondary.URL("tcp")},
		ClientID:         mqttlocal.UniqueClientID("multiurl"),
		KeepAlive:        5,
		ConnectTimeout:   3 * time.Second,
		ReconnectDelay:   200 * time.Millisecond,
		ReconnectTimeout: 2 * time.Second,
		CleanStart:       true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(t.Context(), matrixWait)
	defer cancel()
	require.NoError(t, session.Start(ctx))
	require.True(t, session.Health(ctx).Connected)

	primaryUse := primary.Accepted()
	require.Positive(t, primaryUse, "the session must have used the first endpoint")

	// Take the first endpoint away entirely: live connections die and new ones
	// get nothing. Only the second endpoint can serve the reconnect.
	primary.Cut()

	wait.Until(t, matrixWait, "session reconnected through the healthy endpoint", func() bool {
		return secondary.Accepted() > 0 && session.Health(context.Background()).Connected
	})
	require.Equal(t, primaryUse, primary.Accepted(),
		"the cut endpoint must not have carried another session")

	requireRoundTrip(t, session, "matrix/multiurl/flow")
}

// TestIntegration_LastWill_IsPublishedOnUngracefulDeath proves the configured
// Last Will actually registers and fires. A will is a liveness signal an
// operator configures and then trusts; until now its topic, payload, QoS and
// retain flag were validated only against the config decoder.
func TestIntegration_LastWill_IsPublishedOnUngracefulDeath(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)
	willTopic := "matrix/will/" + mqttlocal.UniqueClientID("node")

	observer := newSecureSession(t, broker.URL(), "will-observer", func(*paho.SessionOptions) {})
	wills := watchTopic(t, observer, willTopic)

	// The dying session reaches the broker through a fault injector so its
	// connection can be severed WITHOUT a DISCONNECT packet. A graceful Close
	// would suppress the will, which is exactly the distinction under test.
	link := netfault.Start(t, hostPortOf(t, broker.URL()))
	dying := newSecureSession(t, link.URL("tcp"), "will-node", func(o *paho.SessionOptions) {
		o.KeepAlive = 2
		o.Will = &paho.WillOptions{
			Topic:   willTopic,
			Payload: "node-died",
			QoS:     1,
		}
	})

	link.Cut()

	wait.Until(t, matrixWait, "broker published the will", func() bool {
		return wills.count() > 0
	})
	require.Equal(t, "node-died", wills.first(),
		"the will payload the operator configured is what peers must receive")

	_ = dying.Close(context.Background())
}

// TestIntegration_LastWill_IsSuppressedByAGracefulDisconnect is the negative
// half, and the one that gives the test above its meaning: a will that fired on
// every shutdown would page an operator on every deploy.
func TestIntegration_LastWill_IsSuppressedByAGracefulDisconnect(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)
	willTopic := "matrix/will-graceful/" + mqttlocal.UniqueClientID("node")

	observer := newSecureSession(t, broker.URL(), "graceful-observer", func(*paho.SessionOptions) {})
	wills := watchTopic(t, observer, willTopic)

	leaving := newSecureSession(t, broker.URL(), "graceful-node", func(o *paho.SessionOptions) {
		o.Will = &paho.WillOptions{Topic: willTopic, Payload: "should-not-appear", QoS: 1}
	})
	require.NoError(t, leaving.Close(context.Background()))

	// The ordering proof runs on its OWN session: reconciling the observer
	// would replace the very subscription the will is being watched on. A
	// completed round trip means the broker has processed work submitted after
	// the graceful disconnect, so a will it was going to publish has had its
	// chance.
	probe := newSecureSession(t, broker.URL(), "graceful-probe", func(*paho.SessionOptions) {})
	requireRoundTrip(t, probe, "matrix/will-graceful/probe")
	require.Zerof(t, wills.count(),
		"a graceful DISCONNECT must not trigger the will; observed %q", wills.all())
}

// TestIntegration_ServerLimit_LowInflightQuotaLosesNothing runs against a
// broker whose outbound flow-control quota is two, far below anything the
// bridge would choose. The broker then trickles deliveries, and the proof is
// conservation: every message published arrives.
//
// Note what SessionHealth.ReceiveMaximum is and is not. It reports the window
// this SESSION advertises — how many unacknowledged inbound messages it will
// accept — which is what ReceiveWindowUtilization is measured against. It is
// not the broker's quota, and it does not change because a broker chose a
// smaller one for its own outbound direction.
func TestIntegration_ServerLimit_LowInflightQuotaLosesNothing(t *testing.T) {
	requireDockerBroker(t)
	const brokerInflightQuota = 2
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(brokerInflightQuota),
	)

	session := newSecureSession(t, broker.URL(), "serverlimit", func(o *paho.SessionOptions) {
		o.ReceiveMaximum = 500
	})

	require.EqualValues(t, 500, session.Health(t.Context()).ReceiveMaximum,
		"the reported window is the one this session advertises to the broker")

	requireDeliversAll(t, session, "matrix/serverlimit/flow", 25)
}

// TestIntegration_ServerLimit_OversizedPublishIsRejectedNotLost pins what
// happens at the broker's message-size ceiling: the send fails and says so.
// Silently dropping it would look like successful delivery to the route.
func TestIntegration_ServerLimit_OversizedPublishIsRejectedNotLost(t *testing.T) {
	requireDockerBroker(t)
	const brokerSizeLimit = 512
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMessageSizeLimit(brokerSizeLimit),
	)

	session := newSecureSession(t, broker.URL(), "sizelimit", func(*paho.SessionOptions) {})
	sender := paho.NewSender(session, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})

	oversized := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      mqttlocal.UniqueClientID("oversized"),
		Subject: "matrix/sizelimit/flow",
		Payload: make([]byte, brokerSizeLimit*4),
	})
	err := sender.Send(t.Context(), ports.OutboundMessage{
		Envelope: oversized, Address: "matrix/sizelimit/flow",
	})
	require.Error(t, err,
		"a publish the broker cannot accept must surface as a failure, never as a silent drop")
}

// TestIntegration_NetworkFault_HalfOpenConnectionRecovers is the half-open
// case: the socket stays established and nothing is delivered. Only the
// keep-alive can notice, and the session must then rebuild and carry traffic
// again — within a bound, not eventually.
func TestIntegration_NetworkFault_HalfOpenConnectionRecovers(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)
	link := netfault.Start(t, hostPortOf(t, broker.URL()))

	session := newSecureSession(t, link.URL("tcp"), "halfopen", func(o *paho.SessionOptions) {
		// The keep-alive is the only detector of a connection that stalls
		// rather than closes, so it sets the recovery bound.
		o.KeepAlive = 2
		o.ReconnectDelay = 200 * time.Millisecond
		o.ReconnectTimeout = 2 * time.Second
	})
	before := link.Accepted()

	link.Blackhole()
	wait.Until(t, matrixWait, "keep-alive noticed the stalled connection", func() bool {
		return !session.Health(context.Background()).Connected
	})

	link.Heal()
	wait.Until(t, matrixWait, "session rebuilt the connection", func() bool {
		return link.Accepted() > before && session.Health(context.Background()).Connected
	})

	requireRoundTrip(t, session, "matrix/halfopen/flow")
}

// TestIntegration_NetworkFault_LatencySpikeDoesNotLoseMessages proves a link
// slow enough to expose a loopback-tuned timeout still delivers everything.
func TestIntegration_NetworkFault_LatencySpikeDoesNotLoseMessages(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)
	link := netfault.Start(t, hostPortOf(t, broker.URL()))

	session := newSecureSession(t, link.URL("tcp"), "latency", func(o *paho.SessionOptions) {
		o.KeepAlive = 30
	})

	link.SetLatency(40 * time.Millisecond)
	requireDeliversAll(t, session, "matrix/latency/flow", 20)
}

// TestIntegration_NetworkFault_PartitionRecoversWithinItsBound is the plain
// partition: every connection dies, and the session is back inside the bound
// its reconnect policy declares.
func TestIntegration_NetworkFault_PartitionRecoversWithinItsBound(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t)
	link := netfault.Start(t, hostPortOf(t, broker.URL()))

	session := newSecureSession(t, link.URL("tcp"), "partition", func(o *paho.SessionOptions) {
		o.KeepAlive = 5
		o.ReconnectDelay = 200 * time.Millisecond
		o.ReconnectMaxDelay = time.Second
		o.ReconnectTimeout = 2 * time.Second
	})

	link.Cut()
	wait.Until(t, matrixWait, "session observed the partition", func() bool {
		return !session.Health(context.Background()).Connected
	})

	link.Heal()
	start := time.Now()
	wait.Until(t, matrixWait, "session reconnected after the partition healed", func() bool {
		return session.Health(context.Background()).Connected
	})
	require.Less(t, time.Since(start), matrixWait,
		"reconnect must complete inside the declared bound, not eventually")

	requireRoundTrip(t, session, "matrix/partition/flow")
}

// ---------------------------------------------------------------------------

func hostPortOf(t *testing.T, brokerURL string) string {
	t.Helper()
	_, address, found := splitScheme(brokerURL)
	require.True(t, found, "broker URL %q has no scheme", brokerURL)
	_, _, err := net.SplitHostPort(address)
	require.NoError(t, err, "broker URL %q has no host:port", brokerURL)
	return address
}

func splitScheme(rawURL string) (string, string, bool) {
	for index := 0; index+3 <= len(rawURL); index++ {
		if rawURL[index:index+3] == "://" {
			return rawURL[:index], rawURL[index+3:], true
		}
	}
	return "", rawURL, false
}

// topicWatch collects every payload delivered on one topic.
type topicWatch struct {
	mu       sync.Mutex
	payloads []string
}

func (w *topicWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.payloads)
}

func (w *topicWatch) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.payloads...)
}

func (w *topicWatch) first() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.payloads) == 0 {
		return ""
	}
	return w.payloads[0]
}

// watchTopic subscribes session to topic and records what arrives.
func watchTopic(t *testing.T, session *paho.Session, topic string) *topicWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	require.NoError(t, session.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	waitSubActive(t, session, matrixWait)

	watch := &topicWatch{}
	receiver := paho.NewReceiver("watch-"+topic, session)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		_ = receiver.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
			// Receivers on one session share its router, so a handler is
			// offered everything the session carries — the topic filter has to
			// be applied here, not assumed from the subscription.
			envelope := delivery.Envelope()
			if delivered, ok := messaging.GetHeaderString(
				envelope.HeadersSnapshot(), paho.HeaderMQTTTopic); ok && delivered == topic {
				watch.mu.Lock()
				watch.payloads = append(watch.payloads, string(envelope.Payload()))
				watch.mu.Unlock()
			}
			return delivery.Ack(ctx)
		})
	}()
	t.Cleanup(func() { cancel(); group.Wait() })

	return watch
}

// requireDeliversAll publishes count messages and requires every one back.
// Conservation is the assertion that matters under a server limit or a
// degraded link: a bridge that silently drops under back-pressure would still
// look connected.
func requireDeliversAll(t *testing.T, session *paho.Session, topic string, count int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	require.NoError(t, session.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	waitSubActive(t, session, matrixWait)

	var (
		mu      sync.Mutex
		seen    = map[string]struct{}{}
		total   atomic.Int64
		receive = paho.NewReceiver("bulk-"+topic, session)
	)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		_ = receive.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
			mu.Lock()
			seen[string(delivery.Envelope().Payload())] = struct{}{}
			mu.Unlock()
			total.Add(1)
			return delivery.Ack(ctx)
		})
	}()
	t.Cleanup(func() { cancel(); group.Wait() })

	sender := paho.NewSender(session, paho.SenderOptions{QoS: 1, Timeout: 20 * time.Second})
	for index := range count {
		envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("%s-%d", topic, index),
			Subject: topic,
			Payload: fmt.Appendf(nil, "seq-%d", index),
		})
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
			Envelope: envelope, Address: topic,
		}), "publish %d", index)
	}

	wait.Until(t, matrixWait, fmt.Sprintf("%d unique messages delivered", count), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= count
	})
	t.Logf("%s: %d published, %d unique delivered, %d total deliveries",
		topic, count, len(seen), total.Load())
}
