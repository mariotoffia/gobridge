package main

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestSecretFields proves the secret-field rule: it flags raw
// secret-looking string fields on a ports.PluginConfig type — both direct
// (FlaggedConfig.Password) and nested via a same-package sub-config
// (Conn.ConnectionString reached from NestedConfig) — and the conservative
// token handling (TokenConfig.AccessToken flagged; TokenBucket / RoutingKey
// not). Fields wrapped in a non-string redaction type (OKConfig.Password,
// TokenConfig.AuthToken) pass.
func TestSecretFields(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "secretcfg")
}
