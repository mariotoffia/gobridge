package paho

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ClientIDSuffixHostname and ClientIDSuffixNonce are the supported
// client_id_suffix tokens (M-3).
const (
	ClientIDSuffixHostname = "hostname"
	ClientIDSuffixNonce    = "nonce"
)

// resolveClientIDSuffix returns base with the suffix token expanded and
// appended ("-<value>"), producing a per-replica-unique client_id for
// $share scale-out from a single shared config file (M-3). An empty suffix
// returns base unchanged. An unsupported token, or a failed hostname
// lookup, returns an error so the misconfiguration fails the build rather
// than silently colliding client_ids. Callers must enforce mode-specific
// suffix restrictions before calling this (see factory.NewSession).
type clientIDSuffixProcessIdentity struct {
	hostnameOnce sync.Once
	hostname     string
	hostnameErr  error
	nonceOnce    sync.Once
	nonce        string
	nonceErr     error
	random       io.Reader
}

// processClientIDSuffixIdentity intentionally has process scope: independently
// decoded reload configs must resolve the same effective ID. Config value copies
// retain an explicit state pointer when one is supplied.
var processClientIDSuffixIdentity = clientIDSuffixProcessIdentity{random: rand.Reader} //nolint:gochecknoglobals

func (c Config) suffixIdentity() *clientIDSuffixProcessIdentity {
	if c.clientIDSuffixIdentity != nil {
		return c.clientIDSuffixIdentity
	}
	return &processClientIDSuffixIdentity
}

func (c Config) resolveClientIDSuffix(base, suffix string) (string, error) {
	return c.suffixIdentity().resolve(base, suffix)
}

// resolveClientIDSuffix preserves the package-level helper for callers that do
// not have a Config. Config capabilities and the factory use the state carried
// by Config so copied values cannot bypass suffix stability.
func resolveClientIDSuffix(base, suffix string) (string, error) {
	return processClientIDSuffixIdentity.resolve(base, suffix)
}

func (identity *clientIDSuffixProcessIdentity) resolve(base, suffix string) (string, error) {
	switch suffix {
	case "":
		return base, nil
	case ClientIDSuffixHostname:
		identity.hostnameOnce.Do(func() {
			identity.hostname, identity.hostnameErr = os.Hostname()
			if identity.hostnameErr == nil && identity.hostname == "" {
				identity.hostnameErr = errors.New("hostname is empty")
			}
		})
		if identity.hostnameErr != nil {
			return "", fmt.Errorf("client_id_suffix %q: hostname lookup failed: %w", suffix, identity.hostnameErr)
		}
		return base + "-" + identity.hostname, nil
	case ClientIDSuffixNonce:
		identity.nonceOnce.Do(func() {
			random := identity.random
			if random == nil {
				random = rand.Reader
			}
			identity.nonce, identity.nonceErr = randomClientNonce(random)
		})
		if identity.nonceErr != nil {
			return "", fmt.Errorf("client_id_suffix %q: nonce entropy failed: %w", suffix, identity.nonceErr)
		}
		return base + "-" + identity.nonce, nil
	default:
		return "", fmt.Errorf("client_id_suffix %q: unsupported (want %q or %q)",
			suffix, ClientIDSuffixHostname, ClientIDSuffixNonce)
	}
}

// randomClientNonce returns 128 bits of cryptographic entropy as hex. Entropy
// failure is returned to the caller; silently deriving a weaker fallback could
// collide replica client IDs and cause broker mutual-takeover storms.
func randomClientNonce(random io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("read nonce entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}
