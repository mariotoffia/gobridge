package httpapi

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Startup configuration validation and the API-key strength rules it enforces,
// including the fail-closed wrappers around caller-supplied key providers.
const minAPIKeyLen = 16

func (s *Server) validateConfig() error {
	adminKeys := s.currentAdminAPIKeys()
	if len(adminKeys) == 0 {
		return fmt.Errorf("httpapi: admin API key is required; set AdminAPIKey or AdminAPIKeys in Config")
	}
	for name, key := range adminKeys {
		if err := validateAdminKeyEntry(name, len(key.Reveal())); err != nil {
			return err
		}
	}
	monitorKey := s.currentMonitorAPIKey()
	if !monitorKey.IsZero() && len(monitorKey.Reveal()) < minAPIKeyLen {
		return fmt.Errorf("httpapi: monitor API key must be at least %d characters when set", minAPIKeyLen)
	}
	if s.cfg.CORSOrigins == "*" {
		return fmt.Errorf("httpapi: wildcard CORS origin '*' is not allowed; specify explicit origins or leave empty to disable CORS")
	}
	for _, o := range strings.Split(s.cfg.CORSOrigins, ",") {
		if strings.TrimSpace(o) == "*" {
			return fmt.Errorf("httpapi: wildcard CORS origin '*' is not allowed; specify explicit origins or leave empty to disable CORS")
		}
	}
	if (s.cfg.TLSCertFile == "") != (s.cfg.TLSKeyFile == "") {
		return fmt.Errorf("httpapi: TLS requires both tls_cert_file and tls_key_file; set both to enable HTTPS or neither to stay plaintext")
	}
	return nil
}

// validateAdminKeyEntry checks one folded admin key's NAME (tag-safe) and its
// length (>= minAPIKeyLen). Shared by validateConfig (startup, over
// shared.Secret values) and ValidateAdminKeys (reload, over raw strings) so
// both boundaries enforce identical rules. It never returns key material — only
// the name and the length bound appear in the error text.
func validateAdminKeyEntry(name string, keyLen int) error {
	if !validAdminKeyName(name) {
		return fmt.Errorf("httpapi: invalid admin key name %q; must match [a-z0-9._-]+ and be 1-64 chars", name)
	}
	if keyLen < minAPIKeyLen {
		return fmt.Errorf("httpapi: admin API key %q must be at least %d characters", name, minAPIKeyLen)
	}
	return nil
}

// ValidateAdminKeys validates a raw name->key admin map against the same rules
// validateConfig enforces at startup (tag-safe names, per-key minAPIKeyLen
// floor). The composition root MUST call this on every resolved/rotated
// admin-key set so a hot reload cannot install a below-floor key or an unsafe
// name that startup would have rejected. It never logs or returns key material
// (only the name and the length bound). An empty map is allowed here (the
// startup "at least one key" guard belongs to validateConfig); callers that
// require a non-empty set check that separately.
func ValidateAdminKeys(keys map[string]string) error {
	for name, k := range keys {
		if err := validateAdminKeyEntry(name, len(k)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateMonitorKey validates a raw monitor API key against the same floor
// validateConfig enforces at startup: an empty key is allowed (the monitor key
// is optional — an unset key means the monitor plane inherits admin-only auth),
// but a non-empty key must be at least minAPIKeyLen characters. The composition
// root MUST call this on every resolved/rotated monitor key so a hot reload
// cannot install a below-floor key that startup would have rejected. It never
// logs or returns key material (only the length bound appears in the error).
func ValidateMonitorKey(key string) error {
	if key != "" && len(key) < minAPIKeyLen {
		return fmt.Errorf("httpapi: monitor API key must be at least %d characters when set", minAPIKeyLen)
	}
	return nil
}

// validateDynamicAdminKey validates one dynamically-refreshed SINGLE admin key.
// An empty value is legitimate (the single legacy key may be unset when named
// keys carry admin auth — currentAdminAPIKeys folds and skips it); a non-empty
// value must clear the same minAPIKeyLen floor validateConfig enforces at
// startup. It never returns key material (only the length bound appears).
func validateDynamicAdminKey(v string) error {
	if v == "" {
		return nil
	}
	return validateAdminKeyEntry("admin", len(v))
}

// validatedKeyProvider wraps a dynamic single-key provider (admin or monitor)
// with per-refresh strength validation and last-good caching. Startup validates
// the provider output once, but a later rotation could return a weak/invalid key
// that per-request use would otherwise install unchecked. This wrapper FAILS
// CLOSED: a refresh whose value fails validate is rejected and the last VALID
// key is returned instead, so a bad rotation can never install a below-floor key
// after startup. The first call has no last-good fallback; an invalid first
// value yields the zero Secret (all requests rejected) rather than a weak key.
// get is safe for concurrent per-request use.
type validatedKeyProvider struct {
	raw      func() string
	validate func(string) error
	logger   *slog.Logger
	what     string // "admin" or "monitor", for the reject log only

	mu       sync.Mutex
	lastGood shared.Secret
	haveGood bool
}

func (p *validatedKeyProvider) get() shared.Secret {
	v := p.raw()
	if err := p.validate(v); err != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.logger != nil {
			p.logger.Warn("httpapi: rejecting dynamically-refreshed API key that fails validation; keeping last valid key",
				"provider", p.what, "error", err.Error())
		}
		if p.haveGood {
			return p.lastGood
		}
		return shared.Secret{}
	}
	sec := shared.NewSecret(v)
	p.mu.Lock()
	p.lastGood = sec
	p.haveGood = true
	p.mu.Unlock()
	return sec
}

// validatedKeysProvider wraps a dynamic named-admin-key-map provider with the
// same fail-closed semantics as validatedKeyProvider, over the whole set. The
// refreshed map is validated with ValidateAdminKeys (per-key name + length); on
// ANY failure the ENTIRE set is rejected atomically and the last valid set is
// returned, so a single bad entry in a rotation cannot install a weak/unsafe key
// nor drop the good ones piecemeal. get is safe for concurrent per-request use.
type validatedKeysProvider struct {
	raw    func() map[string]string
	logger *slog.Logger

	mu       sync.Mutex
	lastGood map[string]shared.Secret
	haveGood bool
}

func (p *validatedKeysProvider) get() map[string]shared.Secret {
	raw := p.raw()
	if err := ValidateAdminKeys(raw); err != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.logger != nil {
			p.logger.Warn("httpapi: rejecting dynamically-refreshed admin key set that fails validation; keeping last valid set",
				"error", err.Error())
		}
		if p.haveGood {
			return p.lastGood
		}
		return nil
	}
	out := make(map[string]shared.Secret, len(raw))
	for name, k := range raw {
		out[name] = shared.NewSecret(k)
	}
	p.mu.Lock()
	p.lastGood = out
	p.haveGood = true
	p.mu.Unlock()
	return out
}

// validAdminKeyName reports whether name is safe to use as an audit Actor and
// potential metric tag: non-empty, at most 64 bytes, and composed only of
// bytes in the set [a-z0-9._-]. Uppercase, whitespace, slashes, and other
// punctuation are rejected so a key name can never inject structure into a log
// line or a metric tag. All allowed bytes are single-byte ASCII, so a byte
// scan is equivalent to the documented per-rune rule.
func validAdminKeyName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
