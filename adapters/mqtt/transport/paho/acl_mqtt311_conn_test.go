package paho

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// The limits newTestMQTT311Conn applies: max_payload_bytes 1024, the guard's
// Maximum Packet Size for it (wirePacketSizeFor), and receive_maximum 4.
const (
	testMQTT311MaxPayloadBytes   = 1024
	testMQTT311MaximumPacketSize = testMQTT311MaxPayloadBytes + mqttPacketOverheadAllowance
	testMQTT311ReceiveMaximum    = 4
)

// newTestMQTT311Conn returns a translator over a fake socket holding wire, the
// socket, and the violations the translator has reported so far.
func newTestMQTT311Conn(wire []byte) (*mqtt311Conn, *testNetConn, *[]error) {
	raw := newTestNetConn(wire, 0)
	violations := new([]error)
	conn := newMQTT311Conn(
		raw,
		testMQTT311MaxPayloadBytes,
		uint32(testMQTT311MaximumPacketSize),
		testMQTT311ReceiveMaximum,
		func(err error) { *violations = append(*violations, err) },
	)
	return conn, raw, violations
}

// writeV5 writes packet through conn the way Paho does: ControlPacket.WriteTo
// issues the fixed header and each body buffer as a separate Write.
func writeV5(conn *mqtt311Conn, packet io.WriterTo) error {
	_, err := packet.WriteTo(conn)
	return err
}

// readV5 returns the next packet conn hands up. Like the guard, it takes one
// whole packet: a Read never spans two packets.
func readV5(t *testing.T, conn *mqtt311Conn) []byte {
	t.Helper()
	buf := make([]byte, 1<<17)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Less(t, n, len(buf))
	return buf[:n]
}

// decodeV5 decodes packet with Paho's MQTT 5 codec and requires its Remaining
// Length to cover it exactly.
func decodeV5(t *testing.T, packet []byte) *packets.ControlPacket {
	t.Helper()
	r := bytes.NewReader(packet)
	cp, err := packets.ReadPacket(r)
	require.NoError(t, err)
	require.Zero(t, r.Len(), "bytes after the packet")
	return cp
}

// publishV5 decodes an MQTT 5 PUBLISH.
func publishV5(t *testing.T, packet []byte) *packets.Publish {
	t.Helper()
	pub, ok := decodeV5(t, packet).Content.(*packets.Publish)
	require.True(t, ok, "not a PUBLISH")
	return pub
}

// publish311 encodes an MQTT 3.1.1 PUBLISH on topic "t" (§3.3). flags holds
// DUP, QoS and RETAIN; id is written for QoS 1 and 2.
func publish311(flags byte, id uint16, payload []byte) []byte {
	body := []byte{0, 1, 't'}
	if flags&0x06 != 0 {
		body = append(body, byte(id>>8), byte(id))
	}
	body = append(body, payload...)
	return append(appendMQTTVBI([]byte{packets.PUBLISH<<4 | flags}, len(body)), body...)
}

func TestMQTT311Conn_ConnectDerivesCleanSessionFromSessionExpiry(t *testing.T) {
	day, zero := uint32(86400), uint32(0)
	cases := []struct {
		name       string
		cleanStart bool
		expiry     *uint32
		flags      byte
	}{
		{"clean start without expiry", true, nil, 0x02},
		{"clean start with a zero expiry", true, &zero, 0x02},
		{"resume with expiry", false, &day, 0x00},
		{"resume without expiry ends the session with the connection", false, nil, 0x02},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			cp := packets.NewControlPacket(packets.CONNECT)
			connect := cp.Content.(*packets.Connect)
			connect.ClientID, connect.KeepAlive, connect.CleanStart = "c", 30, tc.cleanStart
			connect.Properties.SessionExpiryInterval = tc.expiry
			require.NoError(t, writeV5(conn, cp))
			assert.Equal(t, []byte{0x10, 13, 0, 4, 'M', 'Q', 'T', 'T', 4, tc.flags, 0, 30, 0, 1, 'c'}, raw.Written())
		})
	}
}

func TestMQTT311Conn_ConnectKeepsWillAndCredentialsAndDropsProperties(t *testing.T) {
	conn, raw, _ := newTestMQTT311Conn(nil)
	receiveMaximum, maximumPacketSize := uint16(192), uint32(1<<20)
	cp := packets.NewControlPacket(packets.CONNECT)
	connect := cp.Content.(*packets.Connect)
	connect.ClientID, connect.KeepAlive, connect.CleanStart = "c", 10, true
	connect.Properties = &packets.Properties{ReceiveMaximum: &receiveMaximum, MaximumPacketSize: &maximumPacketSize}
	connect.WillFlag, connect.WillQOS, connect.WillRetain = true, 1, true
	connect.WillTopic, connect.WillMessage = "w", []byte("x")
	connect.WillProperties = &packets.Properties{User: []packets.User{{Key: "k", Value: "v"}}}
	connect.UsernameFlag, connect.Username = true, "u"
	connect.PasswordFlag, connect.Password = true, []byte("p")
	require.NoError(t, writeV5(conn, cp))
	assert.Equal(t, []byte{
		0x10, 25, 0, 4, 'M', 'Q', 'T', 'T', 4,
		0x80 | 0x40 | 0x20 | 0x08 | 0x04 | 0x02, // user name, password, will retain, will QoS 1, will, clean session
		0, 10, 0, 1, 'c', 0, 1, 'w', 0, 1, 'x', 0, 1, 'u', 0, 1, 'p',
	}, raw.Written())
}

