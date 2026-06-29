package bridgecfg

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"gopkg.in/yaml.v3"
)

// credentialURIAlternative is the suggested replacement printed in
// every violation message. Kept as a single constant so tests and
// docs stay aligned with the wording the scanner emits.
const credentialURIAlternative = "use a credential URI such as pms://<your-ssm-path> instead of an inline value"

var (
	registryMu sync.RWMutex

	// credentialSchemes is the allow-list of URI schemes the scanner
	// recognises as valid credential references. Pre-populated in
	// init() with the schemes shipped by the repository today
	// (pms, file). Adapters register additional schemes via
	// RegisterCredentialScheme.
	credentialSchemes = map[string]struct{}{}

	// sensitiveFields is the curated set of yaml key names whose
	// values are treated as credentials whenever encountered in a
	// plugin config payload. All entries are lower-case; lookup is
	// done after lower-casing the encountered key.
	sensitiveFields = map[string]struct{}{}
)

func init() {
	for _, s := range []string{"pms", "file"} {
		credentialSchemes[s] = struct{}{}
	}
	for _, f := range []string{
		"password",
		"secret",
		"api_key",
		"apikey",
		"client_secret",
		"bearer_token",
		"private_key",
		"privatekey",
		"token",
		"auth_token",
		"access_token",
		"refresh_token",
		"passphrase",
	} {
		sensitiveFields[f] = struct{}{}
	}
}

// RegisterCredentialScheme adds scheme to the allow-list of
// credential URI schemes recognised by ScanForPlaintextSecrets.
// Registration is idempotent: re-registering a scheme already in the
// list is a no-op so adapter init() functions may register defensively
// without coordinating with each other. An empty scheme is always a
// programming error and panics.
func RegisterCredentialScheme(scheme string) {
	if scheme == "" {
		panic("bridgecfg: credential scheme must not be empty")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	credentialSchemes[scheme] = struct{}{}
}

// CredentialSchemes returns a sorted snapshot of the registered
// credential URI schemes. Intended for diagnostics and tests; the
// returned slice is owned by the caller.
func CredentialSchemes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(credentialSchemes))
	for s := range credentialSchemes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// IsCredentialURI reports whether value parses as <scheme>://<rest>
// with scheme in the allow-list and rest non-empty. The check is
// intentionally simple — full URI parsing is left to the credential
// resolver at runtime; the scanner only needs to distinguish "looks
// like a URI we recognise" from "literal secret value".
func IsCredentialURI(value string) bool {
	idx := strings.Index(value, "://")
	if idx <= 0 {
		return false
	}
	scheme := value[:idx]
	rest := value[idx+len("://"):]
	if rest == "" {
		return false
	}
	registryMu.RLock()
	_, ok := credentialSchemes[scheme]
	registryMu.RUnlock()
	return ok
}

// RegisterSensitiveField adds name to the set of yaml keys treated as
// credential-bearing. The name is lower-cased before insertion;
// matching during scanning is case-insensitive. Registration is
// idempotent. An empty name panics.
func RegisterSensitiveField(name string) {
	if name == "" {
		panic("bridgecfg: sensitive field name must not be empty")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	sensitiveFields[strings.ToLower(name)] = struct{}{}
}

// SensitiveFieldNames returns a sorted snapshot of the registered
// sensitive yaml key names. The returned slice is owned by the caller.
func SensitiveFieldNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(sensitiveFields))
	for f := range sensitiveFields {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	registryMu.RLock()
	_, ok := sensitiveFields[k]
	registryMu.RUnlock()
	return ok
}

