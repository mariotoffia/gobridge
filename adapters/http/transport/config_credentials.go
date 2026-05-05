package transport

import (
	"errors"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)

// CredentialsURI implements ports.CredentialedConfig.
func (c *Config) CredentialsURI() string {
	if c == nil {
		return ""
	}
	return c.CredentialsURIRef
}

// ApplyCredentials implements ports.CredentialedConfig. The HTTP
// transport's only auth field is APIKey: a resolved
// PasswordCredential.Password populates APIKey when it is empty
// ("inline config wins"). TLS material has no current consumer at
// build time and is therefore ignored here.
func (c *Config) ApplyCredentials(set *domain.CredentialSet) error {
	if c == nil {
		return errors.New("http: nil config")
	}
	if set == nil {
		c.CredentialsURIRef = ""
		return nil
	}
	if c.APIKey == "" && set.Password != nil && set.Password.Password != "" {
		c.APIKey = set.Password.Password
	}
	c.CredentialsURIRef = ""
	return nil
}
