package pms

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// AWS Parameter Store Repository Unit Tests
//
// Tests for credential parsing and URI handling. Integration tests with
// actual AWS/LocalStack are in integration_pms_test.go
// ═══════════════════════════════════════════════════════════════════════════

// TestParseCredentials_JSONUsernamePassword validates JSON username/password parsing.
func TestParseCredentials_JSONUsernamePassword(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		username string
		password string
	}{
		{
			name:     "standard format",
			input:    `{"username":"alice","password":"secret123"}`,
			username: "alice",
			password: "secret123",
		},
		{
			name:     "with explicit type",
			input:    `{"type":"usernamePassword","username":"bob","password":"pass"}`,
			username: "bob",
			password: "pass",
		},
		{
			name:     "alternate field names",
			input:    `{"user":"charlie","pass":"mypass"}`,
			username: "charlie",
			password: "mypass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds, err := parseCredentials(tt.input)
			if err != nil {
				t.Fatalf("parseCredentials failed: %v", err)
			}

			if len(creds.Type) != 1 || creds.Type[0] != types.CredentialsTypeUsernamePassword {
				t.Errorf("expected UsernamePassword type, got %v", creds.Type)
			}

			if len(creds.Credentials) != 1 {
				t.Fatalf("expected 1 credential, got %d", len(creds.Credentials))
			}

			up, ok := creds.Credentials[0].(types.UsernamePasswordCredentials)
			if !ok {
				t.Fatalf("expected UsernamePasswordCredentials, got %T", creds.Credentials[0])
			}

			if up.Username != tt.username {
				t.Errorf("expected username %s, got %s", tt.username, up.Username)
			}
			if up.Password != tt.password {
				t.Errorf("expected password %s, got %s", tt.password, up.Password)
			}
		})
	}
}

// TestParseCredentials_SimpleFormat validates simple username:password parsing.
func TestParseCredentials_SimpleFormat(t *testing.T) {
	creds, err := parseCredentials("myuser:mypassword")
	if err != nil {
		t.Fatalf("parseCredentials failed: %v", err)
	}

	if len(creds.Type) != 1 || creds.Type[0] != types.CredentialsTypeUsernamePassword {
		t.Errorf("expected UsernamePassword type, got %v", creds.Type)
	}

	up, ok := creds.Credentials[0].(types.UsernamePasswordCredentials)
	if !ok {
		t.Fatalf("expected UsernamePasswordCredentials, got %T", creds.Credentials[0])
	}

	if up.Username != "myuser" {
		t.Errorf("expected username myuser, got %s", up.Username)
	}
	if up.Password != "mypassword" {
		t.Errorf("expected password mypassword, got %s", up.Password)
	}
}

// TestParseCredentials_TLS validates TLS credential parsing.
func TestParseCredentials_TLS(t *testing.T) {
	input := `{
		"certPem": "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
		"keyPem": "-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----",
		"caPem": ["-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----"],
		"insecure": false
	}`

	creds, err := parseCredentials(input)
	if err != nil {
		t.Fatalf("parseCredentials failed: %v", err)
	}

	if len(creds.Type) != 1 || creds.Type[0] != types.CredentialsTypeTLS {
		t.Errorf("expected TLS type, got %v", creds.Type)
	}

	tls, ok := creds.Credentials[0].(types.TlsCredentials)
	if !ok {
		t.Fatalf("expected TlsCredentials, got %T", creds.Credentials[0])
	}

	if tls.CertPEM == "" {
		t.Error("expected CertPEM to be set")
	}
	if tls.KeyPEM == "" {
		t.Error("expected KeyPEM to be set")
	}
	if len(tls.CaPEM) != 1 {
		t.Errorf("expected 1 CA cert, got %d", len(tls.CaPEM))
	}
}

// TestParseURI validates URI parsing.
func TestParseURI(t *testing.T) {
	r := &Repository{}

	tests := []struct {
		name     string
		uri      string
		expected string
		wantErr  bool
	}{
		{
			name:     "simple path",
			uri:      "pms://prod/db/password",
			expected: "/prod/db/password",
		},
		{
			name:     "nested path",
			uri:      "pms://app/service/credentials/main",
			expected: "/app/service/credentials/main",
		},
		{
			name:    "wrong scheme",
			uri:     "vault://secret/path",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := r.parseURI(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseURI failed: %v", err)
			}
			if path != tt.expected {
				t.Errorf("expected path %s, got %s", tt.expected, path)
			}
		})
	}
}

// TestRepository_GetScheme validates scheme returned.
func TestRepository_GetScheme(t *testing.T) {
	r := &Repository{}
	if r.GetScheme() != "pms" {
		t.Errorf("expected scheme pms, got %s", r.GetScheme())
	}
}

// TestRepository_GetNamespace validates namespace returned.
func TestRepository_GetNamespace(t *testing.T) {
	r := &Repository{config: Config{Namespace: "myapp/prod"}}
	if r.GetNamespace() != "myapp/prod" {
		t.Errorf("expected namespace myapp/prod, got %s", r.GetNamespace())
	}
}

// TestCache validates credential caching.
func TestCache(t *testing.T) {
	r := &Repository{
		cache: make(map[string]*cacheEntry),
	}

	creds := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
		Credentials: []any{
			types.UsernamePasswordCredentials{Username: "test", Password: "pass"},
		},
	}

	// Cache miss
	if got := r.getCached("/test/path"); got != nil {
		t.Error("expected cache miss")
	}

	// Set cache
	r.config.CacheTTL = 1 * time.Hour
	r.setCached("/test/path", creds)

	// Cache hit
	if got := r.getCached("/test/path"); got == nil {
		t.Error("expected cache hit")
	}

	// Clear cache
	r.ClearCache()
	if got := r.getCached("/test/path"); got != nil {
		t.Error("expected cache miss after clear")
	}
}
