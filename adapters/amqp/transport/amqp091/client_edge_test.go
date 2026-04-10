// ═══════════════════════════════════════════════
// Client Edge Case Tests
//
// Validates credential injection and URL redaction
// edge cases in the AMQP 0-9-1 client helpers.
// ═══════════════════════════════════════════════
package amqp091

import (
	"testing"
)

// TestInjectCredentials_ExistingUserInfo validates that credentials
// already present in the URL are not overwritten.
func TestInjectCredentials_ExistingUserInfo(t *testing.T) {
	got := injectCredentials("amqp://existing:pass@host:5672/", "new", "new")
	if got != "amqp://existing:pass@host:5672/" {
		t.Fatalf("expected original URL preserved, got %s", got)
	}
}

// TestInjectCredentials_EmptyUsername validates no-op when username is empty.
func TestInjectCredentials_EmptyUsername(t *testing.T) {
	got := injectCredentials("amqp://host:5672/", "", "pass")
	if got != "amqp://host:5672/" {
		t.Fatalf("expected original URL, got %s", got)
	}
}

// TestInjectCredentials_MalformedURL validates that unparseable URLs
// are returned unchanged.
func TestInjectCredentials_MalformedURL(t *testing.T) {
	got := injectCredentials("://bad", "user", "pass")
	if got != "://bad" {
		t.Fatalf("expected original URL for malformed input, got %s", got)
	}
}

// TestRedactURL_NoUserInfo validates URLs without credentials are unchanged.
func TestRedactURL_NoUserInfo(t *testing.T) {
	got := redactURL("amqp://host:5672/vhost")
	if got != "amqp://host:5672/vhost" {
		t.Fatalf("expected unchanged URL, got %s", got)
	}
}

// TestRedactURL_WithCredentials validates credentials are masked.
func TestRedactURL_WithCredentials(t *testing.T) {
	got := redactURL("amqp://admin:secret@host:5672/vhost")
	if got == "amqp://admin:secret@host:5672/vhost" {
		t.Fatal("expected credentials to be redacted")
	}
	if got != "amqp://REDACTED@host:5672/vhost" {
		t.Fatalf("unexpected redacted URL: %s", got)
	}
}

// TestRedactURL_InvalidURL validates the fallback for unparseable URLs.
func TestRedactURL_InvalidURL(t *testing.T) {
	got := redactURL("://bad")
	if got != "<invalid-url>" {
		t.Fatalf("expected <invalid-url>, got %s", got)
	}
}
