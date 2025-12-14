package pms

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/mariotoffia/gobridge/bridge/types"
)

const (
	// Scheme is the URI scheme for AWS Parameter Store credentials.
	Scheme = "pms"
	// DefaultCacheTTL is the default cache duration.
	DefaultCacheTTL = 5 * time.Minute
)

// Repository implements types.CredentialsRepository for AWS Parameter Store.
type Repository struct {
	config Config
	client *ssm.Client
	cache  map[string]*cacheEntry
	mu     sync.RWMutex
}

// cacheEntry holds a cached credential with expiry.
type cacheEntry struct {
	credentials *types.Credentials
	expiresAt   time.Time
}

// New creates a new AWS Parameter Store credentials repository.
func New(ctx context.Context, opts ...Option) (*Repository, error) {
	r := &Repository{
		config: Config{
			CacheTTL: DefaultCacheTTL,
		},
		cache: make(map[string]*cacheEntry),
	}

	for _, opt := range opts {
		opt(r)
	}

	// Create SSM client if not provided
	if r.client == nil {
		cfg, err := r.loadAWSConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		r.client = ssm.NewFromConfig(cfg)
	}

	return r, nil
}

// GetScheme returns the URI scheme for this repository.
func (r *Repository) GetScheme() string {
	return Scheme
}

// GetNamespace returns the namespace filter for this repository.
func (r *Repository) GetNamespace() string {
	return r.config.Namespace
}

// GetCredentials retrieves credentials from AWS Parameter Store.
// URI format: pms://namespace/path/to/parameter
func (r *Repository) GetCredentials(serverURI string) (*types.Credentials, error) {
	// Parse the URI
	paramPath, err := r.parseURI(serverURI)
	if err != nil {
		return nil, err
	}

	// Check cache
	if creds := r.getCached(paramPath); creds != nil {
		return creds, nil
	}

	// Fetch from Parameter Store
	ctx := context.Background()
	result, err := r.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(paramPath),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get parameter %s: %w", paramPath, err)
	}

	if result.Parameter == nil || result.Parameter.Value == nil {
		return nil, fmt.Errorf("parameter %s has no value", paramPath)
	}

	// Parse the value
	creds, err := parseCredentials(*result.Parameter.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to parse credentials from %s: %w", paramPath, err)
	}

	// Cache the result
	r.setCached(paramPath, creds)

	return creds, nil
}

// parseURI parses a pms:// URI into a parameter path.
func (r *Repository) parseURI(serverURI string) (string, error) {
	u, err := url.Parse(serverURI)
	if err != nil {
		return "", fmt.Errorf("invalid URI: %w", err)
	}

	if u.Scheme != Scheme {
		return "", fmt.Errorf("expected scheme %s, got %s", Scheme, u.Scheme)
	}

	// Combine host and path to get the parameter path
	// pms://namespace/path → /namespace/path
	path := "/" + u.Host
	if u.Path != "" && u.Path != "/" {
		path = path + u.Path
	}

	// Clean up the path
	path = strings.TrimSuffix(path, "/")

	return path, nil
}

// getCached returns cached credentials if still valid.
func (r *Repository) getCached(key string) *types.Credentials {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.cache[key]
	if !ok {
		return nil
	}

	if time.Now().After(entry.expiresAt) {
		return nil
	}

	return entry.credentials
}

// setCached caches credentials.
func (r *Repository) setCached(key string, creds *types.Credentials) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache[key] = &cacheEntry{
		credentials: creds,
		expiresAt:   time.Now().Add(r.config.CacheTTL),
	}
}

// ClearCache clears all cached credentials.
func (r *Repository) ClearCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]*cacheEntry)
}

// loadAWSConfig loads AWS configuration.
func (r *Repository) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}

	if r.config.Region != "" {
		opts = append(opts, config.WithRegion(r.config.Region))
	}

	return config.LoadDefaultConfig(ctx, opts...)
}

// Ensure Repository implements types.CredentialsRepository
var _ types.CredentialsRepository = (*Repository)(nil)