func TestMQTT311Conn_ConnectWithoutAnMQTT311FormDropsTheConnection(t *testing.T) {
	day := uint32(86400)
	for name, mutate := range map[string]func(*packets.Connect){
		"clean start that keeps the session": func(c *packets.Connect) {
			c.CleanStart = true
			c.Properties.SessionExpiryInterval = &day
		},
		"password without a user name": func(c *packets.Connect) { c.PasswordFlag, c.Password = true, []byte("p") },
	} {
		t.Run(name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			cp := packets.NewControlPacket(packets.CONNECT)
			connect := cp.Content.(*packets.Connect)
			connect.ClientID = "c"
			mutate(connect)
			require.Error(t, writeV5(conn, cp))
			assert.Empty(t, raw.Written())
			assert.Equal(t, 1, raw.CloseCount())
		})
	}
}

func TestMQTT311Conn_PublishDropsProperties(t *testing.T) {
	expiry := uint32(60)
	cases := []struct {
		name    string
		publish packets.Publish
		want    []byte
	}{
		{
			name:    "QoS 0",
			publish: packets.Publish{Topic: "t", Payload: []byte("hi")},
			want:    []byte{0x30, 5, 0, 1, 't', 'h', 'i'},
		},
		{
			name: "QoS 1 with DUP, RETAIN and properties",
			publish: packets.Publish{
				Topic: "t", QoS: 1, PacketID: 7, Duplicate: true, Retain: true, Payload: []byte("hi"),
				Properties: &packets.Properties{ContentType: "c", User: []packets.User{{Key: "k", Value: "v"}}},
			},
			want: []byte{0x3B, 7, 0, 1, 't', 0, 7, 'h', 'i'},
		},
		{
			name: "QoS 2 with properties",
			publish: packets.Publish{
				Topic: "t", QoS: 2, PacketID: 8, Payload: []byte("hi"),
				Properties: &packets.Properties{MessageExpiry: &expiry, CorrelationData: []byte("c")},
			},
			want: []byte{0x34, 7, 0, 1, 't', 0, 8, 'h', 'i'},
		},
		{
			name:    "empty payload",
			publish: packets.Publish{Topic: "t"},
			want:    []byte{0x30, 3, 0, 1, 't'},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			require.NoError(t, writeV5(conn, &tc.publish))
			assert.Equal(t, tc.want, raw.Written())
		})
	}
}

func TestMQTT311Conn_PublishByTopicAliasDropsTheConnection(t *testing.T) {
	conn, raw, _ := newTestMQTT311Conn(nil)
	alias := uint16(1)
	publish := &packets.Publish{Payload: []byte("hi"), Properties: &packets.Properties{TopicAlias: &alias}}
	require.Error(t, writeV5(conn, publish))
	assert.Empty(t, raw.Written())
	assert.Equal(t, 1, raw.CloseCount())
}

func TestMQTT311Conn_AcksUseThePacketIdentifierForm(t *testing.T) {
	cases := []struct {
		name string
		ack  io.WriterTo
		want []byte
	}{
		{"PUBACK", &packets.Puback{PacketID: 9, Properties: &packets.Properties{}}, []byte{0x40, 2, 0, 9}},
		{
			"PUBACK with a reason code and properties",
			&packets.Puback{PacketID: 9, ReasonCode: 0x10, Properties: &packets.Properties{ReasonString: "r"}},
			[]byte{0x40, 2, 0, 9},
		},
		{"PUBREC", &packets.Pubrec{PacketID: 9, Properties: &packets.Properties{}}, []byte{0x50, 2, 0, 9}},
		{"PUBREL", &packets.Pubrel{PacketID: 9}, []byte{0x62, 2, 0, 9}},
		// Paho's answer to a PUBREC it does not know.
		{"PUBREL packet identifier not found", &packets.Pubrel{PacketID: 9, ReasonCode: 0x92}, []byte{0x62, 2, 0, 9}},
		{"PUBCOMP", &packets.Pubcomp{PacketID: 9}, []byte{0x70, 2, 0, 9}},
		{"PUBCOMP packet identifier not found", &packets.Pubcomp{PacketID: 9, ReasonCode: 0x92}, []byte{0x70, 2, 0, 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			require.NoError(t, writeV5(conn, tc.ack))
			assert.Equal(t, tc.want, raw.Written())
		})
	}
}

