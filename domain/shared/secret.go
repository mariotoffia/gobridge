package shared

import (
	"log/slog"
	"strings"
)

// redactedMarker is the placeholder emitted by every Secret redaction
// surface in place of the wrapped value.
const redactedMarker = "[REDACTED]"

// Revealed returns a copy of s whose marshal surfaces (JSON/YAML/Text) emit the
// REAL value instead of "[REDACTED]". The reveal intent travels WITH the value
// (an unexported flag on the copy), so it is fully concurrency-safe: a default,
// redacting json.Marshal/yaml.Marshal running in parallel on a different value
// is unaffected — there is no process-global state. Display/log surfaces
// (String/GoString/LogValue) ALWAYS redact regardless of the flag.
//
// Confine Revealed to the authoritative persistence serializers (config save,
// credential store, deploy synthesis), which build a revealed copy of the
// value at the save boundary (see config/parser RevealSecrets).
func (s Secret) Revealed() Secret { return Secret{value: s.value, reveal: true} }

// rendered returns the value to emit on a generic marshal surface: the real
// value when this copy is revealed, otherwise "" for the zero secret or the
// redaction marker.
func (s Secret) rendered() string {
	if s.reveal {
		return s.value
	}
	return s.String()
}

// Secret is an immutable value object wrapping a sensitive string such as a
// password, private key, or token. Its zero value is a valid empty secret.
//
// Redaction is STRUCTURAL on every generic marshal/format surface, not just
// the display ones. String, GoString, LogValue, MarshalJSON, MarshalText and
// MarshalYAML all emit "[REDACTED]" so accidental leakage through fmt, slog,
// json.Marshal, or yaml.Marshal is structurally impossible. encoding/json
// prefers json.Marshaler over encoding.TextMarshaler, so MarshalJSON makes
// every json.Marshal redact; MarshalYAML does the same for yaml.v3.
//
// PERSISTENCE is the deliberate exception. Config/credential serializers that
// must write the REAL value to a durable artifact (blueprint save/load,
// deploy synthesis, credential stores) reveal EXPLICITLY at the save boundary —
// via Reveal() on a scalar field, or RevealSecrets/Revealed for whole-config
// marshalling — instead of relying on the marshaller. Decode is unaffected:
// UnmarshalText still reads a scalar string into a real Secret, so a persisted
// scalar round-trips back into a usable value.
//
// value-object
type Secret struct {
	value string
	// reveal, when true on a copy produced by Revealed(), makes the generic
	// marshal surfaces emit the real value. It NEVER affects String/GoString/
	// LogValue. Unexported so it cannot be set from outside this package.
	reveal bool
}

// NewSecret wraps v in a Secret.
func NewSecret(v string) Secret { return Secret{value: v} }

// RedactedSecret returns the canonical redaction-marker secret that read
// boundaries (e.g. the admin config GET) emit in place of a real value, so
// callers never hard-code the marker string. Merge and write paths use
// IsRedacted to avoid persisting this marker over a real stored secret.
func RedactedSecret() Secret { return Secret{value: redactedMarker} }

// Reveal returns the raw secret value. This is the ONLY accessor that exposes
// the underlying bytes and MUST be confined to adapter boundaries.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret carries no value.
func (s Secret) IsZero() bool { return s.value == "" }

// Equal reports whether two secrets wrap the same value.
func (s Secret) Equal(other Secret) bool { return s.value == other.value }

// IsRedacted reports whether s carries the redaction marker rather than a real
// value — i.e. it was echoed back from a redacted read (admin GET). Merge and
// write paths use this so a client that round-trips a sanitized config never
// overwrites the stored secret with "[REDACTED]".
func (s Secret) IsRedacted() bool { return s.value == redactedMarker }

// String returns a redacted representation: the empty string for the zero
// secret, the redaction marker otherwise. It never exposes the value.
func (s Secret) String() string {
	if s.value == "" {
		return ""
	}
	return redactedMarker
}

// GoString mirrors String so %#v formatting also redacts.
func (s Secret) GoString() string { return s.String() }

// MarshalJSON redacts: json.Marshal of a populated Secret (or any struct
// embedding one) yields "[REDACTED]"; the zero secret yields "". encoding/json
// prefers json.Marshaler over encoding.TextMarshaler, so this makes every
// json.Marshal path safe. Persistence reveals by marshalling a Revealed() copy
// (see Revealed / RevealSecrets). domain/shared is stdlib-only and may not
// import encoding/json, so the JSON string is escaped by hand.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(jsonQuote(s.rendered())), nil }

// jsonQuote returns v as a valid, double-quoted JSON string. It is a minimal
// escaper for the only encoder the domain layer needs (Secret.MarshalJSON):
// domain/shared cannot import encoding/json. It escapes the mandatory JSON
// characters and emits \uXXXX for control runes.
func jsonQuote(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// MarshalYAML redacts: yaml.v3 honours yaml.Marshaler before the TextMarshaler
// fallback, so yaml.Marshal of a populated Secret yields "[REDACTED]" and the
// zero secret yields "". Persistence reveals via Revealed / RevealSecrets.
func (s Secret) MarshalYAML() (any, error) { return s.rendered(), nil }

// MarshalText redacts, mirroring MarshalJSON/MarshalYAML so any generic encoder
// that only honours encoding.TextMarshaler also redacts.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.rendered()), nil }

// UnmarshalText sets the secret value from b, allowing text or JSON decoding
// INTO a Secret. The decoded bytes are the real value, not a redaction.
// encoding/json routes JSON string values here via encoding.TextUnmarshaler
// (defining only MarshalJSON does not suppress the TextUnmarshaler decode
// path), and the config decode hook (TextUnmarshallerHookFunc) does the same
// for scalar YAML/options values.
func (s *Secret) UnmarshalText(b []byte) error {
	s.value = string(b)
	return nil
}

// LogValue implements slog.LogValuer so structured logging redacts the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }
