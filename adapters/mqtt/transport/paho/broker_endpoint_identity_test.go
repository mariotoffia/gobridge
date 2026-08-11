package paho

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerIdentityDomain returns the single ownership domain a one-URL config
// resolves to, so equivalent URLs can be compared as one value.
func brokerIdentityDomain(t *testing.T, url string) string {
	t.Helper()
	cfg := durableIdentityConfig()
	cfg.Session.BrokerURLs = []string{url}
	domains, err := cfg.DurableSessionIdentityDomains(connectivity.SessionPersistent)
	require.NoError(t, err, "broker URL %q must canonicalize", url)
	require.Len(t, domains, 1)
	return domains[0]
}

// TestDurableSessionIdentity_EquivalentBrokerURLAliasesCollide proves URLs that
// dial the SAME broker over the SAME transport resolve to one ownership domain.
//
// Two durable sessions written with different spellings of one endpoint —
// tcp:// and mqtt://, an omitted default port, mixed case — hold the same
// broker-side session. Preflight can only reject that pair if the spellings
// collapse to one domain; otherwise both sessions start, disconnect each other
// on the shared client ID, and split their managed subscription history.
func TestDurableSessionIdentity_EquivalentBrokerURLAliasesCollide(t *testing.T) {
	for _, group := range []struct {
		name string
		urls []string
	}{
		{
			name: "plaintext aliases and the default port",
			urls: []string{
				"tcp://broker.example:1883",
				"mqtt://broker.example:1883",
				"mqtt://BROKER.example",
				"tcp://broker.example/ignored?x=1#frag",
			},
		},
		{
			name: "TLS aliases and the default port",
			urls: []string{
				"ssl://broker.example:8883",
				"tls://broker.example:8883",
				"mqtts://broker.example:8883",
				"mqtt+ssl://broker.example:8883",
				"tcps://broker.example:8883",
				"ssl://broker.example",
			},
		},
		{
			name: "websocket default port with an identical path",
			urls: []string{
				"ws://broker.example/mqtt",
				"ws://broker.example:80/mqtt",
			},
		},
	} {
		t.Run(group.name, func(t *testing.T) {
			want := brokerIdentityDomain(t, group.urls[0])
			for _, alias := range group.urls[1:] {
				assert.Equal(t, want, brokerIdentityDomain(t, alias),
					"%q and %q dial the same broker over the same transport", group.urls[0], alias)
			}
		})
	}

	t.Run("aliased durable failover list is one broker-session domain", func(t *testing.T) {
		cfg := durableIdentityConfig()
		cfg.Session.BrokerURLs = []string{"tcp://broker.example:1883", "mqtt://broker.example"}
		require.NoError(t, cfg.ValidateSessionMode(connectivity.SessionPersistent),
			"aliases of one endpoint are not independent broker failover")
	})
}

// TestDurableSessionIdentity_DistinctEndpointsKeepDistinctDomains proves the
// canonical form still separates endpoints that are genuinely different: a
// different transport family, host, non-default port, or websocket path is a
// different broker-side session and must not be collapsed.
func TestDurableSessionIdentity_DistinctEndpointsKeepDistinctDomains(t *testing.T) {
	distinct := []string{
		"tcp://broker.example:1883",
		"ssl://broker.example:1883",
		"tcp://broker.example:1884",
		"tcp://other.example:1883",
		"ws://broker.example/mqtt",
		"wss://broker.example/mqtt",
		"ws://broker.example/other",
	}

	seen := make(map[string]string, len(distinct))
	for _, url := range distinct {
		domain := brokerIdentityDomain(t, url)
		if previous, collide := seen[domain]; collide {
			t.Fatalf("%q and %q are different endpoints but share one ownership domain", previous, url)
		}
		seen[domain] = url
	}
}

// TestDurableSessionIdentity_UnsupportedSchemeIsRejected proves a broker URL the
// dialer cannot honour fails identity resolution instead of producing an
// ownership domain for an endpoint that can never be reached.
func TestDurableSessionIdentity_UnsupportedSchemeIsRejected(t *testing.T) {
	cfg := durableIdentityConfig()
	cfg.Session.BrokerURLs = []string{"amqp://broker.example:5672"}

	_, err := cfg.DurableSessionIdentity(connectivity.SessionPersistent)
	require.Error(t, err, "an undialable scheme must not resolve to a broker identity")
	assert.ErrorContains(t, err, "scheme")
}

// TestDurableSessionIdentity_CanonicalSpellingKeepsStoredHistory pins the
// endpoint form that must not drift: the durable fingerprint keys
// managed-subscription storage, so a URL already written in the canonical
// spelling with an explicit port has to resolve to that exact endpoint. If this
// changes, every deployment written that way loses its stored subscription
// history on upgrade and needs a whole-cohort replacement.
func TestDurableSessionIdentity_CanonicalSpellingKeepsStoredHistory(t *testing.T) {
	for _, url := range []string{
		"tcp://broker.example:1883",
		"ssl://broker.example:8883",
		"ws://broker.example:80/mqtt",
		"wss://broker.example:443/mqtt",
	} {
		endpoint, err := canonicalBrokerEndpoint(url)
		require.NoError(t, err)
		assert.Equal(t, url, endpoint,
			"canonicalizing %q must be identity-preserving; changing it invalidates stored managed-subscription history", url)
	}
}