func TestMQTT311Conn_SubscribeKeepsOnlyTheRequestedQoS(t *testing.T) {
	conn, raw, _ := newTestMQTT311Conn(nil)
	subscriptionID := 5
	cp := packets.NewControlPacket(packets.SUBSCRIBE)
	sub := cp.Content.(*packets.Subscribe)
	sub.PacketID, sub.Properties.SubscriptionIdentifier = 3, &subscriptionID
	sub.Subscriptions = []packets.SubOptions{
		{Topic: "a/#", QoS: 2, RetainHandling: 1},
		{Topic: "b", QoS: 0, RetainHandling: 2},
	}
	require.NoError(t, writeV5(conn, cp))
	assert.Equal(t, []byte{0x82, 12, 0, 3, 0, 3, 'a', '/', '#', 2, 0, 1, 'b', 0}, raw.Written())
}

func TestMQTT311Conn_SubscribeWithNoLocalOrRetainAsPublishedDropsTheConnection(t *testing.T) {
	for name, option := range map[string]packets.SubOptions{
		"No Local":            {Topic: "a", NoLocal: true},
		"Retain As Published": {Topic: "a", RetainAsPublished: true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			cp := packets.NewControlPacket(packets.SUBSCRIBE)
			sub := cp.Content.(*packets.Subscribe)
			sub.PacketID, sub.Subscriptions = 3, []packets.SubOptions{option}
			require.Error(t, writeV5(conn, cp))
			assert.Empty(t, raw.Written())
			assert.Equal(t, 1, raw.CloseCount())
		})
	}
}

func TestMQTT311Conn_UnsubscribeAndPingreq(t *testing.T) {
	conn, raw, _ := newTestMQTT311Conn(nil)
	cp := packets.NewControlPacket(packets.UNSUBSCRIBE)
	unsub := cp.Content.(*packets.Unsubscribe)
	unsub.PacketID, unsub.Topics = 5, []string{"a", "b"}
	unsub.Properties.User = []packets.User{{Key: "k", Value: "v"}}
	require.NoError(t, writeV5(conn, cp))
	require.NoError(t, writeV5(conn, packets.NewControlPacket(packets.PINGREQ)))
	assert.Equal(t, []byte{0xA2, 8, 0, 5, 0, 1, 'a', 0, 1, 'b', 0xC0, 0}, raw.Written())
}

func TestMQTT311Conn_OnlyANormalDisconnectReachesTheBroker(t *testing.T) {
	disconnect := func(reason byte) func(*mqtt311Conn) {
		return func(conn *mqtt311Conn) {
			cp := packets.NewControlPacket(packets.DISCONNECT)
			cp.Content.(*packets.Disconnect).ReasonCode = reason
			require.NoError(t, writeV5(conn, cp))
		}
	}
	guardReject := func(reason byte) func(*mqtt311Conn) {
		return func(conn *mqtt311Conn) { writeMQTTDisconnect(conn, reason) }
	}
	cases := []struct {
		name  string
		write func(*mqtt311Conn)
		want  []byte
	}{
		{"normal disconnection", disconnect(packets.DisconnectNormalDisconnection), []byte{0xE0, 0}},
		// A 3.1.1 broker publishes the Last Will on a close without DISCONNECT,
		// as an MQTT 5 broker does for every reason but 0x00 (MQTT-3.1.2-8).
		{"disconnect with will message", disconnect(packets.DisconnectDisconnectWithWillMessage), nil},
		{"guard reject of a malformed packet", guardReject(packets.DisconnectMalformedPacket), nil},
		{"guard reject of a packet too large", guardReject(packets.DisconnectPacketTooLarge), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, raw, _ := newTestMQTT311Conn(nil)
			tc.write(conn)
			assert.Equal(t, tc.want, raw.Written())
			assert.Zero(t, raw.CloseCount(), "the caller closes the socket")
		})
	}
}

func TestMQTT311Conn_AuthHasNoMQTT311Form(t *testing.T) {
	conn, raw, _ := newTestMQTT311Conn(nil)
	require.Error(t, writeV5(conn, packets.NewControlPacket(packets.AUTH)))
	assert.Empty(t, raw.Written())
	assert.Equal(t, 1, raw.CloseCount())
}

