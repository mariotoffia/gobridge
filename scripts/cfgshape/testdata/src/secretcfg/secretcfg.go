// Package secretcfg is cfgshape analysistest fixture data for the
// secret-field rule. Types here implement the synthetic ports.PluginConfig
// shape (Kind() string + Validate() error) so the analyzer treats them as
// plugin configs without importing the real ports package.
package secretcfg

// Secret is a stand-in for the approved redaction wrapper (the production
// type is domain/shared.Secret). Any non-string field type passes the rule.
type Secret struct{ v string }

// FlaggedConfig carries a raw secret-looking string field directly on the
// plugin config — it must be flagged.
type FlaggedConfig struct {
	Host     string
	Username string
	Password string // want `plugin-config field FlaggedConfig\.Password is a raw secret-looking string`
}

func (FlaggedConfig) Kind() string    { return "test.flagged" }
func (FlaggedConfig) Validate() error { return nil }

// OKConfig wraps its secret in the approved non-string wrapper — it passes.
type OKConfig struct {
	Host     string
	Password Secret
}

func (OKConfig) Kind() string    { return "test.ok" }
func (OKConfig) Validate() error { return nil }

// Conn is a nested connection sub-config reached from NestedConfig. The rule
// must recurse same-package structs to catch the secret here.
type Conn struct {
	Endpoint         string
	ConnectionString string // want `plugin-config field Conn\.ConnectionString is a raw secret-looking string`
}

// NestedConfig nests Conn by value; recursion must reach Conn.ConnectionString.
type NestedConfig struct {
	Conn Conn
}

func (NestedConfig) Kind() string    { return "test.nested" }
func (NestedConfig) Validate() error { return nil }

// TokenConfig proves the conservative "token" handling and the no-bare-"key"
// rule: AccessToken (suffix) is a secret; TokenBucket (prefix) and RoutingKey
// are not.
type TokenConfig struct {
	TokenBucket string
	RoutingKey  string
	AccessToken string // want `plugin-config field TokenConfig\.AccessToken is a raw secret-looking string`
	AuthToken   Secret
}

func (TokenConfig) Kind() string    { return "test.token" }
func (TokenConfig) Validate() error { return nil }

// PEMFlaggedConfig proves the G2 PEM/private-key suffixes are flagged: a
// raw KeyPEM/CertPEM/CACertPEM string leaks TLS material exactly like a
// password and must be wrapped in shared.Secret.
type PEMFlaggedConfig struct {
	Host       string
	CACertPEM  string // want `plugin-config field PEMFlaggedConfig\.CACertPEM is a raw secret-looking string`
	CertPEM    string // want `plugin-config field PEMFlaggedConfig\.CertPEM is a raw secret-looking string`
	KeyPEM     string // want `plugin-config field PEMFlaggedConfig\.KeyPEM is a raw secret-looking string`
	PrivateKey string // want `plugin-config field PEMFlaggedConfig\.PrivateKey is a raw secret-looking string`
}

func (PEMFlaggedConfig) Kind() string    { return "test.pemflagged" }
func (PEMFlaggedConfig) Validate() error { return nil }

// PEMOKConfig wraps its PEM material in the approved non-string wrapper —
// it passes. RoutingKey (a non-secret *Key field) must NOT be flagged.
type PEMOKConfig struct {
	Host       string
	RoutingKey string
	KeyPEM     Secret
	CertPEM    Secret
}

func (PEMOKConfig) Kind() string    { return "test.pemok" }
func (PEMOKConfig) Validate() error { return nil }
