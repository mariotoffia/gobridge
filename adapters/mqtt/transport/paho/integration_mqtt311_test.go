package paho_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/netfault"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// MQTT 3.1.1 sessions against a real broker.
//
// A session set to protocol_version v3.1.1 runs on the MQTT 5 client with a
// wire translator below the pre-decode guard (ADR 0022). The unit tests pin the
// translation packet by packet; these drive the whole session against Mosquitto,
// which speaks MQTT 3.1.1 and MQTT 5 on one listener, and prove the behaviour
// docs/transports/mqtt-311.md promises: what works as on MQTT 5, and what
// degrades and how.
//
// A 3.1.1 PUBLISH carries no properties, so a delivery has no producer
// mqtt.message-id. Every test here identifies a delivery by its payload.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

// mqtt311Wait bounds every wait on the broker: a delivery, a reconnect, a
// reject. It is the bound of mqtt311Wait and of the in-package brokerWait.
const mqtt311Wait = 30 * time.Second

// TestIntegration_MQTT311_PubSubRoundTrip proves a 3.1.1 subscriber and a 3.1.1
// publisher carry a message at every QoS, and that the delivery reports the QoS
// it was published at: the subscription is granted QoS 2, so the delivered QoS
// is the publisher's.
func TestIntegration_MQTT311_PubSubRoundTrip(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := "mqtt311/roundtrip/" + mqttlocal.UniqueClientID("topic")

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{url},
		ClientID:        mqttlocal.UniqueClientID("mqtt311-pubsub"),
		KeepAlive:       10,
		ConnectTimeout:  5 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	require.NoError(t, sess.Start(ctx), "start the MQTT 3.1.1 subscriber")

	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 2}},
	}))
	waitSubActive(t, sess, 5*time.Second)

	deliveries, _ := recordMQTT311Deliveries(t, sess, "rx-mqtt311-pubsub")
	publisher := startMQTT311Publisher(t, ctx, url, "mqtt311-pubsub-publisher")

	for _, qos := range []byte{0, 1, 2} {
		t.Run(fmt.Sprintf("qos_%d", qos), func(t *testing.T) {
			payload := fmt.Sprintf("hello-mqtt311-qos%d", qos)
			sendMQTT311(t, ctx, publisher, topic, qos, payload)

			got := wait.RequireReceive(t, deliveries, 10*time.Second)
			require.Equal(t, payload, got.payload, "the payload must survive the 3.1.1 hop unchanged")
			require.Equal(t, topic, got.topic)
			require.Equal(t, int(qos), got.qos, "mqtt.qos must be the QoS the message was published at")
		})
	}
}

// TestIntegration_MQTT311_PersistentSessionRedeliversUnsettled proves broker-side
// session resumption on MQTT 3.1.1, where a persistent session sends Clean
// Session 0 instead of a session expiry. Its subscription is established, its
// client goes offline, QoS 1 and QoS 2 messages are queued for it, Mosquitto
// persists them across a restart, and a new Session with the same ClientID
// receives both. A reconnect of that session then shows the broker reported
// Session Present: a durable session that dials to resume and gets Session
// Present 0 counts MQTTSessionResumeLost.
func TestIntegration_MQTT311_PersistentSessionRedeliversUnsettled(t *testing.T) {
	if testing.Short() {
		t.Skip("persistent broker restart integration test")
	}
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clientID := mqttlocal.UniqueClientID("mqtt311-persistent")
	topicPrefix := "mqtt311/session-present/" + mqttlocal.UniqueClientID("topic")
	topicQoS1 := topicPrefix + "/qos1"
	topicQoS2 := topicPrefix + "/qos2"
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: topicQoS1, QoS: 1},
			{Topic: topicQoS2, QoS: 2},
		},
	}
	// A 3.1.1 delivery carries no producer identity, so the payload is the key.
	expected := []string{"queued-qos1", "queued-qos2"}
	accountant, err := prodid.New(expected, false)
	require.NoError(t, err, "new producer accountant")

	// No session expiry: MQTT 3.1.1 cannot send one. A persistent session
	// connects with Clean Session 0 and the broker decides the lifetime.
	first := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{brokerURL},
		ClientID:        clientID,
		KeepAlive:       5,
		ConnectTimeout:  10 * time.Second,
		CleanStart:      false,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	require.NoError(t, first.Start(ctx), "start first persistent session")
	require.NoError(t, first.Reconcile(ctx, plan), "reconcile first persistent session")
	waitSubActive(t, first, 10*time.Second)
	require.NoError(t, first.Close(ctx), "take first persistent session offline")

	publisher := startMQTT311Publisher(t, ctx, brokerURL, "mqtt311-offline-publisher")
	sendMQTT311(t, ctx, publisher, topicQoS1, 1, expected[0])
	sendMQTT311(t, ctx, publisher, topicQoS2, 2, expected[1])
	require.NoError(t, publisher.Close(ctx), "close the offline-queue publisher")
	broker.StopGraceful()
	broker.RestartGraceful()

	metrics := &ports.RecordingExporter{}
	second := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{brokerURL},
		ClientID:        clientID,
		KeepAlive:       5,
		ConnectTimeout:  10 * time.Second,
		CleanStart:      false,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	require.NoError(t, second.Start(ctx), "resume persistent session")
	require.NoError(t, second.Reconcile(ctx, plan), "reconcile resumed persistent session")

	deliveries, _ := recordMQTT311Deliveries(t, second, "mqtt311-resumed-receiver")
	wait.Until(t, mqtt311Wait, "queued QoS 1/2 delivery from resumed broker session", func() bool {
		for {
			select {
			case delivery := <-deliveries:
				accountant.ObserveOutput(delivery.payload, delivery.id)
			default:
				return len(accountant.Reconcile().Missing) == 0
			}
		}
	})

	health := second.Health(ctx)
	if !health.Connected || health.SubscriptionsSatisfied == nil || !*health.SubscriptionsSatisfied {
		t.Fatalf("resumed session health does not show current reconcile evidence: %+v", health)
	}
	if health.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("resumed session service level = %s, want %s", health.ServiceLevel, ports.ServiceLevelFull)
	}
	report := accountant.Reconcile()
	if !report.Exact() || len(report.DLQ) != 0 || len(report.IntentionallyDropped) != 0 {
		t.Fatalf("queued persistent-session accounting failed: %s", report.String())
	}

	// The first connect of a Session object expects no resumption, so it cannot
	// tell Session Present 0 from 1. A reconnect of the same session dials to
	// resume, and only Session Present 1 leaves MQTTSessionResumeLost at zero.
	require.NoError(t, second.Reload(ctx), "reconnect the resumed session")
	require.Empty(t, metrics.FindEntries(paho.MetricMQTTSessionResumeLost),
		"the broker must report Session Present on a Clean Session 0 reconnect")
	require.NoError(t, second.Health(ctx).LastError,
		"a resumed session latches no resume-loss error")
}

