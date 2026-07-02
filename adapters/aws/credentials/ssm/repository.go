package ssm

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

const scheme = "pms"

var (
	_ ports.CredentialRepository = (*Repository)(nil)
	_ ports.CredentialAdmin      = (*Repository)(nil)
)

type config struct {
	Region    string
	Namespace string
	Endpoint  string
	Profile   string
}

// Repository implements ports.CredentialRepository and ports.CredentialAdmin
// for AWS Systems Manager Parameter Store.
//
// All AWS SDK interactions are funnelled through the unexported
// *session (see acl_session.go); this file is intentionally free of
// aws-sdk-go-v2 imports so the domain-side logic stays reviewable in
// isolation.
type Repository struct {
	cfg     config
	session *session
	preset  ssmAPI
}

// Option configures a Repository.
type Option func(*Repository)

// WithClient sets a pre-configured SSM client (or mock for testing).
func WithClient(client ssmAPI) Option {
	return func(r *Repository) { r.preset = client }
}

// WithRegion sets the AWS region for client construction.
func WithRegion(region string) Option {
	return func(r *Repository) { r.cfg.Region = region }
}

// WithNamespace sets the namespace prefix for longest-prefix dispatch.
func WithNamespace(namespace string) Option {
	return func(r *Repository) { r.cfg.Namespace = namespace }
}

// WithEndpoint sets a custom SSM endpoint (e.g. for LocalStack).
func WithEndpoint(endpoint string) Option {
	return func(r *Repository) { r.cfg.Endpoint = endpoint }
}

// WithProfile sets the AWS shared-config profile.
func WithProfile(profile string) Option {
	return func(r *Repository) { r.cfg.Profile = profile }
}

// New creates a new AWS SSM credentials repository.
// The SSM client is built lazily on first use unless WithClient is provided.
func New(opts ...Option) *Repository {
	r := &Repository{}
	for _, o := range opts {
		o(r)
	}
	r.session = newSession(r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile, r.preset)
	return r
}

func (r *Repository) Scheme() string    { return scheme }
func (r *Repository) Namespace() string { return r.cfg.Namespace }

// Get retrieves credentials from AWS Parameter Store.
func (r *Repository) Get(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	paramPath, err := parseURI(uri)
	if err != nil {
		return nil, err
	}

	pv, err := r.session.getParameter(ctx, paramPath, true)
	if err != nil {
		return nil, err
	}

	creds, err := parseCredentials(pv.value)
	if err != nil {
		return nil, fmt.Errorf("ssm: failed to parse credentials from %s: %w", paramPath, err)
	}

	return creds, nil
}

// Create creates new credentials in AWS Parameter Store.
func (r *Repository) Create(ctx context.Context, uri string, creds *connectivity.CredentialSet) error {
	if creds == nil {
		return fmt.Errorf("ssm: credential set must not be nil")
	}

	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	value, err := serializeCredentialSet(creds)
	if err != nil {
		return err
	}

	return r.session.putParameter(ctx, paramPath, value, false)
}

// Update updates existing credentials in AWS Parameter Store.
//
// Concurrency: when version > 0 an optimistic version check is performed,
// but SSM has no conditional PutParameter, so the check and the subsequent
// write are NOT atomic (a TOCTOU window exists — see checkVersion). Two
// admins racing an update can therefore still clobber each other's write.
// This is acceptable for human-driven admin rotation; automated,
// high-frequency rotation that requires atomic CAS must use a CAS-capable
// backend (e.g. the DynamoDB config store) instead of SSM.
func (r *Repository) Update(ctx context.Context, uri string, creds *connectivity.CredentialSet, version int64) error {
	if creds == nil {
		return fmt.Errorf("ssm: credential set must not be nil")
	}

	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	if version > 0 {
		if err := r.checkVersion(ctx, paramPath, version); err != nil {
			return err
		}
	}

	value, err := serializeCredentialSet(creds)
	if err != nil {
		return err
	}

	return r.session.putParameter(ctx, paramPath, value, true)
}

// Delete removes credentials from AWS Parameter Store.
//
// Concurrency: when version > 0 an optimistic version check is performed,
// but as with Update the check and the DeleteParameter are NOT atomic (SSM
// has no conditional delete). See Update and checkVersion for the TOCTOU
// caveat and when to prefer a CAS-capable backend.
func (r *Repository) Delete(ctx context.Context, uri string, version int64) error {
	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	if version > 0 {
		if err := r.checkVersion(ctx, paramPath, version); err != nil {
			return err
		}
	}

	return r.session.deleteParameter(ctx, paramPath)
}

// List lists all credential URIs under the given prefix.
func (r *Repository) List(ctx context.Context, prefix string) ([]string, error) {
	pathPrefix := "/" + r.cfg.Namespace
	if prefix != "" {
		pathPrefix = pathPrefix + "/" + prefix
	}
	pathPrefix = strings.TrimPrefix(pathPrefix, "//")
	if !strings.HasPrefix(pathPrefix, "/") {
		pathPrefix = "/" + pathPrefix
	}

	names, err := r.session.listParameterNames(ctx, pathPrefix)
	if err != nil {
		return nil, err
	}

	uris := make([]string, 0, len(names))
	for _, name := range names {
		uris = append(uris, pathToURI(name))
	}
	return uris, nil
}

// checkVersion performs best-effort optimistic concurrency. SSM does not
// support conditional PutParameter, so there is a TOCTOU window between
// this check and the subsequent write.
func (r *Repository) checkVersion(ctx context.Context, paramPath string, version int64) error {
	got, err := r.session.getParameterVersion(ctx, paramPath)
	if err != nil {
		return err
	}
	if got != version {
		return shared.ErrVersionMismatch.WithMessage(
			fmt.Sprintf("expected version %d, got %d", version, got),
		)
	}
	return nil
}

// parseURI extracts the SSM parameter path from a pms:// URI.
func parseURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("ssm: invalid URI: %w", err)
	}

	if u.Scheme != scheme {
		return "", fmt.Errorf("ssm: expected scheme %s, got %s", scheme, u.Scheme)
	}

	path := "/" + u.Host
	if u.Path != "" && u.Path != "/" {
		path = path + u.Path
	}
	path = strings.TrimSuffix(path, "/")

	return path, nil
}

// pathToURI converts a parameter path back to a pms:// URI.
func pathToURI(path string) string {
	path = strings.TrimPrefix(path, "/")
	return fmt.Sprintf("%s://%s", scheme, path)
}
