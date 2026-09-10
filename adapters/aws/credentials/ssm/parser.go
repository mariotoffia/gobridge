package ssm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// parseCredentials parses a parameter value into a CredentialSet.
// Supports JSON objects (username/password, an opaque password-only
// "secret" shape, TLS, or capabilities combined) and the simple
// username:password format.
func parseCredentials(value string) (*connectivity.CredentialSet, error) {
	value = strings.TrimSpace(value)

	if strings.HasPrefix(value, "{") {
		return parseJSONCredentials(value)
	}

	if strings.Contains(value, ":") {
		return parseSimpleCredentials(value)
	}

	return nil, fmt.Errorf("ssm: unsupported credentials format")
}

// parseJSONCredentials extracts EVERY capability present in the JSON
// document: a credential set may carry password and TLS material at
// the same time (e.g. SASL-over-TLS brokers), so parsing must never
// dispatch to a single branch and silently drop the other capability.
// A capability participates when the "type" field declares it (tokens
// may be combined with '+', e.g. "password+tls") or when its fields
// are present in the document.
//
// A password credential has two shapes. The default carries a username
// and requires it. The opaque "secret" shape carries the whole credential
// in a single value with NO username — the form the Azure Service Bus
// transport consumes for a SAS connection string
// (adapters/azure/transport/servicebus/credentials_refresh.go:35-40). It is
// selected by a "secret" / "opaque" / "sas" type token ONLY when the
// document carries no username; a secret token that appears alongside a
// username keeps parsing as username/password, exactly as pre-opaque readers
// did, so credential sets already stored in SSM stay readable after upgrade.
func parseJSONCredentials(value string) (*connectivity.CredentialSet, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("ssm: invalid JSON credentials: %w", err)
	}

	declaredPassword, declaredSecret, declaredTLS := declaredCapabilities(raw)
	wantPassword := declaredPassword || declaredSecret || hasPasswordFields(raw)
	wantTLS := declaredTLS || hasTLSFields(raw)

	if !wantPassword && !wantTLS {
		return nil, fmt.Errorf("ssm: unable to determine credential type from JSON")
	}

	var password *connectivity.PasswordCredential
	if wantPassword {
		pw, err := parsePasswordJSON(raw, declaredSecret)
		if err != nil {
			return nil, err
		}
		password = pw
	}

	var tls *connectivity.TLSMaterial
	if wantTLS {
		t, err := parseTLSJSON(raw)
		if err != nil {
			return nil, err
		}
		tls = t
	}

	return connectivity.NewCredentialSet(password, tls), nil
}

// declaredCapabilities reads the optional "type" field. Tokens may be
// combined with '+' (the serializer emits "password+tls" for combined
// sets, or "secret" for an opaque password-only credential).
func declaredCapabilities(raw map[string]any) (password, secret, tls bool) {
	typeStr, ok := raw["type"].(string)
	if !ok {
		return false, false, false
	}
	for _, token := range strings.Split(strings.ToLower(typeStr), "+") {
		switch strings.TrimSpace(token) {
		case "usernamepassword", "username_password", "password":
			password = true
		case "secret", "opaque", "sas":
			secret = true
		case "tls", "certificate", "cert":
			tls = true
		}
	}
	return password, secret, tls
}

// hasPasswordFields reports whether the document carries
// username/password material.
func hasPasswordFields(raw map[string]any) bool {
	_, hasUsername := raw["username"]
	_, hasUser := raw["user"]
	return hasUsername || hasUser
}