// TestIntegration_MQTT311_OversizedPublishIsAckedAndDropped pins the oversize
// path MQTT 3.1.1 forces: the session cannot announce a Maximum Packet Size, so
// the broker forwards a publish of any size. The translator hands up the head of
// the payload, the router acks and drops it as poison, and the connection stays
// up: no ingress reject, and a later publish flows on the same connection. A
// fresh session with the same ClientID then never receives the oversized
// publish, so the ack freed the broker's copy instead of parking it for the
// next resume.
func TestIntegration_MQTT311_OversizedPublishIsAckedAndDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const maxPayloadBytes = 1024
	topic := "mqtt311/oversize/" + mqttlocal.UniqueClientID("topic")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)

	// One message in flight per client: the broker sends the next QoS 1 publish
	// only after the PUBACK of the previous one. The small publish arriving
	// therefore proves the broker already holds the oversized publish's ack,
	// which a deliberate close could otherwise race.
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithMaxInflightMessages(1))
	t.Cleanup(broker.Stop)

	clientID := mqttlocal.UniqueClientID("mqtt311-oversize-source")
	metrics := &ports.RecordingExporter{}
	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{broker.URL()},
		ClientID:        clientID,
		ConnectTimeout:  10 * time.Second,
		KeepAlive:       30,
		CleanStart:      false,
		MaxPayloadBytes: maxPayloadBytes,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	received, stopReceiver := recordMQTT311Deliveries(t, source, "mqtt311-oversize", topic)

	publisher := startMQTT311Publisher(t, ctx, broker.URL(), "mqtt311-oversize-publisher")
	oversized := string(make([]byte, 64<<10))
	sendMQTT311(t, ctx, publisher, topic, 1, oversized)
	wait.Until(t, mqtt311Wait, "oversized payload acked and dropped on the poison counter", func() bool {
		return len(metrics.FindEntries(paho.MetricMQTTIngressPoisonDropped)) >= 1
	})

	sendMQTT311(t, ctx, publisher, topic, 1, "small")
	got := wait.RequireReceive(t, received, mqtt311Wait)
	require.Equal(t, "small", got.payload, "traffic after a poison drop must flow on the same connection")
	require.Empty(t, metrics.FindEntries(paho.MetricMQTTIngressRejected),
		"an oversized PUBLISH is a poison drop, never an ingress reject that drops the connection")
	health := source.Health(ctx)
	require.True(t, health.Ready, "a poison publish must not take the session down")
	require.NoError(t, health.LastError)

	stopReceiver()
	require.NoError(t, source.Close(ctx), "take the source session offline")

	// The fresh session admits 64 KiB, so a redelivered oversized publish would
	// reach its receiver instead of being dropped again.
	freshMetrics := &ports.RecordingExporter{}
	fresh := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{broker.URL()},
		ClientID:        clientID,
		ConnectTimeout:  10 * time.Second,
		KeepAlive:       30,
		CleanStart:      false,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionPersistent, nil, freshMetrics)
	t.Cleanup(func() { _ = fresh.Close(context.Background()) })
	require.NoError(t, fresh.Start(ctx))
	require.NoError(t, fresh.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	resumed, _ := recordMQTT311Deliveries(t, fresh, "mqtt311-oversize-fresh", topic)

	sendMQTT311(t, ctx, publisher, topic, 1, "sentinel")
	for {
		got := wait.RequireReceive(t, resumed, mqtt311Wait)
		require.LessOrEqual(t, len(got.payload), maxPayloadBytes,
			"the oversized publish was acked; the broker must never deliver it again")
		if got.payload == "sentinel" {
			break
		}
		// The PUBACK of the settled small publish leaves on Paho's ack tick and
		// can race the deliberate close: that redelivery is the documented
		// at-least-once residual. Nothing else may precede the sentinel.
		require.Equal(t, "small", got.payload,
			"only the sentinel, after a possible redelivery of settled traffic, may arrive")
	}
	require.Empty(t, freshMetrics.FindEntries(paho.MetricMQTTIngressPoisonDropped),
		"the fresh session must not have been sent the oversized publish at all")
}