func TestMQTT311Conn_SplitAndCoalescedWritesTranslateIdentically(t *testing.T) {
	var first, rest bytes.Buffer
	_, err := (&packets.Publish{
		Topic: "t", QoS: 1, PacketID: 7, Payload: []byte("hi"),
		Properties: &packets.Properties{ContentType: "c"},
	}).WriteTo(&first)
	require.NoError(t, err)
	_, err = (&packets.Puback{PacketID: 9, Properties: &packets.Properties{}}).WriteTo(&rest)
	require.NoError(t, err)
	_, err = packets.NewControlPacket(packets.PINGREQ).WriteTo(&rest)
	require.NoError(t, err)
	v5 := append(append([]byte{}, first.Bytes()...), rest.Bytes()...)
	want := []byte{0x32, 7, 0, 1, 't', 0, 7, 'h', 'i', 0x40, 2, 0, 9, 0xC0, 0}

	t.Run("coalesced", func(t *testing.T) {
		conn, raw, _ := newTestMQTT311Conn(nil)
		n, err := conn.Write(v5)
		require.NoError(t, err)
		assert.Equal(t, len(v5), n)
		assert.Equal(t, want, raw.Written())
	})
	t.Run("one byte at a time", func(t *testing.T) {
		conn, raw, _ := newTestMQTT311Conn(nil)
		for i, b := range v5 {
			if i == first.Len()-1 {
				assert.Empty(t, raw.Written(), "nothing reaches the broker before a whole packet")
			}
			n, err := conn.Write([]byte{b})
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}
		assert.Equal(t, want, raw.Written())
	})
}

func TestMQTT311Conn_ConnackMapsReturnCodes(t *testing.T) {
	cases := []struct {
		name           string
		connack        []byte
		sessionPresent bool
		reason         byte
	}{
		{"accepted", []byte{0x20, 2, 0, 0}, false, packets.ConnackSuccess},
		{"accepted with session present", []byte{0x20, 2, 1, 0}, true, packets.ConnackSuccess},
		{"unacceptable protocol version", []byte{0x20, 2, 0, 1}, false, packets.ConnackUnsupportedProtocolVersion},
		{"identifier rejected", []byte{0x20, 2, 0, 2}, false, packets.ConnackInvalidClientID},
		{"server unavailable", []byte{0x20, 2, 0, 3}, false, packets.ConnackServerUnavailable},
		{"bad user name or password", []byte{0x20, 2, 0, 4}, false, packets.ConnackBadUsernameOrPassword},
		{"not authorized", []byte{0x20, 2, 0, 5}, false, packets.ConnackNotAuthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, violations := newTestMQTT311Conn(tc.connack)
			got := readV5(t, conn)
			assert.Equal(t, []byte{0x20, 3, tc.connack[2], tc.reason, 0}, got)
			connack, ok := decodeV5(t, got).Content.(*packets.Connack)
			require.True(t, ok)
			assert.Equal(t, tc.sessionPresent, connack.SessionPresent)
			assert.Equal(t, tc.reason, connack.ReasonCode)
			assert.Empty(t, *violations)
		})
	}
}

func TestMQTT311Conn_InboundPacketsDecodeAsMQTT5(t *testing.T) {
	cases := []struct {
		name  string
		mqtt3 []byte
		mqtt5 []byte
	}{
		{"PUBLISH QoS 0", []byte{0x30, 5, 0, 1, 't', 'h', 'i'}, []byte{0x30, 6, 0, 1, 't', 0, 'h', 'i'}},
		{"PUBLISH QoS 1", []byte{0x32, 7, 0, 1, 't', 0, 1, 'h', 'i'}, []byte{0x32, 8, 0, 1, 't', 0, 1, 0, 'h', 'i'}},
		{"PUBLISH QoS 2 retained", []byte{0x35, 7, 0, 1, 't', 0, 2, 'h', 'i'}, []byte{0x35, 8, 0, 1, 't', 0, 2, 0, 'h', 'i'}},
		// DUP on an identifier not in flight is a redelivery after a resume.
		{"PUBLISH QoS 1 DUP", []byte{0x3A, 7, 0, 1, 't', 0, 3, 'h', 'i'}, []byte{0x3A, 8, 0, 1, 't', 0, 3, 0, 'h', 'i'}},
		{"PUBLISH empty payload", []byte{0x30, 3, 0, 1, 't'}, []byte{0x30, 4, 0, 1, 't', 0}},
		{"PUBACK", []byte{0x40, 2, 0, 9}, []byte{0x40, 2, 0, 9}},
		{"PUBREC", []byte{0x50, 2, 0, 9}, []byte{0x50, 2, 0, 9}},
		{"PUBREL", []byte{0x62, 2, 0, 9}, []byte{0x62, 2, 0, 9}},
		{"PUBCOMP", []byte{0x70, 2, 0, 9}, []byte{0x70, 2, 0, 9}},
		{"SUBACK", []byte{0x90, 6, 0, 3, 0, 1, 2, 0x80}, []byte{0x90, 7, 0, 3, 0, 0, 1, 2, 0x80}},
		{"PINGRESP", []byte{0xD0, 0}, []byte{0xD0, 0}},
	}
	var wire []byte
	for _, tc := range cases {
		wire = append(wire, tc.mqtt3...)
	}
	conn, raw, violations := newTestMQTT311Conn(wire)
	for _, tc := range cases {
		got := readV5(t, conn)
		assert.Equal(t, tc.mqtt5, got, tc.name)
		decodeV5(t, got)
	}
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_InboundPublishGetsANewRemainingLength(t *testing.T) {
	payload := bytes.Repeat([]byte{'p'}, 124) // Remaining Length 127 on 3.1.1, 128 with the property block
	conn, _, _ := newTestMQTT311Conn(publish311(0, 0, payload))
	got := readV5(t, conn)
	assert.Equal(t, append([]byte{0x30, 0x80, 0x01, 0, 1, 't', 0}, payload...), got)
	assert.Equal(t, payload, publishV5(t, got).Payload)
}

func TestMQTT311Conn_UnsubackGetsOneSuccessPerFilter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		filters []string
		want    []byte
	}{
		{"one filter", []string{"a"}, []byte{0xB0, 4, 0, 5, 0, 0}},
		{"three filters", []string{"a", "b", "c"}, []byte{0xB0, 6, 0, 5, 0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, violations := newTestMQTT311Conn([]byte{0xB0, 2, 0, 5, 0xB0, 2, 0, 5})
			cp := packets.NewControlPacket(packets.UNSUBSCRIBE)
			unsub := cp.Content.(*packets.Unsubscribe)
			unsub.PacketID, unsub.Topics = 5, tc.filters
			require.NoError(t, writeV5(conn, cp))

			got := readV5(t, conn)
			assert.Equal(t, tc.want, got)
			unsuback, ok := decodeV5(t, got).Content.(*packets.Unsuback)
			require.True(t, ok)
			assert.Equal(t, make([]byte, len(tc.filters)), unsuback.Reasons)

			_, err := conn.Read(make([]byte, 64))
			require.Error(t, err, "an UNSUBACK answers one UNSUBSCRIBE")
			assert.Len(t, *violations, 1)
		})
	}
}

