package paho

import (
	"io"
	"net"
	"testing"

	"github.com/eclipse/paho.golang/packets"
)

// ═══════════════════════════════════════════════════════════════════════════
// Predecode ingress guard benchmarks
//
// The guard runs on Paho's read goroutine for EVERY inbound packet, so its
// per-packet cost bounds ingress throughput. Two shapes matter:
//
//   - the ordinary max-payload packet, where the guard only frames and
//     validates (the hot path), and
//   - the packet the guard exists for — the advertised maximum filled with
//     five-byte User Properties — where it walks tens of thousands of
//     properties and truncates the list before the SDK decodes it.
//
// The SDK decode is measured both through the guard and raw, so the
// amplification the guard removes stays visible as a number.
// ═══════════════════════════════════════════════════════════════════════════

// loopNetConn serves the same packet forever so one guard and one read buffer
// are reused across iterations; per-iteration cost is then the guard's alone.
type loopNetConn struct {
	*testNetConn
	packet []byte
	offset int
}

func newLoopNetConn(packet []byte) *loopNetConn {
	return &loopNetConn{testNetConn: newTestNetConn(nil, 0), packet: packet}
}

func (c *loopNetConn) Read(p []byte) (int, error) {
	if c.offset == len(c.packet) {
		c.offset = 0
	}
	n := copy(p, c.packet[c.offset:])
	c.offset += n
	return n, nil
}

var _ net.Conn = (*loopNetConn)(nil)

func benchmarkMaxPayloadPacket(b *testing.B) []byte {
	b.Helper()
	payload := make([]byte, DefaultMaxPayloadBytes)
	return testPublishPacket(1, "bench/payload", testUserProperties(8), payload)
}

// BenchmarkIngressGuard_MaxPayloadPacketPassThrough is the hot path: a legal
// max-payload packet framed and validated by the guard, nothing rewritten.
func BenchmarkIngressGuard_MaxPayloadPacketPassThrough(b *testing.B) {
	packet := benchmarkMaxPayloadPacket(b)
	guarded := newMQTTIngressConn(newLoopNetConn(packet), uint32(len(packet)), nil)
	scratch := make([]byte, len(packet))

	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := io.ReadFull(guarded, scratch); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIngressGuard_TruncateMaximumCountProperties is the packet the guard
// exists for: the advertised maximum filled with minimum-size User
// Properties, walked twice and compacted in place.
func BenchmarkIngressGuard_TruncateMaximumCountProperties(b *testing.B) {
	packet, _ := advertisedMaximumPacket(b, DefaultMaxPayloadBytes, 0)
	guarded := newMQTTIngressConn(newLoopNetConn(packet), uint32(len(packet)), nil)
	scratch := make([]byte, len(packet))

	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := io.ReadAtLeast(guarded, scratch, 1); err != nil {
			b.Fatal(err)
		}
		// Drain whatever the guard produced for this packet before the
		// next iteration re-frames from the loop connection.
		for guarded.packet != nil {
			if _, err := guarded.Read(scratch); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkSDKDecode_MaximumCountProperties_Guarded measures what the SDK
// decode costs for the worst legal packet once the guard has truncated it.
func BenchmarkSDKDecode_MaximumCountProperties_Guarded(b *testing.B) {
	packet, _ := advertisedMaximumPacket(b, DefaultMaxPayloadBytes, 0)
	guarded := newMQTTIngressConn(newLoopNetConn(packet), uint32(len(packet)), nil)

	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := packets.ReadPacket(guarded); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSDKDecode_MaximumCountProperties_Unguarded is the reference the
// guard is measured against: the same packet decoded straight off the wire,
// every property materialised by the SDK.
func BenchmarkSDKDecode_MaximumCountProperties_Unguarded(b *testing.B) {
	packet, _ := advertisedMaximumPacket(b, DefaultMaxPayloadBytes, 0)
	raw := newLoopNetConn(packet)

	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := packets.ReadPacket(raw); err != nil {
			b.Fatal(err)
		}
	}
}