// TestIntegration_MQTT311_CredentialFailureSurfacesNotAuthorized pins what an
// operator sees when a 3.1.1 broker refuses a password: CONNACK return code 4
// or 5, which the translator maps to the MQTT 5 reason code of the same
// meaning, so the refusal is classified ErrNotAuthorized and drives the same
// credential re-resolve as on MQTT 5.
func TestIntegration_MQTT311_CredentialFailureSurfacesNotAuthorized(t *testing.T) {
	requireDockerBroker(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(secureUser, securePassword),
		mqttlocal.WithTLS(),
	)

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{broker.TLSURL()},
		ClientID:         mqttlocal.UniqueClientID("mqtt311-bad-credential"),
		KeepAlive:        10,
		ConnectTimeout:   2 * time.Second,
		ReconnectDelay:   50 * time.Millisecond,
		ReconnectTimeout: 250 * time.Millisecond,
		CleanStart:       true,
		Username:         secureUser,
		Password:         shared.NewSecret("not-the-password"),
		ProtocolVersion:  paho.ProtocolVersion311,
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
	// Start reports only that no connection came up, on MQTT 5 as on 3.1.1. The
	// classified refusal reaches the auth-failure callback and Health.
	require.Error(t, session.Start(ctx), "a wrong password must not produce a live session")

	err := wait.RequireReceive(t, denied, secureConnect)
	require.ErrorIs(t, err, shared.ErrNotAuthorized,
		"a refused 3.1.1 credential must be classified, not reported as a transport blip")
	require.ErrorIs(t, session.Health(t.Context()).LastError, shared.ErrNotAuthorized,
		"Health must name the refused credential as the reason the session is down")
}

// TestIntegration_MQTT311_RefusedSubscriptionFailsReconcile drives the one
// SUBACK failure MQTT 3.1.1 has, 0x80, from a broker whose ACL refuses a filter.
// The reconcile fails, classified UNAVAILABLE as the 3.1.1 code carries no
// reason, and the filter the broker granted in the same SUBSCRIBE stays active
// and delivers.
func TestIntegration_MQTT311_RefusedSubscriptionFailsReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	denied := "mqtt311/acl/denied/" + mqttlocal.UniqueClientID("topic")
	permitted := "mqtt311/acl/permitted/" + mqttlocal.UniqueClientID("topic")
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithACL(mqttlocal.ACL{
		DeniedSubscriptions: []string{denied},
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{broker.URL()},
		ClientID:        mqttlocal.UniqueClientID("mqtt311-acl"),
		KeepAlive:       10,
		ConnectTimeout:  5 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx))

	err := session.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: denied, QoS: 1},
			{Topic: permitted, QoS: 1},
		},
	})
	require.Error(t, err, "a filter the broker refused must fail the reconcile")
	require.ErrorIs(t, err, shared.ErrUnavailable,
		"SUBACK 0x80 keeps its MQTT 5 meaning: unspecified and transient")

	health := session.Health(ctx)
	require.True(t, health.HasTopic(permitted), "the granted filter must stay active: %v", health.ActiveTopics)
	require.False(t, health.HasTopic(denied), "the refused filter must not read as active: %v", health.ActiveTopics)

	deliveries, _ := recordMQTT311Deliveries(t, session, "rx-mqtt311-acl")
	publisher := startMQTT311Publisher(t, ctx, broker.URL(), "mqtt311-acl-publisher")
	sendMQTT311(t, ctx, publisher, denied, 1, "to-denied")
	sendMQTT311(t, ctx, publisher, permitted, 1, "to-permitted")

	// Both publishes leave one connection in order, so a delivery on the refused
	// filter would arrive before the permitted one.
	got := wait.RequireReceive(t, deliveries, mqtt311Wait)
	require.Equal(t, permitted, got.topic, "nothing may arrive on the refused filter")
	require.Equal(t, "to-permitted", got.payload)
}