// hasTLSFields reports whether the document carries TLS material.
// Deliberately excludes the ambiguous bare "key" and "insecure" fields
// so a password-only document can never be misread as TLS.
func hasTLSFields(raw map[string]any) bool {
	for _, k := range []string{"certPem", "cert", "certificate", "keyPem", "privateKey", "caPem", "ca"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

// parsePasswordJSON builds the password credential from a JSON document.
//
// declaredSecret is true when the "type" field carried a secret token
// ("secret" / "opaque" / "sas"). The opaque password-only form — the whole
// credential in a single value with an intentionally empty username, the shape
// an Azure Service Bus SAS connection string takes at runtime
// (PasswordCredential{Username:"", Password:<connection string>}) — is selected
// ONLY when a secret token was declared AND the document carries no username.
//
// A secret token that appears next to a username is NOT opaque: it parses as an
// ordinary username/password credential. Older readers ignored unknown type
// tokens and parsed such documents as username/password, and stored SSM values
// are never rewritten on upgrade, so that read must stay backward compatible —
// the invariant is that any JSON an earlier reader accepted still parses to the
// same credential.
//
// The default form requires a non-empty username so a username-less "password"
// document surfaces as an error instead of a broker-breaking anonymous
// credential. The opaque form requires a non-BLANK secret value for the same
// reason: a whitespace-only value ("\n", " ") is a semantically empty
// connection string that would fail broker client creation downstream, so it is
// rejected here rather than deferred to a confusing connect-time failure.
func parsePasswordJSON(raw map[string]any, declaredSecret bool) (*connectivity.PasswordCredential, error) {
	username := getStringField(raw, "username", "user")
	password := getStringField(raw, "password", "pass", "secret")

	if declaredSecret && username == "" {
		if strings.TrimSpace(password) == "" {
			return nil, fmt.Errorf("ssm: opaque secret credential requires a non-empty secret value")
		}
		pw := connectivity.NewPasswordCredential("", password)
		return &pw, nil
	}

	if username == "" {
		return nil, fmt.Errorf("ssm: missing username field")
	}

	pw := connectivity.NewPasswordCredential(username, password)
	return &pw, nil
}

func parseTLSJSON(raw map[string]any) (*connectivity.TLSMaterial, error) {
	certPEM := getStringField(raw, "certPem", "cert", "certificate")
	keyPEM := getStringField(raw, "keyPem", "key", "privateKey")
	insecure := getBoolField(raw, "insecure", "insecureSkipVerify")

	var caPEMs []string
	if ca, ok := raw["caPem"]; ok {
		switch v := ca.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				caPEMs = []string{v}
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					caPEMs = append(caPEMs, s)
				}
			}
		}
	}
	if ca, ok := raw["ca"].(string); ok && strings.TrimSpace(ca) != "" && len(caPEMs) == 0 {
		caPEMs = []string{ca}
	}

	// Reject empty / incomplete TLS material instead of silently
	// producing a credential with no trust anchors. A failed rotation
	// write leaving {"type":"tls"} (or a torn document) must surface as
	// an error, not strip TLS trust from a live transport. Valid forms
	// are a CA bundle (server verification) and/or a complete cert+key
	// pair (mutual TLS); a half cert/key pair is always rejected.
	// Emptiness is judged after trimming so a whitespace-only field
	// (" ", "\n") is treated as absent and rejected at parse time rather
	// than deferred to a confusing connect-time TLS failure.
	hasCert := strings.TrimSpace(certPEM) != ""
	hasKey := strings.TrimSpace(keyPEM) != ""
	if hasCert != hasKey {
		return nil, fmt.Errorf("ssm: incomplete TLS material: certPem and keyPem must be provided together")
	}
	hasPair := hasCert && hasKey
	if !hasPair && len(caPEMs) == 0 {
		return nil, fmt.Errorf("ssm: empty TLS material: require a CA bundle or a complete cert/key pair")
	}

	// Normalise a whitespace-only cert or key to the empty string. The guards
	// above judge presence AFTER trimming, so a CA-only document may still hold
	// blank-but-non-empty cert/key fields (" ", "\n"). Storing those verbatim
	// would later drive a transport into tls.X509KeyPair(" ", " "), which fails
	// at connect/rotation time on material this reader had already accepted.
	// A genuine cert/key pair (hasCert && hasKey) is stored verbatim.
	if !hasCert {
		certPEM = ""
	}
	if !hasKey {
		keyPEM = ""
	}

	tls := connectivity.NewTLSMaterial(certPEM, keyPEM, caPEMs, insecure)
	return &tls, nil
}

