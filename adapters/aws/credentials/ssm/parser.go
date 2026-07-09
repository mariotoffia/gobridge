package ssm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// parseCredentials parses a parameter value into a CredentialSet.
// Supports JSON objects (username/password, TLS, or both capabilities
// combined) and simple username:password format.
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
func parseJSONCredentials(value string) (*connectivity.CredentialSet, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("ssm: invalid JSON credentials: %w", err)
	}

	declaredPassword, declaredTLS := declaredCapabilities(raw)
	wantPassword := declaredPassword || hasPasswordFields(raw)
	wantTLS := declaredTLS || hasTLSFields(raw)

	if !wantPassword && !wantTLS {
		return nil, fmt.Errorf("ssm: unable to determine credential type from JSON")
	}

	var password *connectivity.PasswordCredential
	if wantPassword {
		pw, err := parseUsernamePasswordJSON(raw)
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
// sets).
func declaredCapabilities(raw map[string]any) (password, tls bool) {
	typeStr, ok := raw["type"].(string)
	if !ok {
		return false, false
	}
	for _, token := range strings.Split(strings.ToLower(typeStr), "+") {
		switch strings.TrimSpace(token) {
		case "usernamepassword", "username_password", "password":
			password = true
		case "tls", "certificate", "cert":
			tls = true
		}
	}
	return password, tls
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

func parseUsernamePasswordJSON(raw map[string]any) (*connectivity.PasswordCredential, error) {
	username := getStringField(raw, "username", "user")
	password := getStringField(raw, "password", "pass", "secret")

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
// combined set without losing either capability.
func serializeCredentialSet(creds *connectivity.CredentialSet) (string, error) {
	m := make(map[string]any)
	var capabilities []string

	if creds.Password() != nil {
		capabilities = append(capabilities, "password")
		m["username"] = creds.Password().Username()
		m["password"] = creds.Password().Password().Reveal()
	}

	if creds.TLS() != nil {
		capabilities = append(capabilities, "tls")
		m["certPem"] = creds.TLS().CertPEM()
		m["keyPem"] = creds.TLS().KeyPEM().Reveal()
		if caPEMs := creds.TLS().CAPEMs(); len(caPEMs) > 0 {
			m["caPem"] = caPEMs
		}
		if creds.TLS().InsecureSkipVerify() {
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
	return string(data), nil
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