// TestIntegration_MQTT311_LastWill proves the Last Will on MQTT 3.1.1, where
// will properties do not exist: a connection that ends without DISCONNECT
// publishes the will, and a graceful Close, which sends DISCONNECT, does not.
func TestIntegration_MQTT311_LastWill(t *testing.T) {
	requireDockerBroker(t)
	mqtt311 := func(o *paho.SessionOptions) { o.ProtocolVersion = paho.ProtocolVersion311 }

	t.Run("ungraceful_death_publishes_the_will", func(t *testing.T) {
		broker := mqttlocal.NewBrokerInstance(t)
		willTopic := "mqtt311/will/" + mqttlocal.UniqueClientID("node")

		observer := newSecureSession(t, broker.URL(), "mqtt311-will-observer", mqtt311)
		wills := watchTopic(t, observer, willTopic)

		// The dying session reaches the broker through a fault injector so its
		// connection can be severed without a DISCONNECT packet.
		link := netfault.Start(t, hostPortOf(t, broker.URL()))
		dying := newSecureSession(t, link.URL("tcp"), "mqtt311-will-node", func(o *paho.SessionOptions) {
			mqtt311(o)
			o.KeepAlive = 2
			o.Will = &paho.WillOptions{Topic: willTopic, Payload: "node-died", QoS: 1}
		})

		link.Cut()

		wait.Until(t, mqtt311Wait, "broker published the will", func() bool {
			return wills.count() > 0
		})
		require.Equal(t, "node-died", wills.first(),
			"the will payload the operator configured is what peers must receive")

		_ = dying.Close(context.Background())
	})

	t.Run("graceful_close_suppresses_the_will", func(t *testing.T) {
		broker := mqttlocal.NewBrokerInstance(t)
		willTopic := "mqtt311/will-graceful/" + mqttlocal.UniqueClientID("node")

		observer := newSecureSession(t, broker.URL(), "mqtt311-graceful-observer", mqtt311)
		wills := watchTopic(t, observer, willTopic)

		leaving := newSecureSession(t, broker.URL(), "mqtt311-graceful-node", func(o *paho.SessionOptions) {
			mqtt311(o)
			o.Will = &paho.WillOptions{Topic: willTopic, Payload: "should-not-appear", QoS: 1}
		})
		require.NoError(t, leaving.Close(context.Background()))

		// The broker handles the DISCONNECT before a publish sent after Close
		// returned, and delivers in order to one subscriber: a will it was going
		// to publish would reach the observer before this sentinel.
		ctx, cancel := context.WithTimeout(t.Context(), mqtt311Wait)
		defer cancel()
		publisher := startMQTT311Publisher(t, ctx, broker.URL(), "mqtt311-graceful-publisher")
		sendMQTT311(t, ctx, publisher, willTopic, 1, "published-after-close")

		wait.Until(t, mqtt311Wait, "sentinel published after the graceful close", func() bool {
			return wills.count() > 0
		})
		require.Equal(t, []string{"published-after-close"}, wills.all(),
			"a graceful DISCONNECT must not trigger the will")
	})
}

// TestIntegration_MQTT311_HeadersAreNotCarried pins what an MQTT 3.1.1
// subscriber receives from an MQTT 5 publisher that set a header, a subject and
// an envelope ID. The test needs an MQTT 5 publisher: only MQTT 5 can send them.
// An MQTT 5 control subscriber proves they were on the wire; the 3.1.1
// subscriber gets the topic, the QoS and the RETAIN flag, and an identity the
// adapter minted and marks as minted.
func TestIntegration_MQTT311_HeadersAreNotCarried(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := "mqtt311/headers/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}}}

	legacy := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{url},
		ClientID:        mqttlocal.UniqueClientID("mqtt311-headers"),
		KeepAlive:       10,
		ConnectTimeout:  5 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = legacy.Close(context.Background()) })
	require.NoError(t, legacy.Start(ctx))
	require.NoError(t, legacy.Reconcile(ctx, plan))
	waitSubActive(t, legacy, 5*time.Second)

	control := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("mqtt311-headers-control"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	require.NoError(t, control.Start(ctx))
	require.NoError(t, control.Reconcile(ctx, plan))
	waitSubActive(t, control, 5*time.Second)

	legacyDeliveries, _ := recordMQTT311Deliveries(t, legacy, "rx-mqtt311-headers")
	controlDeliveries, _ := recordMQTT311Deliveries(t, control, "rx-mqtt5-headers")

	publisher := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("mqtt5-headers-publisher"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx))

	producerID := mqttlocal.UniqueClientID("producer-envelope")
	const subject = "mqtt311.headers.subject"
	envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      producerID,
		Subject: subject,
		Payload: []byte("headers-probe"),
		Headers: map[string]any{"app": "1"},
	})
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{Envelope: envelope, Address: topic}))

	v5 := wait.RequireReceive(t, controlDeliveries, mqtt311Wait)
	require.Equal(t, producerID, v5.id, "control: an MQTT 5 subscriber keeps the producer's envelope ID")
	require.Equal(t, subject, v5.subject, "control: an MQTT 5 subscriber keeps the subject")
	require.Equal(t, "1", v5.headers["app"], "control: an MQTT 5 subscriber keeps the header")

	got := wait.RequireReceive(t, legacyDeliveries, mqtt311Wait)
	require.Equal(t, "headers-probe", got.payload)
	require.NotEmpty(t, got.id)
	require.NotEqual(t, producerID, got.id, "the producer's envelope ID cannot cross a 3.1.1 hop")
	require.Empty(t, got.subject, "the subject cannot cross a 3.1.1 hop")
	require.Equal(t, map[string]any{
		paho.HeaderMQTTTopic:        topic,
		paho.HeaderMQTTQoS:          1,
		paho.HeaderMQTTRetained:     false,
		messaging.HeaderGeneratedID: "true",
		paho.HeaderMessageID:        got.id,
	}, got.headers, "only what the 3.1.1 PUBLISH carries, plus the minted identity, may arrive")
}