func TestMQTT311Conn_MalformedInboundIsAViolation(t *testing.T) {
	cases := map[string][]byte{
		"3-byte PUBACK":                       {0x40, 3, 0, 1, 0x87},
		"3-byte PUBREL":                       {0x62, 3, 0, 7, 0x92},
		"PUBREL without its flags":            {0x60, 2, 0, 7},
		"PUBREL with other flags":             {0x63, 2, 0, 7},
		"PUBACK with flags":                   {0x41, 2, 0, 7},
		"PUBACK zero packet identifier":       {0x40, 2, 0, 0},
		"SUBACK zero packet identifier":       {0x90, 3, 0, 0, 0},
		"SUBACK reserved return code":         {0x90, 3, 0, 1, 3},
		"SUBACK without return codes":         {0x90, 2, 0, 1},
		"SUBACK with flags":                   {0x91, 3, 0, 1, 0},
		"CONNACK reserved flags":              {0x20, 2, 2, 0},
		"CONNACK session present on refusal":  {0x20, 2, 1, 5},
		"CONNACK reserved return code":        {0x20, 2, 0, 6},
		"CONNACK with a property block":       {0x20, 3, 0, 0, 0},
		"UNSUBACK for no UNSUBSCRIBE":         {0xB0, 2, 0, 1},
		"PINGRESP with a body":                {0xD0, 1, 0},
		"PUBLISH shorter than a topic length": {0x30, 1, 0},
		"PUBLISH empty topic":                 {0x30, 2, 0, 0},
		"PUBLISH topic with +":                {0x30, 5, 0, 3, 'a', '/', '+'},
		"PUBLISH topic with #":                {0x30, 3, 0, 1, '#'},
		"PUBLISH topic with U+0000":           {0x30, 5, 0, 3, 'a', 0, 'b'},
		"PUBLISH QoS 3":                       {0x36, 3, 0, 1, 't'},
		"PUBLISH QoS 0 with DUP":              {0x38, 3, 0, 1, 't'},
		"PUBLISH QoS 1 zero packet id":        {0x32, 5, 0, 1, 't', 0, 0},
		"PUBLISH QoS 1 without a packet id":   {0x32, 3, 0, 1, 't'},
		"PUBLISH topic past the packet":       {0x30, 3, 0, 9, 't'},
		"server CONNECT":                      {0x10, 0},
		"server SUBSCRIBE":                    {0x82, 4, 0, 1, 0, 0},
		"server UNSUBSCRIBE":                  {0xA2, 2, 0, 1},
		"server PINGREQ":                      {0xC0, 0},
		"server DISCONNECT":                   {0xE0, 0},
		"AUTH":                                {0xF0, 0},
		"reserved packet type":                {0x00, 0},
		"non-canonical Remaining Length":      {0x40, 0x82, 0x00},
		"Remaining Length over four bytes":    {0x40, 0xFF, 0xFF, 0xFF, 0xFF},
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			conn, raw, violations := newTestMQTT311Conn(wire)
			n, err := conn.Read(make([]byte, 64))
			assert.Zero(t, n)
			var ingressErr *mqttIngressError
			require.ErrorAs(t, err, &ingressErr)
			assert.Equal(t, mqttIngressMalformed, ingressErr.kind)
			assert.ErrorIs(t, err, shared.ErrProtocolError)
			require.Len(t, *violations, 1)
			assert.Same(t, ingressErr, (*violations)[0])
			assert.Equal(t, 1, raw.CloseCount())
			assert.Empty(t, raw.Written(), "no DISCONNECT, so a 3.1.1 broker publishes the will")

			_, again := conn.Read(make([]byte, 64))
			assert.Same(t, ingressErr, again)
			assert.Len(t, *violations, 1)
		})
	}
}

