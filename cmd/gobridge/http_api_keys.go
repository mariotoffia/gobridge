package main

import (
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The HTTP API keys may be supplied through the environment so the config
// file — which a Kubernetes ConfigMap makes readable to anyone who can read
// the namespace — never has to carry a secret. A non-empty environment value
// wins over the file value; an unset or empty variable leaves the file value
// in place, so a Secret key that was never filled in fails the server's own
// key validation with an error that names the cause instead of silently
// blanking a configured key.
const (
	adminAPIKeyEnv   = "GOBRIDGE_ADMIN_API_KEY"
	monitorAPIKeyEnv = "GOBRIDGE_MONITOR_API_KEY"
)

// httpAPIKeys resolves the admin and monitor keys from the http block plus the
// environment (lookup is os.LookupEnv in production, injected for tests).
func httpAPIKeys(cfg *ports.HTTPConfig, lookup func(string) (string, bool)) (admin, monitor shared.Secret) {
	admin, monitor = cfg.AdminAPIKey, cfg.MonitorAPIKey
	if v, ok := lookup(adminAPIKeyEnv); ok && v != "" {
		admin = shared.NewSecret(v)
	}
	if v, ok := lookup(monitorAPIKeyEnv); ok && v != "" {
		monitor = shared.NewSecret(v)
	}
	return admin, monitor
}
