package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Package-level sinks keep the compiler from eliminating the benchmarked
// calls; they are written only from benchmarks.
//
//nolint:gochecknoglobals // benchmark escape sinks.
var (
	publishSink  *pahov5.Publish
	envelopeSink *messaging.Envelope
	domainSink   []string
)

// ═══════════════════════════════════════════════════════════════════════════
// MQTT ingress-identity benchmarks
//
// publishWithIdentity runs on EVERY inbound publish, inside the Paho callback
// goroutine that must return promptly to keep PINGRESP/PUBACK flowing. These
// pin the cost of each branch: the pass-through for a publish that already
// carries a producer identity, the sanitise-only copy when a publisher sent the
// adapter-reserved marker, and the mint path for an anonymous publish.
// ═══════════════════════════════════════════════════════════════════════════

// benchIngressPublish builds one inbound publish with the given user
// properties and correlation data.
func benchIngressPublish(correlation []byte, user ...pahov5.UserProperty) *pahov5.Publish {
	return &pahov5.Publish{
		Topic:   "sensors/site-1/device-42/telemetry",
		Payload: []byte(`{"t":21.5,"h":48}`),
		Properties: &pahov5.PublishProperties{
			ContentType:     "application/json",
			CorrelationData: correlation,
			User:            user,
		},
	}
}

// BenchmarkPublishWithIdentity measures the ingress provenance decision per
// inbound publish, one sub-benchmark per branch.
func BenchmarkPublishWithIdentity(b *testing.B) {
	cases := []struct {
		name string
		pub  *pahov5.Publish
	}{
		{
			name: "ProducerMessageID",
			pub:  benchIngressPublish(nil, pahov5.UserProperty{Key: HeaderMessageID, Value: "producer-stable-1"}),
		},
		{
			name: "TextCorrelationData",
			pub:  benchIngressPublish([]byte("corr-0f3a9c21")),
		},
		{
			name: "BinaryCorrelationData",
			pub:  benchIngressPublish([]byte{0x00, 0x91, 0xff, 0x3a, 0x00, 0x12}),
		},
		{
			name: "HostileMarkerStripped",
			pub: benchIngressPublish(nil,
				pahov5.UserProperty{Key: HeaderMessageID, Value: "producer-stable-1"},
				pahov5.UserProperty{Key: headerMQTTGeneratedID, Value: "1"},
			),
		},
		{name: "AnonymousMinted", pub: benchIngressPublish(nil)},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				publishSink = publishWithIdentity(tc.pub)
			}
		})
	}
}

// BenchmarkEnvelopeFromPublish_Identity measures the full ingress conversion
// per identity shape, including the header extraction the identity shares with
// correlation-data retention.
func BenchmarkEnvelopeFromPublish_Identity(b *testing.B) {
	cases := []struct {
		name string
		pub  *pahov5.Publish
	}{
		{
			name: "ProducerMessageID",
			pub:  benchIngressPublish(nil, pahov5.UserProperty{Key: HeaderMessageID, Value: "producer-stable-1"}),
		},
		{name: "TextCorrelationData", pub: benchIngressPublish([]byte("corr-0f3a9c21"))},
		{name: "BinaryCorrelationData", pub: benchIngressPublish([]byte{0x00, 0x91, 0xff, 0x3a, 0x00, 0x12})},
		{name: "AnonymousMinted", pub: benchIngressPublish(nil)},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dispatched := publishWithIdentity(tc.pub)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				envelopeSink = EnvelopeFromPublish(dispatched, nil)
			}
		})
	}
}

// BenchmarkDurableSessionIdentityDomains measures broker-endpoint
// canonicalization, which runs per durable session on every preflight and
// every reload comparison.
func BenchmarkDurableSessionIdentityDomains(b *testing.B) {
	cfg := &Config{Session: SessionOptions{
		BrokerURLs:            []string{"MQTT://broker-a.example", "tcp://broker-a.example:1883"},
		ClientID:              "bridge-client",
		SessionExpiryInterval: 3600,
	}}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		domains, err := cfg.DurableSessionIdentityDomains(connectivity.SessionPersistent)
		if err != nil {
			b.Fatalf("DurableSessionIdentityDomains: %v", err)
		}
		domainSink = domains
	}
}