func TestMQTT311Conn_OversizedNonPublishIsTooLarge(t *testing.T) {
	raw := newTestNetConn(append([]byte{0x90, 0x80, 0x01}, make([]byte, 128)...), 0)
	var violations []error
	conn := newMQTT311Conn(raw, testMQTT311MaxPayloadBytes, 64, testMQTT311ReceiveMaximum,
		func(err error) { violations = append(violations, err) })
	_, err := conn.Read(make([]byte, 64))
	var ingressErr *mqttIngressError
	require.ErrorAs(t, err, &ingressErr)
	assert.Equal(t, mqttIngressPacketTooLarge, ingressErr.kind)
	assert.ErrorIs(t, err, shared.ErrPayloadTooLarge)
	assert.Len(t, violations, 1)
	assert.Equal(t, 128, raw.UnreadBytes(), "the body is never read")
	assert.Equal(t, 1, raw.CloseCount())
	assert.Empty(t, raw.Written())
}

func TestMQTT311Conn_RetransmissionOfAnInFlightPublishIsDropped(t *testing.T) {
	var wire []byte
	for id := uint16(1); id <= testMQTT311ReceiveMaximum; id++ {
		wire = append(wire, publish311(0x02, id, []byte("x"))...)
	}
	// The window is full. A DUP copy of identifier 1, larger than the payload
	// cap, is a retransmission: it is skipped whole and counts nothing.
	wire = append(wire, publish311(0x08|0x02, 1, bytes.Repeat([]byte{'r'}, 2000))...)
	wire = append(wire, 0xD0, 0)
	conn, raw, violations := newTestMQTT311Conn(wire)
	for id := uint16(1); id <= testMQTT311ReceiveMaximum; id++ {
		assert.Equal(t, id, publishV5(t, readV5(t, conn)).PacketID)
	}
	assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn), "the retransmission never reaches Paho")
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_PacketIdentifierIsReusableAfterPahosPuback(t *testing.T) {
	conn, _, violations := newTestMQTT311Conn(append(
		publish311(0x02, 1, []byte("x")),
		publish311(0x02, 1, []byte("z"))...,
	))
	assert.Equal(t, []byte("x"), publishV5(t, readV5(t, conn)).Payload)
	require.NoError(t, writeV5(conn, &packets.Puback{PacketID: 1, Properties: &packets.Properties{}}))
	assert.Equal(t, []byte("z"), publishV5(t, readV5(t, conn)).Payload)
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_RetransmissionThatCrossesItsPubackIsDropped(t *testing.T) {
	for _, id := range []uint16{1, 64, 65535} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			// The broker's retry sends a DUP copy before Paho's PUBACK reaches
			// it, and the copy is read after the PUBACK is written. It is
			// skipped whole. A DUP on an identifier never seen on this
			// connection is still a redelivery after a resume.
			other := id%65535 + 1
			wire := publish311(0x02, id, []byte("x"))
			wire = append(wire, publish311(0x08|0x02, id, bytes.Repeat([]byte{'r'}, 2000))...)
			wire = append(wire, publish311(0x08|0x02, other, []byte("y"))...)
			conn, raw, violations := newTestMQTT311Conn(wire)

			assert.Equal(t, []byte("x"), publishV5(t, readV5(t, conn)).Payload)
			require.NoError(t, writeV5(conn, &packets.Puback{PacketID: id, Properties: &packets.Properties{}}))
			redelivery := publishV5(t, readV5(t, conn))
			assert.Equal(t, other, redelivery.PacketID, "the stale copy never reaches Paho")
			assert.Equal(t, []byte("y"), redelivery.Payload)
			assert.Zero(t, raw.UnreadBytes())
			assert.Empty(t, *violations)
		})
	}
}