// ScanForPlaintextSecrets walks every component of cfg that may carry
// credentials and returns a single aggregated error describing every
// field that contains a non-empty literal value where a credential
// URI was expected. A nil cfg or a config without any violations
// returns nil. See the package doc for the full policy.
func ScanForPlaintextSecrets(cfg *ports.BridgeConfig) error {
	if cfg == nil {
		return nil
	}

	var violations []error

	// Top-level HTTP keys are shared.Secret value objects; Reveal the
	// underlying value so the scanner can verify it is a credential URI
	// rather than an inline plaintext literal.
	if cfg.HTTP != nil {
		violations = appendIfPlaintext(violations, "http.admin_api_key", cfg.HTTP.AdminAPIKey.Reveal())
		violations = appendIfPlaintext(violations, "http.monitor_api_key", cfg.HTTP.MonitorAPIKey.Reveal())
	}

	for i := range cfg.Sessions {
		s := &cfg.Sessions[i]
		path := fmt.Sprintf("sessions[%d].config", i)
		violations = scanPluginPayload(violations, path, s.Config, s.Raw())
	}
	for i := range cfg.Receivers {
		r := &cfg.Receivers[i]
		path := fmt.Sprintf("receivers[%d].config", i)
		violations = scanPluginPayload(violations, path, r.Config, r.Raw())
		for j := range r.Topics {
			t := &r.Topics[j]
			tp := fmt.Sprintf("receivers[%d].topics[%d].config", i, j)
			violations = scanPluginPayload(violations, tp, t.Config, t.Raw())
		}
	}
	for i := range cfg.Senders {
		s := &cfg.Senders[i]
		path := fmt.Sprintf("senders[%d].config", i)
		violations = scanPluginPayload(violations, path, s.Config, s.Raw())
	}
	for i := range cfg.Bindings {
		b := &cfg.Bindings[i]
		path := fmt.Sprintf("bindings[%d].config", i)
		violations = scanPluginPayload(violations, path, b.Config, b.Raw())
	}

	violations = scanStore(violations, "stores.lease", cfg.Stores.Lease)
	violations = scanStore(violations, "stores.outbox", cfg.Stores.Outbox)
	violations = scanStore(violations, "stores.dlq", cfg.Stores.DLQ)

	if len(violations) == 0 {
		return nil
	}
	return errors.Join(violations...)
}

func scanStore(into []error, path string, sc *ports.StoreConfig) []error {
	if sc == nil {
		return into
	}
	return scanPluginPayload(into, path+".config", sc.Config, sc.Raw())
}

// scanPluginPayload prefers the typed PluginConfig (round-tripped
// through yaml.Marshal to obtain a generic map keyed by yaml tag) and
// falls back to RawConfig.Decode when the typed value is nil. When
// both are nil there is nothing to scan — hand-built test configs
// without a decoded payload are intentionally skipped.
func scanPluginPayload(into []error, path string, cfg ports.PluginConfig, raw ports.RawConfig) []error {
	payload, ok := decodeForScan(cfg, raw)
	if !ok {
		return into
	}
	return walkValue(into, path, payload)
}

func decodeForScan(cfg ports.PluginConfig, raw ports.RawConfig) (any, bool) {
	if cfg != nil {
		// Reveal shared.Secret values so the scanner inspects the REAL value:
		// a credential URI (pms://…) must be recognised as compliant, and a
		// genuine inline plaintext must still be flagged. Without revealing,
		// every secret would marshal to "[REDACTED]" and the scanner would
		// false-positive on compliant credential-URI configs.
		data, err := yaml.Marshal(shared.RevealSecrets(cfg))
		if err == nil {
			var out any
			if uerr := yaml.Unmarshal(data, &out); uerr == nil {
				return out, true
			}
		}
		// fall through to raw on marshal/unmarshal failure
	}
	if raw != nil {
		var out any
		if err := raw.Decode(&out); err == nil {
			return out, true
		}
	}
	return nil, false
}

// walkValue recursively descends maps and slices building dotted/
// JSON-pointer-ish paths and reports a violation each time a
// sensitive key is bound to a non-empty literal string that is not a
// credential URI. Numbers, booleans, time values and other scalars
// are ignored — only string leaves can carry secrets.
func walkValue(into []error, path string, v any) []error {
	switch val := v.(type) {
	case map[string]any:
		// Stable order so test assertions and operator output are
		// deterministic.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := val[k]
			childPath := path + "." + strings.ToLower(k)
			if isSensitiveKey(k) {
				if s, isString := child.(string); isString {
					into = appendIfPlaintext(into, childPath, s)
					continue
				}
			}
			into = walkValue(into, childPath, child)
		}
	case map[any]any:
		// yaml.v3 generally returns map[string]any for mapping nodes
		// when decoded into `any`, but defensively handle the legacy
		// shape too.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, fmt.Sprintf("%v", k))
		}
		sort.Strings(keys)
		for _, ks := range keys {
			var child any
			for k, c := range val {
				if fmt.Sprintf("%v", k) == ks {
					child = c
					break
				}
			}
			childPath := path + "." + strings.ToLower(ks)
			if isSensitiveKey(ks) {
				if s, isString := child.(string); isString {
					into = appendIfPlaintext(into, childPath, s)
					continue
				}
			}
			into = walkValue(into, childPath, child)
		}
	case []any:
		for i, item := range val {
			into = walkValue(into, fmt.Sprintf("%s[%d]", path, i), item)
		}
	}
	return into
}

func appendIfPlaintext(into []error, path, value string) []error {
	if value == "" {
		return into
	}
	if IsCredentialURI(value) {
		return into
	}
	return append(into, fmt.Errorf(
		"bridgecfg: plaintext secret in field %s: %s",
		path, credentialURIAlternative,
	))
}
