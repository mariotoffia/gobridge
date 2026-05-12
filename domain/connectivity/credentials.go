package connectivity

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

// Equal returns true when two credential sets hold identical material.
// It performs a deep, value-based comparison (not pointer equality) so
// that callers can safely use it to dedup rotation events where each
// resolve() produces a freshly-allocated *CredentialSet.
//
// Why: the PushCredentialStore contract requires implementations to
// emit only on actual changes; this is the canonical equality check
// used by runtime/credentials.PollBasedWrapper and any future push store.
func (c *CredentialSet) Equal(other *CredentialSet) bool {
	if c == nil || other == nil {
		return c == nil && other == nil
	}
	if !passwordEqual(c.Password, other.Password) {
		return false
	}
	return tlsEqual(c.TLS, other.TLS)
}

func passwordEqual(a, b *PasswordCredential) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Username == b.Username && a.Password == b.Password
}

func tlsEqual(a, b *TLSMaterial) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.CertPEM != b.CertPEM || a.KeyPEM != b.KeyPEM {
		return false
	}
	if a.InsecureSkipVerify != b.InsecureSkipVerify {
		return false
	}
	if len(a.CAPEMs) != len(b.CAPEMs) {
		return false
	}
	for i := range a.CAPEMs {
		if a.CAPEMs[i] != b.CAPEMs[i] {
			return false
		}
	}
	return true
}
