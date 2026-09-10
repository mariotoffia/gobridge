package paho

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ValidateSessionMode rejects durable MQTT failover across independent broker
// session domains. Managed subscription history is keyed by one durable
// identity, so applying it to unrelated brokers could unsubscribe filters on
// one broker based on state established on another. Ephemeral sessions carry no
// durable broker state and may continue to use multi-URL failover.
func (c Config) ValidateSessionMode(mode connectivity.SessionMode) error {
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	if mode == connectivity.SessionEphemeral {
		return nil
	}
	if mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive {
		return errors.New("mqtt: invalid session mode")
	}
	brokers, err := canonicalBrokerSet(c.Session.BrokerURLs, c.Session.BrokerURL)
	if err != nil {
		return err
	}
	domains := make(map[string]struct{}, len(brokers))
	for _, broker := range brokers {
		domains[broker] = struct{}{}
	}
	if len(domains) > 1 {
		return errors.New("mqtt: persistent and exclusive sessions require one broker-session domain; independent multi-broker failover is unsafe for durable managed subscription history")
	}
	return nil
}

// DurableSessionIdentity returns an opaque SHA-256 fingerprint of the
// broker-side state selected by this config. Credential and tuning fields are
// deliberately absent. In particular, broker URL userinfo is removed before
// hashing because it is authentication material, not broker identity.
func (c Config) DurableSessionIdentity(mode connectivity.SessionMode) (string, error) {
	clientID, brokers, normalizedMode, err := c.durableSessionIdentityCoordinates(mode)
	if err != nil {
		return "", err
	}
	cleanStart, expiry := effectiveSessionState(c.Session, normalizedMode)

	// Length-prefix every component so concatenation is unambiguous. The raw
	// descriptor exists only in this stack frame and is never returned/logged.
	var descriptor strings.Builder
	appendIdentityPart(&descriptor, string(normalizedMode))
	appendIdentityPart(&descriptor, clientID)
	appendIdentityPart(&descriptor, strconv.FormatBool(cleanStart))
	appendIdentityPart(&descriptor, strconv.FormatUint(uint64(expiry), 10))
	for _, broker := range brokers {
		appendIdentityPart(&descriptor, broker)
	}
	return identityDigest(descriptor.String()), nil
}

// DurableSessionIdentityDomains returns one opaque ownership key for each
// canonical broker endpoint paired with the effective client ID. The ordered
// whole-list fingerprint remains separate so failover-order changes are still
// detected, while overlapping failover lists collide on their shared endpoint.
func (c Config) DurableSessionIdentityDomains(mode connectivity.SessionMode) ([]string, error) {
	clientID, brokers, _, err := c.durableSessionIdentityCoordinates(mode)
	if err != nil {
		return nil, err
	}
	domains := make([]string, 0, len(brokers))
	for _, broker := range brokers {
		var descriptor strings.Builder
		appendIdentityPart(&descriptor, clientID)
		appendIdentityPart(&descriptor, broker)
		domains = append(domains, identityDigest(descriptor.String()))
	}
	return domains, nil
}

func (c Config) durableSessionIdentityCoordinates(mode connectivity.SessionMode) (string, []string, connectivity.SessionMode, error) {
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	if mode != connectivity.SessionEphemeral &&
		mode != connectivity.SessionPersistent &&
		mode != connectivity.SessionExclusive {
		return "", nil, "", errors.New("mqtt: durable session identity has invalid session mode")
	}
	if err := c.ValidateSessionMode(mode); err != nil {
		return "", nil, "", err
	}
	if c.Session.ClientIDSuffix == ClientIDSuffixNonce && mode != connectivity.SessionEphemeral {
		return "", nil, "", errors.New("mqtt: nonce replica identity is valid only for ephemeral sessions")
	}
	clientID, err := c.resolveClientIDSuffix(c.Session.ClientID, c.Session.ClientIDSuffix)
	if err != nil {
		return "", nil, "", fmt.Errorf("mqtt: durable session identity cannot resolve client ID suffix: %w", err)
	}
	brokers, err := canonicalBrokerSet(c.Session.BrokerURLs, c.Session.BrokerURL)
	if err != nil {
		return "", nil, "", err
	}
	return clientID, brokers, mode, nil
}

