package bootstrap

import (
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

// The bridge config's `http:` block is not honoured by this profile: admin and
// monitor listeners are configured from deployment bootstrap, and TLS terminates
// at the load balancer. This is the check that says so out loud.

// checkIgnoredHTTPBlock enforces the file-based deployment profile's policy for
// the optional bridge config `http:` block. This profile sources admin/monitor
// addresses and API keys from bootstrap env/SSM (a.cfg / apiKeysRef) and expects
// TLS to terminate at the load balancer (ALB); it does NOT feed ports.HTTPConfig
// into the served httpapi.Config. Only the cmd/gobridge binary and library
// embeddings honor the `http:` block.
//
// A tls_cert_file/tls_key_file entry is an explicit "encrypt this" instruction
// the profile cannot satisfy in-process. Silently serving the admin API in
// plaintext would be a security fail-open, so any TLS entry FAILS CLOSED and
// aborts startup — mirroring httpapi's own refusal to run a misconfigured TLS
// listener (httpapi/server.go rejects a half-set pair). An operator who needs
// in-process TLS must use the cmd/gobridge binary or a library embedding.
//
// A bare `http:` block (addresses/keys only, no TLS entry) is legitimately
// overridden by the SSM-driven bootstrap, so it is warned-and-ignored rather
// than rejected: WARN-and-continue keeps existing deployments booting. The
// addresses/keys are deliberately NOT re-sourced from logical.HTTP (that would
// double-source and conflict with the SSM-driven design); API keys are secrets
// and are never logged.
func checkIgnoredHTTPBlock(logger *slog.Logger, logical *ports.BridgeConfig) error {
	if logical == nil || logical.HTTP == nil {
		return nil
	}
	if logical.HTTP.TLSCertFile != "" || logical.HTTP.TLSKeyFile != "" {
		return fmt.Errorf("bootstrap: the file-based deployment profile does not serve in-process TLS " +
			"(TLS is expected to terminate at the load balancer), so the bridge config `http:` block's " +
			"tls_cert_file/tls_key_file cannot be honored here; continuing would silently serve the admin " +
			"API in plaintext. Remove the TLS pair from bridge.yaml, or use the cmd/gobridge binary or a " +
			"library embedding if you need in-process TLS")
	}
	logger.Warn("bootstrap: the file-based deployment profile does NOT honor the bridge config `http:` block; "+
		"admin/monitor addresses and API keys are configured via bootstrap env/SSM and TLS is expected to "+
		"terminate at the load balancer (ALB). The `http:` block is honored only by the cmd/gobridge binary "+
		"and library embeddings — it is IGNORED here",
		"admin_addr", logical.HTTP.AdminAddr,
		"monitor_addr", logical.HTTP.MonitorAddr,
		"tls_cert_file_set", logical.HTTP.TLSCertFile != "",
		"tls_key_file_set", logical.HTTP.TLSKeyFile != "",
	)
	return nil
}
