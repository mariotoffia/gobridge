package bridgecfg

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// MQTTOption mutates the *paho.Config the builder produces for a
// session. Options are applied after the initial broker URL and
// keep-alive defaults are seeded so a later option may override
// either.
type MQTTOption func(*paho.Config)

// WithMQTTBroker adds an MQTT broker session under the given session
// id pointing at brokerURL. The MQTT transport in this codebase is
// the Eclipse Paho adapter (kind "mqtt.paho"); the short form
// "mqtt" is registered alongside.
//
// The session is seeded with DefaultMQTTKeepAlive and
// DefaultMQTTConnectTimeout. Operators that need other values pass
// MQTTOption helpers (WithMQTTKeepAlive, MQTTCredsFromSSM, …).
func (b *Builder) WithMQTTBroker(sessionID, brokerURL string, opts ...MQTTOption) *Builder {
	if !b.reserveID(b.sessionIDs, "session", sessionID) {
		return b
	}
	if brokerURL == "" {
		b.fail(fmt.Errorf("bridgecfg: session %q: broker url must not be empty", sessionID))
		return b
	}
	cfg := &paho.Config{
		Session: paho.SessionOptions{
			BrokerURLs:     []string{brokerURL},
			ClientID:       sessionID,
			KeepAlive:      DefaultMQTTKeepAlive,
			ConnectTimeout: DefaultMQTTConnectTimeout,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if err := cfg.Validate(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: session %q: %w", sessionID, err))
		return b
	}
	def := ports.SessionDef{ID: sessionID, Transport: mqttTransport}
	def.SetDecoded(cfg, nil)
	b.cfg.Sessions = append(b.cfg.Sessions, def)
	return b
}

// MQTTCredsFromSSM returns an MQTTOption that wires the session's
// CredentialsURIRef to a pms:// URI derived from ref.Name(). The
// runtime credential resolver consumes the URI at startup and feeds
// the resolved username/password into the MQTT session. Inline
// credentials remain rejected by ScanForPlaintextSecrets — this is
// the only supported way to thread MQTT auth through the CDK
// pipeline.
//
// The ref is captured by name; an unresolved ref still produces a
// well-formed URI. The aggregated annotation pass reports the missing SSM
// registration with an actionable annotation so the operator sees
// every miss in a single synth pass.
func MQTTCredsFromSSM(ref registry.ParamRef) MQTTOption {
	return func(c *paho.Config) {
		c.CredentialsURIRef = paramRefToPMS(ref)
	}
}

// WithMQTTKeepAlive overrides the keep-alive (in seconds) on a
// session.
func WithMQTTKeepAlive(seconds uint16) MQTTOption {
	return func(c *paho.Config) { c.Session.KeepAlive = seconds }
}

// WithMQTTConnectTimeout overrides the connect timeout on a session.
func WithMQTTConnectTimeout(d time.Duration) MQTTOption {
	return func(c *paho.Config) { c.Session.ConnectTimeout = d }
}

// WithMQTTClientID overrides the auto-derived client ID (which
// defaults to the session ID).
func WithMQTTClientID(clientID string) MQTTOption {
	return func(c *paho.Config) { c.Session.ClientID = clientID }
}

// mqttTransport is the registered short discriminator for the paho
// MQTT adapter.
const mqttTransport = paho.ShortKind

// paramRefToPMS converts an SsmParamRegistry ref's logical name into
// the canonical pms:// URI form. The repository contract (see
// adapters/aws/credentials/ssm) strips the leading slash from the
// SSM path before composing the URI host so "/bridge/mqtt" becomes
// "pms://bridge/mqtt".
func paramRefToPMS(ref registry.ParamRef) string {
	name := strings.TrimPrefix(ref.Name(), "/")
	return "pms://" + name
}
