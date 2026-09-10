package paho

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The predecode guard truncates the User Property list of a PUBLISH to one
// entry above the retained cap before the SDK decodes it, so the decode cost
// of a packet the callback will refuse anyway is bounded by the cap and not by
// the wire. Everything else in the packet — topic, flags, packet identifier,
// every other property, the payload, and the framing of the packet behind it —
// must survive untouched.

func testRawContentType(value string) []byte {
	return append([]byte{0x03, byte(len(value) >> 8), byte(len(value))}, value...)
}

func testRawCorrelationData(data []byte) []byte {
	return append([]byte{0x09, byte(len(data) >> 8), byte(len(data))}, data...)
}

func testRawUserProperty(key, value string) []byte {
	out := []byte{0x26, byte(len(key) >> 8), byte(len(key))}
	out = append(out, key...)
	out = append(out, byte(len(value)>>8), byte(len(value)))
	return append(out, value...)
}

// testDecodePublish decodes exactly one PUBLISH from wire and reports how many
// bytes it consumed, so a test can prove the packet behind it kept its framing.
func testDecodePublish(t *testing.T, wire []byte) (*packets.Publish, int) {
	t.Helper()
	reader := bytes.NewReader(wire)
	control, err := packets.ReadPacket(reader)
	require.NoError(t, err)
	publish, ok := control.Content.(*packets.Publish)
	require.True(t, ok, "expected a PUBLISH, got packet type %d", control.Type)
	return publish, len(wire) - reader.Len()
}

// oversizedUserPropertyPacket builds a PUBLISH carrying excess User Properties
// above the cap, with other properties before AND after the excess so the test
// can prove they are kept, and returns the User Properties a truncated packet
// must still carry: the first cap+1, verbatim and in order.
func oversizedUserPropertyPacket(qos byte, excess int) (packet []byte, want []packets.User) {
	properties := testRawContentType("application/json")
	properties = append(properties, 0x02, 0, 0, 0, 60) // Message Expiry Interval 60
	for i := range maxIngressUserProperties + 1 + excess {
		key := fmt.Sprintf("k%d", i)
		properties = append(properties, testRawUserProperty(key, "v")...)
		if i <= maxIngressUserProperties {
			want = append(want, packets.User{Key: key, Value: "v"})
		}
	}
	properties = append(properties, testRawCorrelationData([]byte{1, 2, 3})...)
	properties = append(properties, 0x0B, 0x05) // Subscription Identifier 5
	packet = testPublishPacketWithPacketID(qos, "guard/truncate", 9, properties, []byte("payload"))
	packet[0] |= 0x01 // RETAIN
	if qos > 0 {
		packet[0] |= 0x08 // DUP
	}
	return packet, want
}

func TestMQTTIngressConn_TruncatesUserPropertiesToCapPlusOne(t *testing.T) {
	for _, qos := range []byte{0, 1, 2} {
		t.Run(testNameForInt(int(qos)), func(t *testing.T) {
			packet, want := oversizedUserPropertyPacket(qos, 40)
			trailer := []byte{0xD0, 0x00} // PINGRESP behind the PUBLISH
			guarded := newMQTTIngressConn(
				newTestNetConn(append(append([]byte{}, packet...), trailer...), 1),
				uint32(len(packet)),
				nil,
			)

			got, err := io.ReadAll(guarded)
			require.NoError(t, err)
			require.Less(t, len(got), len(packet)+len(trailer),
				"excess User Properties must be removed before the SDK sees the packet")

			publish, consumed := testDecodePublish(t, got)
			assert.Equal(t, trailer, got[consumed:], "the packet behind a truncated PUBLISH must keep its framing")
			assert.Equal(t, "guard/truncate", publish.Topic)
			assert.Equal(t, qos, publish.QoS)
			assert.True(t, publish.Retain)
			assert.Equal(t, qos > 0, publish.Duplicate)
			if qos > 0 {
				assert.Equal(t, uint16(9), publish.PacketID)
			}
			assert.Equal(t, []byte("payload"), publish.Payload)
			assert.Equal(t, want, publish.Properties.User)
			assert.Equal(t, "application/json", publish.Properties.ContentType)
			require.NotNil(t, publish.Properties.MessageExpiry)
			assert.Equal(t, uint32(60), *publish.Properties.MessageExpiry)
			assert.Equal(t, []byte{1, 2, 3}, publish.Properties.CorrelationData,
				"a property behind the excess must survive")
			require.NotNil(t, publish.Properties.SubscriptionIdentifier)
			assert.Equal(t, 5, *publish.Properties.SubscriptionIdentifier)
		})
	}
}

