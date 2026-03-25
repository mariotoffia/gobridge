package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
)

// CredentialStore resolves credentials by URI. The URI scheme selects
// the backend (e.g. "file://", "pms://", "vault://"). This is the
// primary driven port for credential resolution.
type CredentialStore interface {
	Resolve(ctx context.Context, uri string) (*domain.CredentialSet, error)
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
