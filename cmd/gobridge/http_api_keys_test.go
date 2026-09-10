package main

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The HTTP API keys may come from the environment so a config file that is
// mounted from a ConfigMap never has to carry a secret: the Kubernetes profile
// sets GOBRIDGE_ADMIN_API_KEY (and optionally GOBRIDGE_MONITOR_API_KEY) from a
// Secret. An environment value wins over the file value, because the file is
// the shareable part of the deployment and the environment is the secret part.

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestHTTPAPIKeys_EnvironmentOverridesConfig(t *testing.T) {
	cfg := &ports.HTTPConfig{
		AdminAPIKey:   shared.NewSecret("file-admin-key-16chars"),
		MonitorAPIKey: shared.NewSecret("file-monitor-key-16chr"),
	}
	admin, monitor := httpAPIKeys(cfg, lookupFrom(map[string]string{
		adminAPIKeyEnv:   "env-admin-key-16chars!",
		monitorAPIKeyEnv: "env-monitor-key-16chr!",
	}))
	if admin.Reveal() != "env-admin-key-16chars!" {
		t.Fatalf("admin key = %q, want the environment value", admin.Reveal())
	}
	if monitor.Reveal() != "env-monitor-key-16chr!" {
		t.Fatalf("monitor key = %q, want the environment value", monitor.Reveal())
	}
}

func TestHTTPAPIKeys_ConfigWhenEnvironmentUnset(t *testing.T) {
	cfg := &ports.HTTPConfig{AdminAPIKey: shared.NewSecret("file-admin-key-16chars")}
	admin, monitor := httpAPIKeys(cfg, lookupFrom(nil))
	if admin.Reveal() != "file-admin-key-16chars" {
		t.Fatalf("admin key = %q, want the file value", admin.Reveal())
	}
	if !monitor.IsZero() {
		t.Fatalf("monitor key = %q, want unset", monitor.Reveal())
	}
}

func TestHTTPAPIKeys_EmptyEnvironmentValueDoesNotBlankTheKey(t *testing.T) {
	// A variable that is present but empty (a Secret key that was never
	// filled in) must not silently erase the configured key: the server's
	// own validation then rejects an empty admin key and the failure names
	// the real cause instead of an auth mystery.
	cfg := &ports.HTTPConfig{AdminAPIKey: shared.NewSecret("file-admin-key-16chars")}
	admin, _ := httpAPIKeys(cfg, lookupFrom(map[string]string{adminAPIKeyEnv: ""}))
	if admin.Reveal() != "file-admin-key-16chars" {
		t.Fatalf("admin key = %q, want the file value", admin.Reveal())
	}
}
