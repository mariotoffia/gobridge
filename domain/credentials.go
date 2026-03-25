package domain

// CredentialKind identifies the type of credential material.
type CredentialKind string

const (
	CredentialPassword CredentialKind = "password"
	CredentialTLS      CredentialKind = "tls"
)

// PasswordCredential holds username/password authentication material.
// No String or GoString method is provided intentionally: credential
// values must never appear in log output.
type PasswordCredential struct {
	Username string
	Password string
}

// TLSMaterial holds TLS certificate and key material.
// No String or GoString method is provided intentionally: credential
// values must never appear in log output.
type TLSMaterial struct {
	CertPEM            string
	KeyPEM             string
	CAPEMs             []string
	InsecureSkipVerify bool
}

// CredentialSet is a composite credential container. A single URI
// resolution can yield both password and TLS material. Nil fields
// indicate the credential kind is not present.
type CredentialSet struct {
	Password *PasswordCredential
	TLS      *TLSMaterial
}
