package types

// CredentialsType is indicating the type of credentials.
type CredentialsType int

const (
	// CredentialsTypeUnknown is illegal type
	CredentialsTypeUnknown CredentialsType = 0
	// CredentialsTypeUsernamePassword is for `UsernamePasswordCredentials`
	CredentialsTypeUsernamePassword CredentialsType = 1
	// CredentialsTypeTLS is for `TlsCredentials`
	CredentialsTypeTLS CredentialsType = 2
)

type Credentials struct {
	// Type is a slice indicating the type of credentials stored in the Credentials field.
	Type []CredentialsType `json:"type"`
	// Credentials holds the actual credentials object(s).
	//
	// The object types are determined by the Type field:
	// - CredentialsTypeUsernamePassword → UsernamePasswordCredentials
	// - CredentialsTypeTLS → TlsCredentials
	Credentials []any `json:"credentials"`
}

// UsernamePasswordCredentials is for standard username/password authentication.
type UsernamePasswordCredentials struct {
	Username string
	Password string
}

type TlsCredentials struct {
	// CertPEM is the _URI_ to the client certificate file (PEM encoded). If no scheme is specified,
	// an embedded PEM is assumed. If both public and private keys are present it do not need the `KeyPEM`.
	CertPEM string `json:"cert,omitempty"`
	// KeyPEM is the _URI_ to the client private key file (PEM encoded). If no scheme is specified,
	// an embedded PEM is assumed.
	KeyPEM string `json:"key,omitempty"`
	// CaPEM is the _URI_ to the CA certificate file (PEM encoded) used to verify the server certificate.
	// If no scheme is specified, an embedded PEM is assumed.
	CaPEM []string `json:"ca,omitempty"`
	// InsecureSkipVerify skips verification of the server certificate chain and host name.
	InsecureSkipVerify bool `json:"insecure,omitempty"`
}

// CredentialsRepository is used to lookup credentials for a given server URI.
// It registers itself for a specific URI scheme (e.g., "pms") and optionally a namespace.
//
// The namespace matching rules:
// 1. The repository’s GetScheme() value must match the URI scheme of the server URI.
// 2. The repository’s GetNamespace() value must be a path-prefix of the server URI’s path (after scheme).
// 3. If multiple repositories match (same scheme and namespace is prefix), the one with the *longest* (most specific) namespace wins.
// 4. If no repository with a non-empty namespace matches, a repository with an empty namespace may serve as the fallback (root).
//
// Example: Suppose you register two repositories for scheme “pms” (AWS Parameter Store):
//
//	– RepoA: scheme = "pms", namespace = ""               // fallback for all “pms://…” URIs
//	– RepoB: scheme = "pms", namespace = "tenantA/app1"   // specialized for tenantA/app1
//
// Lookup scenarios:
//   - URI = "pms://tenantA/app1/prod/db/password" → RepoB matches (namespace “tenantA/app1” is longest prefix).
//   - URI = "pms://tenantA/app2/prod/db/password" → RepoA matches (RepoB namespace does not match path “tenantA/app2/…”).
//   - URI = "pms://tenantB/appX/…” → RepoA matches (no other repository prefix covers “tenantB”).
//
// Interface definition:
type CredentialsRepository interface {
	// GetScheme returns the URI scheme that this repository handles (e.g., "pms" for "pms://…").
	GetScheme() string

	// GetNamespace returns the optional namespace prefix this repository is responsible for.
	// If empty, the repository acts as a root fallback for the scheme.
	GetNamespace() string

	// GetCredentials returns the credentials object for the given server URI (which includes scheme://path).
	// It may return an error if credentials cannot be found or repository cannot serve that URI.
	GetCredentials(serverURI string) (*Credentials, error)
}
