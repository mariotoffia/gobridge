package paho

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// DurableSessionIdentity returns an opaque SHA-256 fingerprint of the
// broker-side state selected by this config. Credential and tuning fields are
// deliberately absent. In particular, broker URL userinfo is removed before
// hashing because it is authentication material, not broker identity.
func (c *Config) DurableSessionIdentity(mode connectivity.SessionMode) (string, error) {
	if c == nil {
		return "", errors.New("mqtt: durable session identity unavailable")
	}
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	if mode != connectivity.SessionEphemeral &&
		mode != connectivity.SessionPersistent &&
		mode != connectivity.SessionExclusive {
		return "", errors.New("mqtt: durable session identity has invalid session mode")
	}
	if c.Session.ClientIDSuffix == ClientIDSuffixNonce && mode != connectivity.SessionEphemeral {
		return "", errors.New("mqtt: nonce replica identity is valid only for ephemeral sessions")
	}

	clientID, err := resolveClientIDSuffix(c.Session.ClientID, c.Session.ClientIDSuffix)
	if err != nil {
		return "", errors.New("mqtt: durable session identity cannot resolve client ID suffix")
	}
	brokers, err := canonicalBrokerSet(c.Session.BrokerURLs, c.Session.BrokerURL)
	if err != nil {
		return "", err
	}
	cleanStart, expiry := effectiveSessionState(c.Session, mode)

	// Length-prefix every component so concatenation is unambiguous. The raw
	// descriptor exists only in this stack frame and is never returned/logged.
	var descriptor strings.Builder
	appendIdentityPart(&descriptor, string(mode))
	appendIdentityPart(&descriptor, clientID)
	appendIdentityPart(&descriptor, strconv.FormatBool(cleanStart))
	appendIdentityPart(&descriptor, strconv.FormatUint(uint64(expiry), 10))
	for _, broker := range brokers {
		appendIdentityPart(&descriptor, broker)
	}
	sum := sha256.Sum256([]byte(descriptor.String()))
	return hex.EncodeToString(sum[:]), nil
}

// ReplicaIdentityStrategy reports the configured per-replica client-ID
// strategy without resolving or exposing the effective client ID.
func (c *Config) ReplicaIdentityStrategy() string {
	if c == nil {
		return ""
	}
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
	set := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, errors.New("mqtt: durable session identity cannot canonicalize broker URL")
		}
		u.User = nil
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.RawQuery = u.Query().Encode()
		set[u.String()] = struct{}{}
	}
	canonical := make([]string, 0, len(set))
	for broker := range set {
		canonical = append(canonical, broker)
	}
	slices.Sort(canonical)
	return canonical, nil
}

func appendIdentityPart(dst *strings.Builder, value string) {
	_, _ = fmt.Fprintf(dst, "%d:%s;", len(value), value)
}