// TestIntegration_MQTT311_ContentHashIDSurvivesRedelivery proves message_id
// content_hash against a real broker. A persistent MQTT 3.1.1 session retries a
// QoS 1 delivery, which recycles the connection, and the broker redelivers the
// message on the resumed session. The redelivery carries the same envelope ID
// and is not marked adapter-minted, so the runtime's replay ledger can count it
// and the outbox can deduplicate it (ADR 0022).
func TestIntegration_MQTT311_ContentHashIDSurvivesRedelivery(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	topic := "mqtt311/content-hash/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}}}

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{url},
		ClientID:        mqttlocal.UniqueClientID("mqtt311-content-hash"),
		KeepAlive:       10,
		ConnectTimeout:  10 * time.Second,
		CleanStart:      false,
		ProtocolVersion: paho.ProtocolVersion311,
		MessageID:       paho.MessageIDContentHash,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	require.NoError(t, sess.Start(ctx))
	require.NoError(t, sess.Reconcile(ctx, plan))
	waitSubActive(t, sess, 10*time.Second)

	// Deliveries are keyed by payload: the first of a payload is retried, every
	// later one is acked. A delivery is forwarded once it is settled.
	deliveries := make(chan mqtt311Delivery, 8)
	var mu sync.Mutex
	seen := make(map[string]int)
	receiver := paho.NewReceiver("rx-mqtt311-content-hash", sess, paho.WithTopicFilters(topic))
	runCtx, stopReceiver := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(ctx context.Context, delivery ports.Delivery) error {
			envelope := delivery.Envelope()
			payload := string(envelope.Payload())
			mu.Lock()
			seen[payload]++
			first := seen[payload] == 1
			mu.Unlock()
			settle := delivery.Ack
			if first {
				settle = func(ctx context.Context) error { return delivery.Retry(ctx, 0, shared.ErrUnavailable) }
			}
			if err := settle(ctx); err != nil {
				return fmt.Errorf("settle %q: %w", payload, err)
			}
			select {
			case deliveries <- mqtt311Delivery{id: envelope.ID(), payload: payload, headers: envelope.HeadersSnapshot()}:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	t.Cleanup(func() {
		stopReceiver()
		if err := wait.RequireReceive(t, runDone, 10*time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("receiver stopped with %v, want context.Canceled", err)
		}
	})
	wait.RequireClosed(t, receiver.Started(), 10*time.Second)

	payload := mqttlocal.UniqueClientID("content-hash-payload")
	publisher := startMQTT311Publisher(t, ctx, url, "mqtt311-content-hash-publisher")
	sendMQTT311(t, ctx, publisher, topic, 1, payload)

	first := wait.RequireReceive(t, deliveries, mqtt311Wait)
	redelivered := wait.RequireReceive(t, deliveries, mqtt311Wait)
	require.Equal(t, payload, first.payload)
	require.Equal(t, payload, redelivered.payload, "one message was published, so a second delivery is its redelivery")
	require.True(t, strings.HasPrefix(first.id, "mqtt-sha256:"), "envelope id %q is not a content hash", first.id)
	require.Equal(t, first.id, redelivered.id, "a content-hash id must survive the broker's redelivery")
	for _, delivery := range []mqtt311Delivery{first, redelivered} {
		require.NotContains(t, delivery.headers, messaging.HeaderGeneratedID,
			"a content-hash id is stable, so it must not be marked adapter-minted")
	}
}

// TestIntegration_MQTT311_UnsubscribeConverges proves the UNSUBACK the
// translator synthesises — a 3.1.1 UNSUBACK has no reason codes — lets a
// reconcile that removes a filter converge, and that the broker really stopped
// delivering on it.
func TestIntegration_MQTT311_UnsubscribeConverges(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prefix := "mqtt311/unsubscribe/" + mqttlocal.UniqueClientID("topic")
	topicA := prefix + "/a"
	topicB := prefix + "/b"

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{url},
		ClientID:        mqttlocal.UniqueClientID("mqtt311-unsubscribe"),
		KeepAlive:       10,
		ConnectTimeout:  5 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	require.NoError(t, sess.Start(ctx))

	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topicA, QoS: 1}, {Topic: topicB, QoS: 1}},
	}), "Reconcile {A, B}")
	waitSubActive(t, sess, 5*time.Second)

	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topicA, QoS: 1}},
	}), "Reconcile {A} must converge on the synthesised UNSUBACK")
	waitSubActive(t, sess, 5*time.Second)
	health := sess.Health(ctx)
	require.Equal(t, []string{topicA}, health.ActiveTopics)
	require.NotNil(t, health.SubscriptionsSatisfied)
	require.True(t, *health.SubscriptionsSatisfied, "the reduced plan must read as converged")

	// No topic filter: the receiver sees whatever the broker still delivers.
	deliveries, _ := recordMQTT311Deliveries(t, sess, "rx-mqtt311-unsubscribe")
	publisher := startMQTT311Publisher(t, ctx, url, "mqtt311-unsubscribe-publisher")
	sendMQTT311(t, ctx, publisher, topicB, 1, "to-b")
	sendMQTT311(t, ctx, publisher, topicA, 1, "to-a")

	// Both QoS 1 publishes leave one connection in order, so a delivery on the
	// removed filter would arrive before the one on the kept filter.
	got := wait.RequireReceive(t, deliveries, mqtt311Wait)
	require.Equal(t, topicA, got.topic, "the removed filter must not deliver")
	require.Equal(t, "to-a", got.payload)
}

