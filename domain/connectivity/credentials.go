package connectivity

import "github.com/mariotoffia/gobridge/domain/shared"

// CredentialKind identifies the type of credential material.
type CredentialKind string

const (
	CredentialPassword CredentialKind = "password"
	CredentialTLS      CredentialKind = "tls"
)

// PasswordCredential is an immutable value object holding username/password
// authentication material. The password is wrapped in a shared.Secret so it
// cannot leak through logs or formatting. Construct via NewPasswordCredential
// and read fields through the accessors.
type PasswordCredential struct {
	username string
	password shared.Secret
}

// NewPasswordCredential builds a PasswordCredential from a username and a
// plaintext password.
func NewPasswordCredential(username, password string) PasswordCredential {
	return PasswordCredential{
		username: username,
		password: shared.NewSecret(password),
	}
}

// Username returns the username.
func (p PasswordCredential) Username() string { return p.username }

// Password returns the redaction-safe secret. Call Reveal at the adapter
// boundary to obtain the raw value.
func (p PasswordCredential) Password() shared.Secret { return p.password }

func (PasswordCredential) String() string   { return "PasswordCredential{REDACTED}" }
func (PasswordCredential) GoString() string { return "PasswordCredential{REDACTED}" }

// TLSMaterial is an immutable value object holding TLS certificate and key
// material. The private key is wrapped in a shared.Secret. Construct via
// NewTLSMaterial, which defensively copies the CA bundle; CAPEMs likewise
// returns a fresh copy so the stored material can never be mutated by callers.
type TLSMaterial struct {
	certPEM            string
	keyPEM             shared.Secret
	caPEMs             []string
	insecureSkipVerify bool
}

// NewTLSMaterial builds TLSMaterial, defensively copying caPEMs so later
// mutation of the caller's slice cannot alter the stored material.
func NewTLSMaterial(certPEM, keyPEM string, caPEMs []string, insecureSkipVerify bool) TLSMaterial {
	var cas []string
	if len(caPEMs) > 0 {
		cas = make([]string, len(caPEMs))
		copy(cas, caPEMs)
	}
	return TLSMaterial{
		certPEM:            certPEM,
		keyPEM:             shared.NewSecret(keyPEM),
		caPEMs:             cas,
		insecureSkipVerify: insecureSkipVerify,
	}
}

// CertPEM returns the certificate PEM.
func (t TLSMaterial) CertPEM() string { return t.certPEM }

// KeyPEM returns the redaction-safe private-key secret. Call Reveal at the
// adapter boundary to obtain the raw PEM.
func (t TLSMaterial) KeyPEM() shared.Secret { return t.keyPEM }

// CAPEMs returns a fresh copy of the CA PEM bundle so callers cannot mutate
// the stored material. It returns nil when no CA material is present.
func (t TLSMaterial) CAPEMs() []string {
	if len(t.caPEMs) == 0 {
		return nil
	}
	out := make([]string, len(t.caPEMs))
	copy(out, t.caPEMs)
	return out
}

// InsecureSkipVerify reports whether certificate verification is disabled.
func (t TLSMaterial) InsecureSkipVerify() bool { return t.insecureSkipVerify }

func (TLSMaterial) String() string   { return "TLSMaterial{REDACTED}" }
func (TLSMaterial) GoString() string { return "TLSMaterial{REDACTED}" }

// CredentialSet is a composite credential container. A single URI
// resolution can yield both password and TLS material. Nil fields
// indicate the credential kind is not present. The pointed-to value
// objects are immutable.
//
// aggregate-root
type CredentialSet struct {
	password *PasswordCredential
	tls      *TLSMaterial
}

// NewCredentialSet builds a credential set from its (immutable) value-object
// parts. Either may be nil.
func NewCredentialSet(password *PasswordCredential, tls *TLSMaterial) *CredentialSet {
	return &CredentialSet{password: password, tls: tls}
}

// Password returns the password credential, or nil if absent.
func (c *CredentialSet) Password() *PasswordCredential {
	if c == nil {
		return nil
	}
	return c.password
}

// TLS returns the TLS material, or nil if absent.
func (c *CredentialSet) TLS() *TLSMaterial {
	if c == nil {
		return nil
	}
	return c.tls
}

func (c *CredentialSet) String() string   { return "CredentialSet{REDACTED}" }
func (c *CredentialSet) GoString() string { return "CredentialSet{REDACTED}" }

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
	if !passwordEqual(c.password, other.password) {
		return false
	}
	return tlsEqual(c.tls, other.tls)
}

func passwordEqual(a, b *PasswordCredential) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.username == b.username && a.password.Equal(b.password)
}

func tlsEqual(a, b *TLSMaterial) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.certPEM != b.certPEM || !a.keyPEM.Equal(b.keyPEM) {
		return false
	}
	if a.insecureSkipVerify != b.insecureSkipVerify {
		return false
	}
	if len(a.caPEMs) != len(b.caPEMs) {
		return false
	}
	for i := range a.caPEMs {
		if a.caPEMs[i] != b.caPEMs[i] {
			return false
		}
	}
	return true
}
