package credentials

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

type Resolver struct {
	mu       *sync.RWMutex
	registry []types.CredentialsRepository
}

// NewResolver creates a new Credentials Repository Resolver.
func NewResolver() *Resolver {
	return &Resolver{
		mu:       &sync.RWMutex{},
		registry: make([]types.CredentialsRepository, 0),
	}
}

// RegisterRepository adds a repository to the registry.
// Should be called during initialization (before lookups).
func (r *Resolver) RegisterRepository(repo types.CredentialsRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry = append(r.registry, repo)
}

// ResolveRepository returns the best matching repository for the given serverURI.
//
// Returns (repo, true) if found, or (nil, false) if none matches.
func (r *Resolver) ResolveRepository(serverURI string) (types.CredentialsRepository, bool, error) {
	u, err := url.Parse(serverURI)
	if err != nil {
		return nil, false, fmt.Errorf("invalid server URI %q: %w", serverURI, err)
	}

	scheme := u.Scheme
	// Combine host + path for full namespace comparison
	path := strings.Trim(strings.TrimPrefix(u.Host+"/"+strings.Trim(u.Path, "/"), "/"), "/")

	r.mu.RLock()
	defer r.mu.RUnlock()

	var (
		bestMatch        types.CredentialsRepository
		bestNamespaceLen = -1
	)

	for _, repo := range r.registry {
		if repo.GetScheme() != scheme {
			continue
		}
		ns := strings.Trim(repo.GetNamespace(), "/")
		if ns == "" {
			if bestMatch == nil && bestNamespaceLen < 0 {
				bestMatch = repo
				bestNamespaceLen = 0
			}
			continue
		}
		if path == ns || strings.HasPrefix(path, ns+"/") {
			if len(ns) > bestNamespaceLen {
				bestMatch = repo
				bestNamespaceLen = len(ns)
			}
		}
	}

	if bestMatch == nil {
		return nil, false, nil
	}
	return bestMatch, true, nil
}
