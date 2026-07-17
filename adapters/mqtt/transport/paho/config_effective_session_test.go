package paho

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func TestConfigValidateEffectiveSession_RejectsMissingBrokerURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ClientID = "stable-ha-client"
	cfg.Session.BrokerURLs = nil

	err := cfg.ValidateEffectiveSession(connectivity.SessionExclusive)
	require.ErrorContains(t, err, "at least one broker URL is required")
}

func TestConfigValidateEffectiveSession_RejectsIndependentDurableBrokerDomains(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ClientID = "stable-ha-client"
	cfg.Session.BrokerURLs = []string{"tls://mqtt-a.example:8883", "tls://mqtt-b.example:8883"}

	err := cfg.ValidateEffectiveSession(connectivity.SessionExclusive)
	require.ErrorContains(t, err, "one broker-session domain")
}

func TestConfigValidateEffectiveSession_AcceptsStableExclusiveIdentity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ClientID = "stable-ha-client"
	cfg.Session.BrokerURLs = []string{"tls://mqtt.example:8883"}

	require.NoError(t, cfg.ValidateEffectiveSession(connectivity.SessionExclusive))
}
