package bridge

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// egressReportingSender is a minimal ports.Sender that also implements
// ports.NonDurableEgressReporter, so the advisory helper can be exercised
// without a real transport adapter.
type egressReportingSender struct {
	nonDurable bool
}

func (s *egressReportingSender) Send(context.Context, ports.OutboundMessage) error { return nil }
func (s *egressReportingSender) NonDurableEgress() bool                            { return s.nonDurable }

var _ ports.NonDurableEgressReporter = (*egressReportingSender)(nil)

// plainSender implements only ports.Sender (no egress reporting). The helper
// must treat it as durable-egress and never advise.
type plainSender struct{}

func (plainSender) Send(context.Context, ports.OutboundMessage) error { return nil }

// syntheticEarlyAckMode is a delivery mode that does NOT exist in production. It
// stands in for a hypothetical future mode that acks the source BEFORE the
// egress durability boundary, proving the forward-guard trips for such a mode.
const syntheticEarlyAckMode routing.DeliveryMode = "synthetic_early_ack"

// TestEgressDurabilityAdvisory_TruthTable pins the full decision table of the
// advisory helper. It proves the advisory is SILENT for both real delivery
// modes (they gate the source ack behind egress durability) and fires ONLY for
// a non-durable-egress sender on a mode that acks the source early.
func TestEgressDurabilityAdvisory_TruthTable(t *testing.T) {
	nonDurable := &egressReportingSender{nonDurable: true}
	durable := &egressReportingSender{nonDurable: false}

	cases := []struct {
		name   string
		mode   routing.DeliveryMode
		sender ports.Sender
		want   bool
	}{
		{
			name:   "non-durable egress on direct_hold is gated (silent)",
			mode:   routing.DeliveryDirectHold,
			sender: nonDurable,
			want:   false,
		},
		{
			name:   "non-durable egress on shared_outbox is gated (silent)",
			mode:   routing.DeliverySharedOutbox,
			sender: nonDurable,
			want:   false,
		},
		{
			// A route that omits delivery_mode carries an empty string here;
			// the runtime defaults it to direct_hold (RoutePolicy.WithDefaults),
			// so the advisory MUST treat it as the loss-safe default and stay
			// silent — otherwise every QoS>=1 MQTT route without an explicit
			// delivery_mode would trip a spurious startup WARN.
			name:   "non-durable egress on an unset mode defaults to direct_hold (silent)",
			mode:   routing.DeliveryMode(""),
			sender: nonDurable,
			want:   false,
		},
		{
			name:   "durable egress never advises, even on an early-ack mode",
			mode:   syntheticEarlyAckMode,
			sender: durable,
			want:   false,
		},
		{
			name:   "sender that does not report egress durability is treated as durable",
			mode:   syntheticEarlyAckMode,
			sender: plainSender{},
			want:   false,
		},
		{
			name:   "non-durable egress on an early-ack mode trips the forward-guard",
			mode:   syntheticEarlyAckMode,
			sender: nonDurable,
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressDurabilityAdvisory(tc.mode, tc.sender); got != tc.want {
				t.Errorf("egressDurabilityAdvisory(%q, ...) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}
