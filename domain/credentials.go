package domain

// CredentialKind identifies the type of credential material.
type CredentialKind string

const (
	CredentialPassword CredentialKind = "password"
	CredentialTLS      CredentialKind = "tls"
)

// PasswordCredential holds username/password authentication material.
type PasswordCredential struct {
	Username string
	Password string
}

func (PasswordCredential) String() string   { return "PasswordCredential{REDACTED}" }
func (PasswordCredential) GoString() string { return "PasswordCredential{REDACTED}" }

// TLSMaterial holds TLS certificate and key material.
type TLSMaterial struct {
	CertPEM            string
	KeyPEM             string
	CAPEMs             []string
	InsecureSkipVerify bool
}

func (TLSMaterial) String() string   { return "TLSMaterial{REDACTED}" }
func (TLSMaterial) GoString() string { return "TLSMaterial{REDACTED}" }

// CredentialSet is a composite credential container. A single URI
// resolution can yield both password and TLS material. Nil fields
// indicate the credential kind is not present.
type CredentialSet struct {
	Password *PasswordCredential
	TLS      *TLSMaterial
}
