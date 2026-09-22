package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A receiver topics[] entry decodes its options block through the same
// registry decoder as a session or sender block, so options.subscription is
// read, defaulted and validated on the one typed path every configuration
// takes. The interval paces a re-SUBSCRIBE against the broker, so a value
// below the minimum is refused at parse time rather than loading the broker.
//
// Category: unit (TESTS.md §1).

// topicOptionsWithQoSRecheck is the options block of one topics[] entry that
// sets only subscription.qos_recheck_interval.
func topicOptionsWithQoSRecheck(interval string) map[string]any {
	return map[string]any{
		"subscription": map[string]any{"qos_recheck_interval": interval},
	}
}

func TestRegistryDecode_SubscriptionWithoutQoSRecheckIntervalDefaultsToOneHour(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{})

	assert.Equal(t, time.Hour, cfg.Subscription.QoSRecheckInterval)
}

func TestRegistryDecode_SubscriptionQoSRecheckIntervalHonoursOffMinimumAndLonger(t *testing.T) {
	cases := map[string]time.Duration{
		"0s":  0,
		"1m":  time.Minute,
		"90m": 90 * time.Minute,
	}
	for interval, want := range cases {
		t.Run(interval, func(t *testing.T) {
			cfg := decodeRegistry(t, topicOptionsWithQoSRecheck(interval))

			assert.Equal(t, want, cfg.Subscription.QoSRecheckInterval)
		})
	}
}

func TestRegistryDecode_SubscriptionQoSRecheckIntervalRejectsNegativeAndBelowMinimum(t *testing.T) {
	for _, interval := range []string{"-1s", "30s"} {
		t.Run(interval, func(t *testing.T) {
			reg := ports.NewRegistry()
			require.NoError(t, Register(reg))

			_, err := reg.Decode(ShortKind, parser.NewRawConfig(topicOptionsWithQoSRecheck(interval)))

			var bridgeErr *shared.BridgeError
			require.ErrorAs(t, err, &bridgeErr)
			assert.Equal(t, shared.ErrCodeInvalidConfig, bridgeErr.Code)
			assert.Equal(t, shared.ErrorPermanent, bridgeErr.Class)
		})
	}
}

// TestRegistryDecode_SubscriptionQoSRecheckIntervalRejectsBareNumber pins why
// the documented off value is "0s": a bare number would be read as nanoseconds,
// so the parser refuses it for every duration, including a bare 0.
func TestRegistryDecode_SubscriptionQoSRecheckIntervalRejectsBareNumber(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))

	_, err := reg.Decode(ShortKind, parser.NewRawConfig(map[string]any{
		"subscription": map[string]any{"qos_recheck_interval": 0},
	}))

	require.Error(t, err, "a bare 0 must be written as 0s")
}

// TestSubscriptionOptionsValidate_RejectsNegative pins that the option owns its
// sign check: the validator alone refuses a negative interval.
func TestSubscriptionOptionsValidate_RejectsNegative(t *testing.T) {
	assert.ErrorIs(t, SubscriptionOptions{QoSRecheckInterval: -time.Second}.validate(), shared.ErrInvalidConfig)
}

// TestConfigValidate_NegativeQoSRecheckInterval_DoesNotAdviseZeroForDefault pins
// the wording. The shared duration advice "use 0 for the default" is wrong for
// this option: 0 turns the re-check off, and only omitting it gives the default,
// so an operator following that advice would silently disable probing.
func TestConfigValidate_NegativeQoSRecheckInterval_DoesNotAdviseZeroForDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}
	cfg.Session.ClientID = "negative-qos-recheck"
	cfg.Subscription.QoSRecheckInterval = -time.Second

	err := cfg.Validate()
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.NotContains(t, err.Error(), "use 0 for the default")
}

// TestSubscriptionOptionsValidate_AcceptsOffOrAtLeastMinimum pins the boundary
// on the validator itself, so a rejection above is the value being refused and
// not the strict decoder refusing the key.
func TestSubscriptionOptionsValidate_AcceptsOffOrAtLeastMinimum(t *testing.T) {
	assert.NoError(t, SubscriptionOptions{}.validate(), "0 turns the re-check off")
	assert.NoError(t, SubscriptionOptions{QoSRecheckInterval: MinQoSRecheckInterval}.validate(),
		"the minimum is inclusive")
	assert.ErrorIs(t,
		SubscriptionOptions{QoSRecheckInterval: MinQoSRecheckInterval - time.Nanosecond}.validate(),
		shared.ErrInvalidConfig)
}