// TestIntegration_MQTT311_SharedSubscription proves two MQTT 3.1.1 consumers of
// one $share group split a stream without duplication: every message reaches
// exactly one of them, and both get work.
func TestIntegration_MQTT311_SharedSubscription(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	baseTopic := "mqtt311/shared-sub/" + mqttlocal.UniqueClientID("topic")
	shareTopic := "$share/" + mqttlocal.UniqueClientID("mqtt311group") + "/" + baseTopic
	const msgCount = 20

	consumer := func(prefix string) <-chan mqtt311Delivery {
		sess := paho.NewSession(paho.SessionOptions{
			BrokerURLs:      []string{brokerURL},
			ClientID:        mqttlocal.UniqueClientID(prefix),
			KeepAlive:       10,
			ConnectTimeout:  5 * time.Second,
			CleanStart:      true,
			ProtocolVersion: paho.ProtocolVersion311,
		}, connectivity.SessionEphemeral, nil)
		t.Cleanup(func() { _ = sess.Close(context.Background()) })
		require.NoError(t, sess.Start(ctx), "Start (%s)", prefix)
		require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: shareTopic, QoS: 1}},
		}), "Reconcile (%s)", prefix)
		waitSubActive(t, sess, 5*time.Second)
		deliveries, _ := recordMQTT311Deliveries(t, sess, "rx-"+prefix)
		return deliveries
	}
	deliveriesA := consumer("mqtt311-share-a")
	deliveriesB := consumer("mqtt311-share-b")

	publisher := startMQTT311Publisher(t, ctx, brokerURL, "mqtt311-share-publisher")
	for i := range msgCount {
		sendMQTT311(t, ctx, publisher, baseTopic, 1, fmt.Sprintf("shared-msg-%d", i))
	}

	seenA, seenB := map[string]int{}, map[string]int{}
	countA, countB := 0, 0
	wait.Until(t, mqtt311Wait, fmt.Sprintf("%d distinct messages across the group", msgCount), func() bool {
		for {
			select {
			case delivery := <-deliveriesA:
				seenA[delivery.payload]++
				countA++
			case delivery := <-deliveriesB:
				seenB[delivery.payload]++
				countB++
			default:
				distinct := make(map[string]struct{}, msgCount)
				for payload := range seenA {
					distinct[payload] = struct{}{}
				}
				for payload := range seenB {
					distinct[payload] = struct{}{}
				}
				return len(distinct) >= msgCount
			}
		}
	})

	require.Equal(t, msgCount, countA+countB,
		"every message must reach exactly one consumer (A=%d, B=%d)", countA, countB)
	for payload := range seenA {
		require.NotContains(t, seenB, payload, "both consumers received %q", payload)
	}
	require.Positive(t, countA, "consumer A got no work: the group did not split the stream")
	require.Positive(t, countB, "consumer B got no work: the group did not split the stream")
}

// TestIntegration_MQTT311_BrokerWindowAboveReceiveMaximumIsRefusedNotWedged
// pins the in-flight window MQTT 3.1.1 cannot negotiate. A 3.1.1 CONNECT cannot
// announce receive_maximum, so Mosquitto sends up to its own
// max_inflight_messages (20 by default). A session with receive_maximum 4 must
// refuse the fifth unacknowledged publish as an ingress reject on every
// connection, keep redialling, and keep Close bounded, instead of parking its
// reader on a full window. Nothing is acked on the way: a session whose window
// fits then receives every message.
func TestIntegration_MQTT311_BrokerWindowAboveReceiveMaximumIsRefusedNotWedged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const (
		narrowWindow = 4
		wideWindow   = 32
		queued       = 20 // Mosquitto's default max_inflight_messages
	)
	broker := mqttlocal.NewBrokerInstance(t)
	brokerURL := broker.URL()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clientID := mqttlocal.UniqueClientID("mqtt311-window")
	topic := "mqtt311/window/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}}}
	options := func(receiveMaximum uint16) paho.SessionOptions {
		return paho.SessionOptions{
			BrokerURLs:     []string{brokerURL},
			ClientID:       clientID,
			KeepAlive:      5,
			ConnectTimeout: 10 * time.Second,
			// A short reconnect base keeps the reject backoff inside the wait.
			ReconnectDelay:    200 * time.Millisecond,
			ReconnectMaxDelay: time.Second,
			CleanStart:        false,
			ReceiveMaximum:    receiveMaximum,
			ProtocolVersion:   paho.ProtocolVersion311,
		}
	}

	first := paho.NewSession(options(narrowWindow), connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	require.NoError(t, first.Start(ctx), "start first persistent session")
	require.NoError(t, first.Reconcile(ctx, plan), "reconcile first persistent session")
	waitSubActive(t, first, 10*time.Second)
	require.NoError(t, first.Close(ctx), "take first persistent session offline")

	expected := make([]string, 0, queued)
	publisher := startMQTT311Publisher(t, ctx, brokerURL, "mqtt311-window-publisher")
	for i := range queued {
		payload := fmt.Sprintf("window-%02d", i)
		expected = append(expected, payload)
		sendMQTT311(t, ctx, publisher, topic, 1, payload)
	}

	// No receiver: nothing is acked, so the broker's window fills at once.
	narrowMetrics := &ports.RecordingExporter{}
	narrow := paho.NewSession(options(narrowWindow), connectivity.SessionPersistent, nil, narrowMetrics)
	t.Cleanup(func() { _ = narrow.Close(context.Background()) })
	require.NoError(t, narrow.Start(ctx), "resume with a window below the broker's")
	wait.Until(t, mqtt311Wait, "the publish beyond receive_maximum refused as an ingress reject", func() bool {
		return len(narrowMetrics.FindEntries(paho.MetricMQTTIngressRejected)) >= 1
	})
	// A session whose reader parked on a full window would stay "connected" and
	// never reconnect. This one drops the connection, redials, and is refused
	// again, because the broker resends the same unacknowledged window.
	wait.Until(t, mqtt311Wait, "the session redialled and was refused again instead of wedging", func() bool {
		return len(narrowMetrics.FindEntries(paho.MetricMQTTIngressRejected)) >= 2
	})

	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(closeCancel)
	closed := make(chan error, 1)
	go func() { closed <- narrow.Close(closeCtx) }()
	require.NoError(t, wait.RequireReceive(t, closed, 10*time.Second),
		"Close must return promptly: a window the broker overran must not wedge the session")

	wideMetrics := &ports.RecordingExporter{}
	wide := paho.NewSession(options(wideWindow), connectivity.SessionPersistent, nil, wideMetrics)
	t.Cleanup(func() { _ = wide.Close(context.Background()) })
	require.NoError(t, wide.Start(ctx), "resume with a window that fits the broker's")
	require.NoError(t, wide.Reconcile(ctx, plan), "reconcile the resumed session")
	deliveries, _ := recordMQTT311Deliveries(t, wide, "mqtt311-window-receiver", topic)

	seen := map[string]struct{}{}
	wait.Until(t, mqtt311Wait, fmt.Sprintf("all %d queued messages delivered", queued), func() bool {
		for {
			select {
			case delivery := <-deliveries:
				seen[delivery.payload] = struct{}{}
			default:
				return len(seen) >= queued
			}
		}
	})
	got := make([]string, 0, len(seen))
	for payload := range seen {
		got = append(got, payload)
	}
	sort.Strings(got)
	require.Equal(t, expected, got, "the refused connection must have acked and lost nothing")
	require.Empty(t, wideMetrics.FindEntries(paho.MetricMQTTIngressRejected),
		"a receive_maximum at least the broker's in-flight limit is never overrun")
}