func TestMQTTIngressConn_UserPropertyCountAtOrBelowCapPlusOneIsUntouched(t *testing.T) {
	for _, count := range []int{maxIngressUserProperties, maxIngressUserProperties + 1} {
		t.Run(testNameForInt(count), func(t *testing.T) {
			packet := testPublishPacket(1, "guard/at-cap", testUserProperties(count), []byte("ok"))
			reported := 0
			guarded := newMQTTIngressConn(newTestNetConn(packet, 4), uint32(len(packet)), nil)
			guarded.onTruncate = func(int) { reported++ }

			got, err := io.ReadAll(guarded)
			require.NoError(t, err)
			assert.Equal(t, packet, got)
			assert.Zero(t, reported, "nothing was removed, so nothing must be reported")
		})
	}
}

func TestMQTTIngressConn_TruncationSurvivesFragmentedReads(t *testing.T) {
	packet, want := oversizedUserPropertyPacket(1, 300)
	whole, err := io.ReadAll(newMQTTIngressConn(newTestNetConn(packet, 0), uint32(len(packet)), nil))
	require.NoError(t, err)

	guarded := newMQTTIngressConn(newTestNetConn(packet, 1), uint32(len(packet)), nil)
	var fragmented bytes.Buffer
	scratch := make([]byte, 3)
	for {
		n, readErr := guarded.Read(scratch)
		fragmented.Write(scratch[:n])
		if readErr == io.EOF {
			break
		}
		require.NoError(t, readErr)
	}

	assert.Equal(t, whole, fragmented.Bytes())
	publish, _ := testDecodePublish(t, fragmented.Bytes())
	assert.Equal(t, want, publish.Properties.User)
}

// TestMQTTIngressConn_TruncationShrinksRemainingLengthEncoding covers the
// packet whose Remaining Length needs fewer variable-byte-integer digits after
// truncation than before: every later byte moves, and the packet behind it
// must still start where its own fixed header says.
func TestMQTTIngressConn_TruncationShrinksRemainingLengthEncoding(t *testing.T) {
	packet, want := oversizedUserPropertyPacket(1, 3_400)
	require.GreaterOrEqual(t, len(packet), 1<<14, "the original Remaining Length must need three digits")
	trailer := testPublishPacket(0, "guard/next", nil, []byte("next"))
	guarded := newMQTTIngressConn(
		newTestNetConn(append(append([]byte{}, packet...), trailer...), 1<<10),
		uint32(len(packet)),
		nil,
	)

	got, err := io.ReadAll(guarded)
	require.NoError(t, err)
	publish, consumed := testDecodePublish(t, got)
	assert.Equal(t, want, publish.Properties.User)
	assert.Less(t, consumed, 1<<14, "the truncated Remaining Length must fit two digits")
	assert.Equal(t, trailer, got[consumed:])
}

func TestMQTTIngressConn_TruncationReportsTheWireCount(t *testing.T) {
	const excess = 25
	packet, _ := oversizedUserPropertyPacket(0, excess)
	var reported []int
	violations := 0
	guarded := newMQTTIngressConn(newTestNetConn(packet, 64), uint32(len(packet)), func(error) { violations++ })
	guarded.onTruncate = func(count int) { reported = append(reported, count) }

	_, err := io.ReadAll(guarded)
	require.NoError(t, err)
	assert.Equal(t, []int{maxIngressUserProperties + 1 + excess}, reported,
		"the guard must report how many User Properties were on the wire, once per packet")
	assert.Zero(t, violations, "truncation is not a violation: the packet stays broker-forwardable and is acked by the callback")
	assert.NoError(t, guarded.readErr)
}

func TestSession_PredecodeTruncationIsCountedPerSession(t *testing.T) {
	rec := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{ClientID: "truncate-session"}, connectivity.SessionEphemeral, nil, rec)

	session.notePredecodeTruncation(5_000)
	session.notePredecodeTruncation(129 + 1)

	entries := rec.FindEntries(MetricMQTTIngressUserPropertiesTruncated)
	require.Len(t, entries, 2, "every truncated packet is counted")
	for _, entry := range entries {
		assert.Equal(t, int64(1), entry.IValue)
		assert.Contains(t, entry.Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "truncate-session"})
	}
	session.mu.Lock()
	terminalErr := session.terminalErr
	session.mu.Unlock()
	assert.NoError(t, terminalErr, "truncation must never latch the session terminal")
}

func TestSession_IngressGuardWiresTruncationToTheSessionCounter(t *testing.T) {
	rec := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{ClientID: "wired-session"}, connectivity.SessionEphemeral, nil, rec)

	guard, err := session.guardIngress(newTestNetConn(nil, 0))
	require.NoError(t, err)
	require.NotNil(t, guard.onTruncate)
	guard.onTruncate(1_000)

	assert.Len(t, rec.FindEntries(MetricMQTTIngressUserPropertiesTruncated), 1)
	wire, err := wirePacketSizeFor(session.opts.MaxPayloadBytes)
	require.NoError(t, err)
	assert.Equal(t, wire, guard.maximumPacketSize)
	require.NotNil(t, guard.onViolation)
}
