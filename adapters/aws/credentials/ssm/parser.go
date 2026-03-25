package ssm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

type credentialType int

const (
	credTypeUnknown credentialType = iota
	credTypePassword
	credTypeTLS
)

// parseCredentials parses a parameter value into a CredentialSet.
// Supports JSON objects (username/password or TLS) and simple
// username:password format.
func parseCredentials(value string) (*domain.CredentialSet, error) {
	value = strings.TrimSpace(value)

	if strings.HasPrefix(value, "{") {
		return parseJSONCredentials(value)
	}

	if strings.Contains(value, ":") {
		return parseSimpleCredentials(value)
	}

	return nil, fmt.Errorf("ssm: unsupported credentials format")
}

func parseJSONCredentials(value string) (*domain.CredentialSet, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("ssm: invalid JSON credentials: %w", err)
	}

	ct := detectCredentialType(raw)

	switch ct {
	case credTypePassword:
		return parseUsernamePasswordJSON(raw)
	case credTypeTLS:
		return parseTLSJSON(raw)
	default:
		return nil, fmt.Errorf("ssm: unable to determine credential type from JSON")
	}
}

func detectCredentialType(raw map[string]any) credentialType {
	if typeStr, ok := raw["type"].(string); ok {
		switch strings.ToLower(typeStr) {
		case "usernamepassword", "username_password", "password":
			return credTypePassword
		case "tls", "certificate", "cert":
			return credTypeTLS
		}
	}

	if _, ok := raw["username"]; ok {
		return credTypePassword
	}
	if _, ok := raw["user"]; ok {
		return credTypePassword
	}
	if _, ok := raw["certPem"]; ok {
		return credTypeTLS
	}
	if _, ok := raw["cert"]; ok {
		return credTypeTLS
	}

	return credTypeUnknown
}

func parseUsernamePasswordJSON(raw map[string]any) (*domain.CredentialSet, error) {
	username := getStringField(raw, "username", "user")
	password := getStringField(raw, "password", "pass", "secret")

	if username == "" {
		return nil, fmt.Errorf("ssm: missing username field")
	}

	return &domain.CredentialSet{
		Password: &domain.PasswordCredential{
			Username: username,
			Password: password,
		},
	}, nil
}

func parseTLSJSON(raw map[string]any) (*domain.CredentialSet, error) {
	certPEM := getStringField(raw, "certPem", "cert", "certificate")
	keyPEM := getStringField(raw, "keyPem", "key", "privateKey")
	insecure := getBoolField(raw, "insecure", "insecureSkipVerify")

	var caPEMs []string
	if ca, ok := raw["caPem"]; ok {
		switch v := ca.(type) {
		case string:
			caPEMs = []string{v}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					caPEMs = append(caPEMs, s)
				}
			}
		}
	}
	if ca, ok := raw["ca"].(string); ok && len(caPEMs) == 0 {
		caPEMs = []string{ca}
	}

	return &domain.CredentialSet{
		TLS: &domain.TLSMaterial{
			CertPEM:            certPEM,
			KeyPEM:             keyPEM,
			CAPEMs:             caPEMs,
			InsecureSkipVerify: insecure,
		},
	}, nil
}

func parseSimpleCredentials(value string) (*domain.CredentialSet, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("ssm: invalid simple credentials format, expected username:password")
	}

	return &domain.CredentialSet{
		Password: &domain.PasswordCredential{
			Username: parts[0],
			Password: parts[1],
		},
	}, nil
}

// serializeCredentialSet converts a CredentialSet to JSON for storage.
func serializeCredentialSet(creds *domain.CredentialSet) (string, error) {
	m := make(map[string]any)

	if creds.Password != nil {
		m["type"] = "password"
		m["username"] = creds.Password.Username
		m["password"] = creds.Password.Password
	}

	if creds.TLS != nil {
		if creds.Password == nil {
			m["type"] = "tls"
		}
		m["certPem"] = creds.TLS.CertPEM
		m["keyPem"] = creds.TLS.KeyPEM
		if len(creds.TLS.CAPEMs) > 0 {
			m["caPem"] = creds.TLS.CAPEMs
		}
		if creds.TLS.InsecureSkipVerify {
			m["insecure"] = true
		}
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