// TestIntegration_MQTT311_ResumedSessionDoesNotReplayRetained pins what Session
// Present buys a 3.1.1 session, which cannot ask for Retain Handling: every
// SUBSCRIBE makes the broker send the filter's retained messages again. A
// persistent session gets the retained message once when it subscribes. Its
// connection then drops and resumes with Session Present 1, the broker still
// holds the subscription, and the reconcile sends no SUBSCRIBE, so the retained
// message does not come back while live traffic still arrives. The control: the
// broker then loses the session, the reconnect gets Session Present 0, and the
// session subscribes again and receives the retained message, as a fresh
// subscriber does.
func TestIntegration_MQTT311_ResumedSessionDoesNotReplayRetained(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	t.Cleanup(cancel)

	// One message in flight per client: the broker sends the next QoS 1 publish
	// only after it holds the PUBACK of the previous one. A delivery therefore
	// proves the broker has the ack of the one before it, so a drop cannot make
	// the broker redeliver that one.
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithMaxInflightMessages(1))
	link := netfault.Start(t, hostPortOf(t, broker.URL()))

	clientID := mqttlocal.UniqueClientID("mqtt311-retained-resume")
	topic := "mqtt311/retained-resume/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}}}

	publisher := startMQTT311Publisher(t, ctx, broker.URL(), "mqtt311-retained-resume-publisher")
	retainMQTT311(t, ctx, publisher, topic, "retained-state")

	metrics := &ports.RecordingExporter{}
	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:        []string{link.URL("tcp")},
		ClientID:          clientID,
		KeepAlive:         5,
		ConnectTimeout:    10 * time.Second,
		ReconnectDelay:    200 * time.Millisecond,
		ReconnectMaxDelay: time.Second,
		ReconnectTimeout:  2 * time.Second,
		CleanStart:        false,
		ProtocolVersion:   paho.ProtocolVersion311,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx), "start the persistent session")
	pump := startReconcilePump(ctx, session, plan)
	pump.waitCount(t, ports.SessionReconciled, 1, mqtt311Wait, "first reconcile")
	deliveries, _ := recordMQTT311Deliveries(t, session, "mqtt311-retained-resume", topic)

	first := wait.RequireReceive(t, deliveries, mqtt311Wait)
	require.Equal(t, "retained-state", first.payload)
	require.Equal(t, true, first.headers[paho.HeaderMQTTRetained], "a new subscription gets the retained message")
	sendMQTT311(t, ctx, publisher, topic, 1, "before-drop")
	require.Equal(t, "before-drop", wait.RequireReceive(t, deliveries, mqtt311Wait).payload)

	// The connection drops without a DISCONNECT and resumes the session.
	link.Cut()
	pump.waitCount(t, ports.SessionDisconnected, 1, mqtt311Wait, "the drop")
	link.Heal()
	pump.waitCount(t, ports.SessionConnected, 2, mqtt311Wait, "the resumed connection")
	pump.waitCount(t, ports.SessionReconciled, 2, mqtt311Wait, "the reconcile of the resumed connection")
	require.Empty(t, metrics.FindEntries(paho.MetricMQTTSessionResumeLost),
		"the broker must answer Session Present 1")

	// A retained message sent for a SUBSCRIBE in that reconcile would be queued
	// before this publish, which is sent after the reconcile finished.
	sendMQTT311(t, ctx, publisher, topic, 1, "after-resume")
	for {
		got := wait.RequireReceive(t, deliveries, mqtt311Wait)
		require.Equal(t, false, got.headers[paho.HeaderMQTTRetained],
			"a resumed session must not be sent the retained message again (got %q)", got.payload)
		if got.payload == "after-resume" {
			break
		}
		// Its PUBACK can race the drop; that redelivery is ordinary at-least-once.
		require.Equal(t, "before-drop", got.payload, "only a redelivery may precede the live publish")
	}

	// Control: the broker loses the session while the connection is down. A
	// client connecting with the same client id and Clean Session 1 makes the
	// broker discard it (MQTT 3.1.1 §3.1.2.4).
	link.Cut()
	pump.waitCount(t, ports.SessionDisconnected, 2, mqtt311Wait, "the second drop")
	wiper := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{broker.URL()},
		ClientID:        clientID,
		KeepAlive:       10,
		ConnectTimeout:  10 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	require.NoError(t, wiper.Start(ctx), "discard the broker session")
	require.NoError(t, wiper.Close(ctx))
	link.Heal()
	pump.waitCount(t, ports.SessionConnected, 3, mqtt311Wait, "the connection after the loss")
	pump.waitCount(t, ports.SessionReconciled, 3, mqtt311Wait, "the reconcile after the loss")
	require.Len(t, metrics.FindEntries(paho.MetricMQTTSessionResumeLost), 1,
		"the broker must answer Session Present 0")

	sendMQTT311(t, ctx, publisher, topic, 1, "after-loss")
	replayed := wait.RequireReceive(t, deliveries, mqtt311Wait)
	require.Equal(t, "retained-state", replayed.payload,
		"a session the broker lost subscribes again and gets the retained message first")
	require.Equal(t, true, replayed.headers[paho.HeaderMQTTRetained])
	require.Equal(t, "after-loss", wait.RequireReceive(t, deliveries, mqtt311Wait).payload)
}

