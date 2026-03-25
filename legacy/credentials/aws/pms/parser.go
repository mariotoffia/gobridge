package pms

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// parseCredentials parses a parameter value into Credentials.
// Supports multiple formats:
// - JSON: {"username":"user","password":"pass"}
// - JSON with type: {"type":"usernamePassword","username":"user","password":"pass"}
// - Simple: username:password
// - TLS JSON: {"certPem":"...", "keyPem":"...", "caPem":["..."]}
func parseCredentials(value string) (*types.Credentials, error) {
	value = strings.TrimSpace(value)

	// Try JSON first
	if strings.HasPrefix(value, "{") {
		return parseJSONCredentials(value)
	}

	// Try simple username:password format
	if strings.Contains(value, ":") {
		return parseSimpleCredentials(value)
	}

	return nil, fmt.Errorf("unsupported credentials format")
}

// parseJSONCredentials parses JSON-formatted credentials.
func parseJSONCredentials(value string) (*types.Credentials, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON credentials: %w", err)
	}

	// Determine credential type
	credType := detectCredentialType(raw)

	switch credType {
	case types.CredentialsTypeUsernamePassword:
		return parseUsernamePasswordJSON(raw)
	case types.CredentialsTypeTLS:
		return parseTLSJSON(raw)
	default:
		return nil, fmt.Errorf("unable to determine credential type from JSON")
	}
}

// detectCredentialType determines the credential type from JSON fields.
func detectCredentialType(raw map[string]any) types.CredentialsType {
	// Check for explicit type field
	if typeStr, ok := raw["type"].(string); ok {
		switch strings.ToLower(typeStr) {
		case "usernamepassword", "username_password", "password":
			return types.CredentialsTypeUsernamePassword
		case "tls", "certificate", "cert":
			return types.CredentialsTypeTLS
		}
	}

	// Detect by presence of fields
	if _, hasUsername := raw["username"]; hasUsername {
		return types.CredentialsTypeUsernamePassword
	}
	if _, hasUser := raw["user"]; hasUser {
		return types.CredentialsTypeUsernamePassword
	}
	if _, hasCert := raw["certPem"]; hasCert {
		return types.CredentialsTypeTLS
	}
	if _, hasCert := raw["cert"]; hasCert {
		return types.CredentialsTypeTLS
	}

	return types.CredentialsTypeUnknown
}

// parseUsernamePasswordJSON parses username/password credentials from JSON.
func parseUsernamePasswordJSON(raw map[string]any) (*types.Credentials, error) {
	username := getStringField(raw, "username", "user")
	password := getStringField(raw, "password", "pass", "secret")

	if username == "" {
		return nil, fmt.Errorf("missing username field")
	}

	return &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
		Credentials: []any{
			types.UsernamePasswordCredentials{
				Username: username,
				Password: password,
			},
		},
	}, nil
}

// parseTLSJSON parses TLS credentials from JSON.
func parseTLSJSON(raw map[string]any) (*types.Credentials, error) {
	certPEM := getStringField(raw, "certPem", "cert", "certificate")
	keyPEM := getStringField(raw, "keyPem", "key", "privateKey")
	insecure := getBoolField(raw, "insecure", "insecureSkipVerify")

	var caPEM []string
	if ca, ok := raw["caPem"]; ok {
		switch v := ca.(type) {
		case string:
			caPEM = []string{v}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					caPEM = append(caPEM, s)
				}
			}
		}
	}
	if ca, ok := raw["ca"].(string); ok && len(caPEM) == 0 {
		caPEM = []string{ca}
	}

	return &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeTLS},
		Credentials: []any{
			types.TlsCredentials{
				CertPEM:            certPEM,
				KeyPEM:             keyPEM,
				CaPEM:              caPEM,
				InsecureSkipVerify: insecure,
			},
		},
	}, nil
}

// parseSimpleCredentials parses simple username:password format.
func parseSimpleCredentials(value string) (*types.Credentials, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid simple credentials format, expected username:password")
	}

	return &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
		Credentials: []any{
			types.UsernamePasswordCredentials{
				Username: parts[0],
				Password: parts[1],
			},
		},
	}, nil
}

// getStringField gets a string field from a map, trying multiple keys.
func getStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if val, ok := m[key].(string); ok {
			return val
		}
	}
	return ""
}

// getBoolField gets a bool field from a map, trying multiple keys.
func getBoolField(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if val, ok := m[key].(bool); ok {
			return val
		}
	}
	return false
}
