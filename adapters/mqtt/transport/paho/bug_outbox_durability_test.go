// F-3 (egress durability model). Autopaho keeps outbound QoS 1/2 packet state
// IN MEMORY, so a publish in flight at process death is lost and QoS 2 is not
// exactly-once across a restart. The old adapter surfaced this as a blanket
// per-session WARN at every QoS 1/2 sender build. That advisory was imprecise:
// it fired even for routes whose delivery mode gates the SOURCE acknowledgement
// behind the egress durability boundary (direct_hold acks only after
// PUBACK/PUBCOMP; shared_outbox only after a version-fenced outbox persist), so
// an in-flight publish lost at crash causes NO bridge-level loss — the source
// redelivers or the outbox replays. Telling those operators their data is
// "LOST" was misleading.
//
// The blanket WARN is therefore removed. The Sender instead reports its egress
// durability honestly via ports.NonDurableEgressReporter, and the bridge builder
// decides per route whether an advisory is warranted (see
// bridge.egressDurabilityAdvisory). These tests pin the adapter half of that
// contract: the factory no longer warns, and NonDurableEgress tracks QoS.
package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

const outboxAdvisorySubstr = "IN-MEMORY"

// TestBug_NewSender_NoBlanketOutboxWarn proves the imprecise blanket
// egress-durability advisory is gone: building QoS 1 and QoS 2 senders emits no
// per-session WARN. Egress durability is now judged per route by the bridge,
// not blindly at sender-build time.
func TestBug_NewSender_NoBlanketOutboxWarn(t *testing.T) {
	logs := &recordingLogHandler{}
	f := &Factory{Logger: slog.New(logs)}
	sess := NewSession(SessionOptions{
		BrokerURLs:            []string{"tcp://broker:1883"},
		ClientID:              "outbox-warn",
		SessionExpiryInterval: 3600,
	}, connectivity.SessionPersistent, slog.New(logs))

	for _, qos := range []byte{1, 2} {
		_, err := f.NewSender(context.Background(), ports.SenderSpec{
			ID:     "snd",
			Config: Config{Sender: SenderOptions{QoS: qos}},
		}, sess)
		require.NoError(t, err)
	}

	require.Equal(t, 0, logs.warnCountContaining(outboxAdvisorySubstr),
		"the blanket QoS 1/2 egress-durability WARN must be removed; the bridge decides per route")
}

// TestSender_NonDurableEgress_TracksQoS pins the ports.NonDurableEgressReporter
// contract the bridge relies on: QoS 1/2 senders declare non-durable egress
// (in-memory packet state can lose an accepted publish at crash), while a QoS 0
// sender makes no delivery claim and declares durable-by-vacuity false.
func TestSender_NonDurableEgress_TracksQoS(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://broker:1883"},
		ClientID:   "egress-report",
	}, connectivity.SessionEphemeral, nil)

	cases := []struct {
		qos  byte
		want bool
	}{
		{qos: 0, want: false},
		{qos: 1, want: true},
		{qos: 2, want: true},
	}
	for _, tc := range cases {
		s := NewSender(sess, SenderOptions{QoS: tc.qos})
		require.Equal(t, tc.want, s.NonDurableEgress(),
			"QoS %d: NonDurableEgress must report %v", tc.qos, tc.want)
	}
}
