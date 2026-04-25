package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// PullCredentialStore fetches credentials on demand by URI. The URI
// scheme selects the backend (e.g. "file://", "pms://", "vault://").
// This is the canonical pull-style contract used by stateless lookups
// at session/receiver/sender construction time.
//
// Implementations: native file, AWS SSM, HashiCorp Vault, etc.
type PullCredentialStore interface {
	Resolve(ctx context.Context, uri string) (*domain.CredentialSet, error)
}

// PushCredentialStore observes credential rotation for a given URI.
// When the backing credential changes, a new *domain.CredentialSet is
// published on the returned channel. Callers are expected to apply the
// new credentials to any long-lived session bound to the URI (usually
// by forcing a reconnect or updating the next connect packet).
//
// Contract:
//   - The returned channel is closed when ctx is cancelled, the store
//     is closed, or a terminal backend error occurs.
//   - Implementations MUST dedup: emit only when credentials actually
//     changed per domain.CredentialSet.Equal.
//   - The first value MAY be emitted eagerly (initial snapshot) or lazily
//     on first rotation — both are acceptable; callers should not rely
//     on either timing.
//
// Implementations: runtime.PollBasedWrapper (timer over a Pull store),
// Vault lease watchers, Kubernetes secret informers, etc.
type PushCredentialStore interface {
	Watch(ctx context.Context, uri string) (<-chan *domain.CredentialSet, error)
}

// CredentialStore is the historical alias for PullCredentialStore. It
// is preserved so pre-refresh consumers (builder options, adapters,
// existing tests) keep compiling unchanged.
//
// Why: introducing two explicit interfaces (pull/push) without breaking
// the current codebase requires an alias, because many call sites
// accept `ports.CredentialStore` directly and type-switch on the
// concrete store. Aliasing keeps assignability intact.
type CredentialStore = PullCredentialStore

// PollBasedWrapperConfig configures a poll-based adaptation from a
// PullCredentialStore into a PushCredentialStore. The wrapper lives in
// the runtime package (runtime.NewPollBasedWrapper) so the ports layer
// stays interface-only.
type PollBasedWrapperConfig struct {
	// PollInterval is the cadence at which Resolve is invoked. A zero
	// or negative value causes runtime.NewPollBasedWrapper to use
	// DefaultCredentialPollInterval (5 minutes).
	PollInterval time.Duration
	// Jitter, when > 0, randomises each poll by up to ±Jitter to spread
	// load across many concurrent watchers. Zero disables jitter.
	Jitter time.Duration
	// EmitOnStart causes the wrapper to publish the first successful
	// resolve before starting the ticker. Default (false) means the
	// first emission happens on the first detected rotation.
	EmitOnStart bool
}

// CredentialRepository is a backend adapter for a specific credential
// storage system. Implementations register a URI scheme and optional
// namespace prefix for longest-prefix dispatch.
type CredentialRepository interface {
	Scheme() string
	Namespace() string
	Get(ctx context.Context, uri string) (*domain.CredentialSet, error)
}

// CredentialAdmin extends CredentialRepository with write operations
// for credential lifecycle management.
type CredentialAdmin interface {
	CredentialRepository
	Create(ctx context.Context, uri string, creds *domain.CredentialSet) error
	Update(ctx context.Context, uri string, creds *domain.CredentialSet, version int64) error
	Delete(ctx context.Context, uri string, version int64) error
	List(ctx context.Context, prefix string) ([]string, error)
}