func TestMQTT311Conn_RetransmissionThatCrossesItsPubackTakesNoWindowSlot(t *testing.T) {
	wire := publish311(0x02, 1, []byte("x"))
	wire = append(wire, publish311(0x08|0x02, 1, []byte("x"))...)
	for id := uint16(2); id <= testMQTT311ReceiveMaximum+1; id++ {
		wire = append(wire, publish311(0x02, id, []byte("y"))...)
	}
	conn, raw, violations := newTestMQTT311Conn(wire)

	readV5(t, conn)
	require.NoError(t, writeV5(conn, &packets.Puback{PacketID: 1, Properties: &packets.Properties{}}))
	for id := uint16(2); id <= testMQTT311ReceiveMaximum+1; id++ {
		assert.Equal(t, id, publishV5(t, readV5(t, conn)).PacketID, "receive_maximum new identifiers fit the window")
	}
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_IdentifierReusedAfterPubackIsANewMessage(t *testing.T) {
	var wire []byte
	wire = append(wire, publish311(0x02, 1, []byte("x"))...)
	wire = append(wire, publish311(0x02, 1, []byte("z"))...)      // the broker reuses 1 after the PUBACK
	wire = append(wire, publish311(0x08|0x02, 1, []byte("z"))...) // a retransmission while in flight
	wire = append(wire, 0xD0, 0)
	wire = append(wire, publish311(0x08|0x02, 1, []byte("z"))...) // a retransmission that crossed the PUBACK
	wire = append(wire, 0xD0, 0)
	conn, raw, violations := newTestMQTT311Conn(wire)
	puback := &packets.Puback{PacketID: 1, Properties: &packets.Properties{}}

	assert.Equal(t, []byte("x"), publishV5(t, readV5(t, conn)).Payload)
	require.NoError(t, writeV5(conn, puback))
	assert.Equal(t, []byte("z"), publishV5(t, readV5(t, conn)).Payload, "a first copy without DUP is a new message")
	assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn), "the in-flight retransmission is dropped")
	require.NoError(t, writeV5(conn, puback))
	assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn), "the crossing retransmission is dropped")
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_InFlightIdentifierReusedWithoutDUPIsAViolation(t *testing.T) {
	conn, raw, violations := newTestMQTT311Conn(append(
		publish311(0x02, 1, []byte("x")),
		publish311(0x02, 1, []byte("z"))...,
	))
	readV5(t, conn)
	_, err := conn.Read(make([]byte, 64))
	var ingressErr *mqttIngressError
	require.ErrorAs(t, err, &ingressErr)
	assert.Equal(t, mqttIngressMalformed, ingressErr.kind)
	assert.Len(t, *violations, 1)
	assert.Equal(t, 1, raw.CloseCount())
}

func TestMQTT311Conn_QoS2StaysInFlightUntilPubcomp(t *testing.T) {
	var wire []byte
	wire = append(wire, publish311(0x04, 1, []byte("x"))...)
	wire = append(wire, publish311(0x08|0x04, 1, []byte("x"))...) // DUP after the PUBREC
	wire = append(wire, 0xD0, 0)
	wire = append(wire, publish311(0x04, 1, []byte("z"))...) // a new message after the PUBCOMP
	conn, _, violations := newTestMQTT311Conn(wire)

	assert.Equal(t, []byte("x"), publishV5(t, readV5(t, conn)).Payload)
	require.NoError(t, writeV5(conn, &packets.Pubrec{PacketID: 1, Properties: &packets.Properties{}}))
	assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn), "still in flight after PUBREC, so the DUP is dropped")
	require.NoError(t, writeV5(conn, &packets.Pubcomp{PacketID: 1}))
	assert.Equal(t, []byte("z"), publishV5(t, readV5(t, conn)).Payload)
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_BrokerWindowAboveReceiveMaximumIsRefusedBeforePaho(t *testing.T) {
	var wire []byte
	for id := uint16(1); id <= testMQTT311ReceiveMaximum+1; id++ {
		wire = append(wire, publish311(0x02, id, []byte("x"))...)
	}
	conn, raw, violations := newTestMQTT311Conn(wire)
	for range testMQTT311ReceiveMaximum {
		readV5(t, conn)
	}
	n, err := conn.Read(make([]byte, 64))
	assert.Zero(t, n, "no byte of the packet reaches Paho")
	var ingressErr *mqttIngressError
	require.ErrorAs(t, err, &ingressErr)
	assert.Equal(t, mqttIngressWindowExceeded, ingressErr.kind)
	assert.ErrorIs(t, err, shared.ErrProtocolError)
	assert.Equal(t,
		"mqtt: the broker sent more than receive_maximum 4 unacknowledged QoS 1/2 publishes on MQTT 3.1.1; "+
			"raise receive_maximum to at least the broker's per-client in-flight limit",
		err.Error())
	require.Len(t, *violations, 1)
	assert.Same(t, ingressErr, (*violations)[0])
	assert.Equal(t, 1, raw.CloseCount())
	assert.Empty(t, raw.Written())
}

