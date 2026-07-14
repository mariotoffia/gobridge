package paho

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

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
		u, err := url.Parse(raw)
		if err != nil {
			return nil, errors.New("mqtt: durable session identity cannot canonicalize broker URL")
		}
		u.User = nil
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.RawQuery = u.Query().Encode()
		canonical = append(canonical, u.String())
	}
	return canonical, nil
}

func appendIdentityPart(dst *strings.Builder, value string) {
	_, _ = fmt.Fprintf(dst, "%d:%s;", len(value), value)
}
