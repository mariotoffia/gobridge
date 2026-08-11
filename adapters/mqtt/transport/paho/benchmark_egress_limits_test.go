package paho

import (
	"context"
	"io"
	"strings"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// benchmarkPublish builds a realistic egress packet: a typical set of
// bridge-to-bridge user properties, a correlation id and a 4 KiB body.
func benchmarkPublish() *pahov5.Publish {
	return &pahov5.Publish{
		Topic:    "bench/egress/topic",
		QoS:      1,
		PacketID: 7,
		Payload:  make([]byte, 4096),
		Properties: &pahov5.PublishProperties{
			ContentType:     "application/json",
			CorrelationData: []byte("bench-correlation-id"),
			User: []pahov5.UserProperty{
				{Key: HeaderMessageID, Value: "bench-id-123"},
				{Key: HeaderGobridgeSubject, Value: "bench/topic"},
				{Key: "x-tenant-id", Value: "tenant-42"},
				{Key: "x-app-note", Value: "bench"},
			},
		},
	}
}

// BenchmarkEncodedPublishSize measures the per-publish cost of the egress
// ceiling check. It runs on every send, so it must stay allocation-free —
// packing the packet a second time to measure it would double the property
// serialisation cost of the whole send path.
func BenchmarkEncodedPublishSize(b *testing.B) {
	pub := benchmarkPublish()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = encodedPublishSize(pub)
	}
}

// BenchmarkEncodedPublishSizeViaSDKEncoder is the comparison baseline: what the
// same measurement costs if taken by asking the SDK to encode the packet.
func BenchmarkEncodedPublishSizeViaSDKEncoder(b *testing.B) {
	pub := benchmarkPublish()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pub.Packet().ToControlPacket().WriteTo(io.Discard)
	}
}

// BenchmarkValidatePublishFieldLimits measures the field-limit sweep that runs
// once per constructed publish.
func BenchmarkValidatePublishFieldLimits(b *testing.B) {
	pub := benchmarkPublish()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = validatePublishFieldLimits(pub)
	}
}

// BenchmarkPublishEnvelopeRejectedByBrokerLimit measures the full rejection
// path through the ACL seam: construct, validate fields, measure, refuse. A
// route facing a misconfigured producer takes this path for every message, so
// it must not be more expensive than the accepted path.
func BenchmarkPublishEnvelopeRejectedByBrokerLimit(b *testing.B) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "bench-id-123",
		Subject: "bench/topic",
		Payload: make([]byte, 4096),
	})
	conn := &pahoConn{brokerMaxPacketSize: func() uint32 { return 1024 }}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.PublishEnvelope(ctx, env, "bench/egress/topic", SenderOptions{QoS: 1}, nil); err == nil {
			b.Fatal("expected the over-limit publish to be rejected")
		}
	}
}

// BenchmarkBrokerProxyDialer measures the per-dial environment resolution.
// Unlike golang.org/x/net/proxy it is not cached in a sync.Once, so the cost is
// paid on every connect and every autopaho reconnect.
func BenchmarkBrokerProxyDialer(b *testing.B) {
	env := map[string]string{
		"ALL_PROXY": "socks5://127.0.0.1:1080",
		"NO_PROXY":  strings.Join([]string{"localhost", "127.0.0.1", "broker.internal"}, ","),
	}
	lookup := proxyEnvLookup(func(name string) string { return env[name] })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := brokerProxyDialer(lookup); err != nil {
			b.Fatal(err)
		}
	}
}