func parseSimpleCredentials(value string) (*connectivity.CredentialSet, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("ssm: invalid simple credentials format, expected username:password")
	}

	username, password := parts[0], parts[1]
	if username == "" || password == "" {
		// A ":pass", "user:" or ":" value would otherwise yield an
		// anonymous/half-empty credential that drives transports into
		// authentication-failure reconnect loops.
		return nil, fmt.Errorf("ssm: simple credentials require a non-empty username and password")
	}

	pw := connectivity.NewPasswordCredential(username, password)
	return connectivity.NewCredentialSet(&pw, nil), nil
}

// serializeCredentialSet converts a CredentialSet to JSON for storage.
// The "type" field enumerates every capability present (joined with
// '+', e.g. "password+tls") so parseJSONCredentials round-trips a
// combined set without losing either capability. A password whose
// username is empty is written as the opaque "secret" shape so the
// reader reconstructs the empty-username credential (e.g. a Service Bus
// SAS connection string) instead of rejecting a username-less
// password.
//
// Every serialized payload is validated by round-tripping it back
// through the package's own reader (parseCredentials) before it is
// returned for storage. An admin Create/Update must never persist a
// value that a later Get or rotation poll cannot parse, because a single
// unreadable write would otherwise become a persistent credential outage
// empty sets, torn TLS halves, empty passwords, and any other
// unreadable shape are rejected here at write time.
func serializeCredentialSet(creds *connectivity.CredentialSet) (string, error) {
	m := make(map[string]any)
	var capabilities []string

	if pw := creds.Password(); pw != nil {
		if pw.Username() == "" {
			// Opaque password-only credential: the whole secret is the
			// password and there is no username (Service Bus SAS et al.).
			capabilities = append(capabilities, "secret")
			m["secret"] = pw.Password().Reveal()
		} else {
			capabilities = append(capabilities, "password")
			m["username"] = pw.Username()
			m["password"] = pw.Password().Reveal()
		}
	}

	if tls := creds.TLS(); tls != nil {
		capabilities = append(capabilities, "tls")
		m["certPem"] = tls.CertPEM()
		m["keyPem"] = tls.KeyPEM().Reveal()
		if caPEMs := tls.CAPEMs(); len(caPEMs) > 0 {
			m["caPem"] = caPEMs
		}
		if tls.InsecureSkipVerify() {
			m["insecure"] = true
		}
	}

	if len(capabilities) > 0 {
		m["type"] = strings.Join(capabilities, "+")
	}

	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("ssm: failed to serialize credentials: %w", err)
	}

	if err := ensureReadable(string(data), creds); err != nil {
		return "", err
	}
	return string(data), nil
}

// ensureReadable is the write-side round-trip guard. It reparses the
// serialized payload with the package's own reader and confirms the result is
// an equivalent CredentialSet, so Create/Update can never persist a value that
// a subsequent Get or rotation poll would reject. The failure is classified as
// shared.ErrInvalidPayload for parity with the sibling file repository's
// usable-credential guard (adapters/native/credentials/file/repository.go).
func ensureReadable(serialized string, original *connectivity.CredentialSet) error {
	roundTripped, err := parseCredentials(serialized)
	if err != nil {
		return shared.ErrInvalidPayload.WithMessage(
			"ssm: refusing to store credentials the reader cannot parse back").Wrap(err)
	}
	if !roundTripped.Equal(original) {
		return shared.ErrInvalidPayload.WithMessage(
			"ssm: refusing to store credentials that do not round-trip through the reader")
	}
	return nil
}

func getStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if val, ok := m[key].(string); ok {
			return val
		}
	}
	return ""
}

func getBoolField(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if val, ok := m[key].(bool); ok {
			return val
		}
	}
	return false
}
