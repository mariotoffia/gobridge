package paho

import (
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ═══════════════════════════════════════════════════════════════════════════
// MQTT Hot-Path Benchmarks
//
// Establish performance baselines for critical per-message operations.
// All benchmarks use b.ReportAllocs() to track allocation counts.
// ═══════════════════════════════════════════════════════════════════════════

// BenchmarkRouter_Route_SingleHandler measures the per-message cost of
// Route() with a single registered handler: goroutine spawn, Publish
// struct copy, Payload deep-copy.
func BenchmarkRouter_Route_SingleHandler(b *testing.B) {
	for _, size := range []int{1024, 10240, 102400} {
		name := map[int]string{1024: "1KB", 10240: "10KB", 102400: "100KB"}[size]
		b.Run(name, func(b *testing.B) {
			r := newRouter(nil, nil)
			r.Register("bench", func(_ *pahov5.Publish) {})

			payload := make([]byte, size)
			pb := &packets.Publish{
				Topic:      "bench/topic",
				Payload:    payload,
				Properties: &packets.Properties{ContentType: "application/json"},
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Route(pb)
				r.Wait()
			}
		})
	}
}

// BenchmarkRouter_Route_MultiHandler measures scaling cost with 3
// handlers: 3x goroutine spawn, 3x Payload copy, 3x struct copy.
func BenchmarkRouter_Route_MultiHandler(b *testing.B) {
	for _, size := range []int{1024, 10240, 102400} {
		name := map[int]string{1024: "1KB", 10240: "10KB", 102400: "100KB"}[size]
		b.Run(name, func(b *testing.B) {
			r := newRouter(nil, nil)
			for i := 0; i < 3; i++ {
				id := string(rune('a' + i))
				r.Register(id, func(_ *pahov5.Publish) {})
			}

			payload := make([]byte, size)
			pb := &packets.Publish{
				Topic:      "bench/topic",
				Payload:    payload,
				Properties: &packets.Properties{ContentType: "application/json"},
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Route(pb)
				r.Wait()
			}
		})
	}
}

// BenchmarkDeriveEnvelopeID measures the cost of the deterministic
// SHA-256 based fallback ID derivation (topic + payload hash, 16 bytes
// hex encoded).
func BenchmarkDeriveEnvelopeID(b *testing.B) {
	payload := make([]byte, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = deriveEnvelopeID("bench/topic/path", payload)
	}
}

// BenchmarkEnvelopeFromPublish measures the full MQTT-to-Envelope
// conversion cost including header map allocation, Property extraction,
// and deriveEnvelopeID().
func BenchmarkEnvelopeFromPublish(b *testing.B) {
	pub := &pahov5.Publish{
		Topic:   "bench/topic/path",
		Payload: make([]byte, 1024),
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr-123"),
			ContentType:     "application/json",
			ResponseTopic:   "reply/topic",
			MessageExpiry:   func() *uint32 { v := uint32(300); return &v }(),
			User: pahov5.UserProperties{
				{Key: "key1", Value: "val1"},
				{Key: "key2", Value: "val2"},
				{Key: "key3", Value: "val3"},
				{Key: "key4", Value: "val4"},
				{Key: "key5", Value: "val5"},
			},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EnvelopeFromPublish(pub, nil)
	}
}

// BenchmarkPublishFromEnvelope measures the reverse: Envelope to MQTT
// Publish conversion for the send path.
func BenchmarkPublishFromEnvelope(b *testing.B) {
	now := time.Now()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "bench-id-123",
		Subject: "bench/topic",
		Payload: make([]byte, 1024),
		Headers: map[string]any{
			"key1": "val1",
			"key2": "val2",
			"key3": "val3",
			"key4": "val4",
			"key5": "val5",
		},
		CreatedAt: now,
		ExpiresAt: now.Add(5 * time.Minute),
	})
	opts := SenderOptions{
		DefaultTopic: "bench/topic",
		QoS:          1,
		Timeout:      10 * time.Second,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = PublishFromEnvelope(env, env.Subject(), opts, nil)
	}
}