// ---------------------------------------------------------------------------
// MQTT 3.1.1 helpers
// ---------------------------------------------------------------------------

// mqtt311Delivery is what a receiver's handler saw of one delivery.
type mqtt311Delivery struct {
	id      string
	subject string
	payload string
	topic   string
	qos     int
	headers map[string]any
}

// recordMQTT311Deliveries runs a Receiver on session that acknowledges every
// delivery and then forwards it on the returned channel, so a delivery read
// from the channel has been settled. Without filters the receiver sees every
// publish the session carries. The handler is registered before this returns.
// stop ends the receiver and is idempotent; it also runs when the test ends.
func recordMQTT311Deliveries(
	t *testing.T, session *paho.Session, receiverID string, filters ...string,
) (<-chan mqtt311Delivery, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	delivered := make(chan mqtt311Delivery, 64)
	var options []paho.ReceiverOption
	if len(filters) > 0 {
		options = append(options, paho.WithTopicFilters(filters...))
	}
	receiver := paho.NewReceiver(receiverID, session, options...)
	stopped := make(chan error, 1)
	go func() {
		stopped <- receiver.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
			envelope := delivery.Envelope()
			headers := envelope.HeadersSnapshot()
			topic, _ := headers[paho.HeaderMQTTTopic].(string)
			qos, _ := headers[paho.HeaderMQTTQoS].(int)
			if err := delivery.Ack(ctx); err != nil {
				return fmt.Errorf("ack %q: %w", envelope.Payload(), err)
			}
			select {
			case delivered <- mqtt311Delivery{
				id:      envelope.ID(),
				subject: envelope.Subject(),
				payload: string(envelope.Payload()),
				topic:   topic,
				qos:     qos,
				headers: headers,
			}:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			// Canceled, not an emit error: an Ack that failed would have stopped
			// the receiver early, and every delivery after it would go unseen.
			err := wait.RequireReceive(t, stopped, 5*time.Second)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("receiver %s stopped with %v, want context.Canceled", receiverID, err)
			}
		})
	}
	t.Cleanup(stop)
	wait.RequireClosed(t, receiver.Started(), 5*time.Second)
	return delivered, stop
}

// startMQTT311Publisher starts an ephemeral MQTT 3.1.1 session to publish from,
// the way any other 3.1.1 client of the broker would. It closes when the test
// ends.
func startMQTT311Publisher(t *testing.T, ctx context.Context, brokerURL, prefix string) *paho.Session {
	t.Helper()
	publisher := paho.NewSession(paho.SessionOptions{
		BrokerURLs:      []string{brokerURL},
		ClientID:        mqttlocal.UniqueClientID(prefix),
		KeepAlive:       10,
		ConnectTimeout:  10 * time.Second,
		CleanStart:      true,
		ProtocolVersion: paho.ProtocolVersion311,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx), "start the publisher %s", prefix)
	return publisher
}

// sendMQTT311 publishes payload to topic at qos from publisher. At QoS 1 and 2
// it returns once the broker has acknowledged the publish.
func sendMQTT311(t *testing.T, ctx context.Context, publisher *paho.Session, topic string, qos byte, payload string) {
	t.Helper()
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: qos, Timeout: 10 * time.Second})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: []byte(payload)}),
		Address:  topic,
	}), "publish to %s at QoS %d", topic, qos)
}

// retainMQTT311 publishes payload to topic at QoS 1 with the RETAIN flag, and
// returns once the broker has stored it.
func retainMQTT311(t *testing.T, ctx context.Context, publisher *paho.Session, topic, payload string) {
	t.Helper()
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Retain: true, Timeout: 10 * time.Second})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: []byte(payload)}),
		Address:  topic,
	}), "publish a retained message to %s", topic)
}