func identityDigest(descriptor string) string {
	sum := sha256.Sum256([]byte(descriptor))
	return hex.EncodeToString(sum[:])
}

// ReplicaIdentityStrategy reports the configured per-replica client-ID
// strategy without resolving or exposing the effective client ID.
func (c Config) ReplicaIdentityStrategy() string {
	return c.Session.ClientIDSuffix
}

func effectiveSessionState(opts SessionOptions, mode connectivity.SessionMode) (bool, uint32) {
	switch mode {
	case connectivity.SessionEphemeral:
		return true, 0
	case connectivity.SessionExclusive:
		expiry := opts.SessionExpiryInterval
		if expiry == 0 {
			expiry = DefaultPersistentSessionExpiry
		}
		return false, expiry
	default: // persistent; mode was validated by DurableSessionIdentity.
		expiry := opts.SessionExpiryInterval
		if expiry == 0 {
			expiry = DefaultPersistentSessionExpiry
		}
		return opts.CleanStart, expiry
	}
}

func canonicalBrokerSet(brokerURLs []string, brokerURL string) ([]string, error) {
	urls := append([]string(nil), brokerURLs...)
	if len(urls) == 0 && brokerURL != "" {
		urls = append(urls, brokerURL)
	}
	canonical := make([]string, 0, len(urls))
	for _, raw := range urls {
		endpoint, err := canonicalBrokerEndpoint(raw)
		if err != nil {
			return nil, err
		}
		canonical = append(canonical, endpoint)
	}
	return canonical, nil
}

// canonicalBrokerEndpoint reduces a broker URL to the endpoint the dialer will
// actually reach, so two spellings of one endpoint compare equal.
//
// Ownership of a broker-side session is decided by what the connection reaches,
// not by how the URL was written. tcp:// and mqtt:// select the same dialer, an
// omitted port means the family's default, and host case, userinfo and fragment
// change nothing about the connection. Left uncollapsed, two durable sessions
// spelled differently pass the duplicate-identity preflight, then disconnect
// each other on their shared client ID and split their managed subscription
// history. Path and query survive only for the WebSocket families, where they
// address distinct broker endpoints.
func canonicalBrokerEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("mqtt: durable session identity cannot canonicalize broker URL")
	}
	family, defaultPort, addressed := brokerDialFamily(u.Scheme)
	if family == "" {
		return "", fmt.Errorf("mqtt: durable session identity has unsupported broker URL scheme %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("mqtt: durable session identity requires a broker URL host")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	endpoint := family + "://" + net.JoinHostPort(host, port)
	if !addressed {
		return endpoint, nil
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	endpoint += path
	if query := u.Query().Encode(); query != "" {
		endpoint += "?" + query
	}
	return endpoint, nil
}

// brokerDialFamily maps a broker URL scheme to the transport the adapter
// actually dials, the port assumed when the URL omits one, and whether the URL
// path/query address the endpoint (true only for the WebSocket families, where
// the broker listens on a specific path).
//
// It is the single list of supported schemes: dialMQTTConnection selects its
// dialer from the same families, so a scheme accepted at identity preflight is
// exactly a scheme the adapter can connect with.
//
// The family names are the dominant spelling of each group (tcp, ssl, ws, wss)
// rather than invented labels, because the canonical endpoint feeds the durable
// session fingerprint that keys managed-subscription storage. A URL already
// written in that spelling with an explicit port keeps the identity it had
// before endpoints were canonicalized, so the common configurations carry their
// stored history forward.
func brokerDialFamily(scheme string) (family, defaultPort string, addressed bool) {
	switch strings.ToLower(scheme) {
	case "", "mqtt", "tcp":
		return "tcp", "1883", false
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		return "ssl", "8883", false
	case "ws":
		return "ws", "80", true
	case "wss":
		return "wss", "443", true
	default:
		return "", "", false
	}
}

func appendIdentityPart(dst *strings.Builder, value string) {
	_, _ = fmt.Fprintf(dst, "%d:%s;", len(value), value)
}