func TestMQTT311Conn_QoS0IsNotCountedAgainstTheWindow(t *testing.T) {
	var wire []byte
	for id := uint16(1); id <= testMQTT311ReceiveMaximum; id++ {
		wire = append(wire, publish311(0x02, id, []byte("x"))...)
	}
	for range 3 {
		wire = append(wire, publish311(0, 0, []byte("q"))...)
	}
	conn, raw, violations := newTestMQTT311Conn(wire)
	for range testMQTT311ReceiveMaximum + 3 {
		readV5(t, conn)
	}
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_OversizedPublishHandsTheHeadUpBeforeDraining(t *testing.T) {
	payload := bytes.Repeat([]byte{'p'}, 1<<20)
	wire := append(publish311(0x02, 1, payload), 0xD0, 0)
	conn, raw, violations := newTestMQTT311Conn(wire)

	head := publishV5(t, readV5(t, conn))
	assert.Equal(t, uint16(1), head.PacketID)
	assert.Equal(t, payload[:testMQTT311MaxPayloadBytes+1], head.Payload,
		"one byte over the cap, so the router acks and drops it")
	assert.Equal(t, len(payload)-(testMQTT311MaxPayloadBytes+1)+2, raw.UnreadBytes(),
		"nothing past the kept payload is read before the head is handed up")

	assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn))
	assert.Zero(t, raw.UnreadBytes())
	assert.Empty(t, *violations)
}

func TestMQTT311Conn_PayloadAtTheCapIsKeptWhole(t *testing.T) {
	for _, tc := range []struct {
		size, kept int
	}{
		{testMQTT311MaxPayloadBytes, testMQTT311MaxPayloadBytes},
		{testMQTT311MaxPayloadBytes + 1, testMQTT311MaxPayloadBytes + 1},
		{testMQTT311MaxPayloadBytes + 2, testMQTT311MaxPayloadBytes + 1},
	} {
		payload := bytes.Repeat([]byte{'p'}, tc.size)
		conn, raw, _ := newTestMQTT311Conn(append(publish311(0, 0, payload), 0xD0, 0))
		assert.Equal(t, payload[:tc.kept], publishV5(t, readV5(t, conn)).Payload, "payload %d", tc.size)
		assert.Equal(t, []byte{0xD0, 0}, readV5(t, conn), "payload %d", tc.size)
		assert.Zero(t, raw.UnreadBytes())
	}
}

func TestMQTT311Conn_AcksWrittenWhileReadingReleaseTheirIdentifiers(t *testing.T) {
	const window = 64
	var wire []byte
	for range 2 {
		for id := uint16(1); id <= window; id++ {
			wire = append(wire, publish311(0x02, id, []byte("x"))...)
		}
	}
	raw := newTestNetConn(wire, 0)
	var violations []error
	conn := newMQTT311Conn(raw, testMQTT311MaxPayloadBytes, uint32(testMQTT311MaximumPacketSize), window,
		func(err error) { violations = append(violations, err) })

	// Paho acks from the router's goroutine while its reader goes on reading.
	received := make(chan uint16, window)
	acked := make(chan struct{})
	go func() {
		defer close(acked)
		for id := range received {
			assert.NoError(t, writeV5(conn, &packets.Puback{PacketID: id, Properties: &packets.Properties{}}))
		}
	}()
	func() {
		defer close(received)
		for range window {
			received <- publishV5(t, readV5(t, conn)).PacketID
		}
	}()
	<-acked

	for id := uint16(1); id <= window; id++ {
		assert.Equal(t, id, publishV5(t, readV5(t, conn)).PacketID, "a released identifier is accepted again")
	}
	assert.Empty(t, violations)
}

func FuzzMQTT311Inbound(f *testing.F) {
	for _, seed := range [][]byte{
		{0x20, 2, 0, 0},
		append(publish311(0x02, 1, []byte("x")), publish311(0x08|0x02, 1, []byte("x"))...),
		append(publish311(0x04, 2, bytes.Repeat([]byte{'p'}, 1100)), 0xD0, 0),
		{0x90, 3, 0, 1, 0x80},
		{0xB0, 2, 0, 1},
		{0x40, 3, 0, 1, 0x87},
		{0x62, 3, 0, 7, 0x92},
		{0x30, 0x80, 0x80, 0x80, 0x01},
	} {
		f.Add(seed)
	}
	// Large enough for the largest packet the translator hands up, so one
	// Read is one whole packet.
	buf := make([]byte, testMQTT311MaximumPacketSize+3)
	f.Fuzz(func(t *testing.T, wire []byte) {
		conn, _, _ := newTestMQTT311Conn(wire)
		unsubscribe := packets.NewControlPacket(packets.UNSUBSCRIBE)
		unsub := unsubscribe.Content.(*packets.Unsubscribe)
		unsub.PacketID, unsub.Topics = 1, []string{"a", "b"}
		require.NoError(t, writeV5(conn, unsubscribe))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			require.Less(t, n, len(buf))
			cp := decodeV5(t, buf[:n])
			if cp.Type == packets.PUBLISH {
				_, width, err := readMQTTVBIFromBytes(buf[1:n])
				require.NoError(t, err)
				assert.LessOrEqual(t, n-1-width, testMQTT311MaxPayloadBytes+1+65_540,
					"a PUBLISH buffers at most the payload cap plus one byte and the largest variable header")
			}
		}
	})
}
