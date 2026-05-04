package ssm

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/mariotoffia/gobridge/domain"
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
type Repository struct {
	cfg        config
	client     ssmAPI
	clientOnce sync.Once
	clientErr  error
}

// Option configures a Repository.
type Option func(*Repository)

// WithClient sets a pre-configured SSM client (or mock for testing).
func WithClient(client ssmAPI) Option {
	return func(r *Repository) { r.client = client }
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
	return r
}

func (r *Repository) Scheme() string    { return scheme }
func (r *Repository) Namespace() string { return r.cfg.Namespace }

// Get retrieves credentials from AWS Parameter Store.
func (r *Repository) Get(ctx context.Context, uri string) (*domain.CredentialSet, error) {
	paramPath, err := parseURI(uri)
	if err != nil {
		return nil, err
	}

	if err := r.ensureClient(ctx); err != nil {
		return nil, err
	}

	result, err := r.client.GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           aws.String(paramPath),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, mapAWSError(err)
	}

	if result.Parameter == nil || result.Parameter.Value == nil {
		return nil, domain.ErrNotFound.WithMessage(
			fmt.Sprintf("SSM parameter %s has no value", paramPath),
		)
	}

	creds, err := parseCredentials(*result.Parameter.Value)
	if err != nil {
		return nil, fmt.Errorf("ssm: failed to parse credentials from %s: %w", paramPath, err)
	}

	return creds, nil
}

// Create creates new credentials in AWS Parameter Store.
func (r *Repository) Create(ctx context.Context, uri string, creds *domain.CredentialSet) error {
	if creds == nil {
		return fmt.Errorf("ssm: credential set must not be nil")
	}

	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	if err := r.ensureClient(ctx); err != nil {
		return err
	}

	value, err := serializeCredentialSet(creds)
	if err != nil {
		return err
	}

	_, err = r.client.PutParameter(ctx, &awsssm.PutParameterInput{
		Name:      aws.String(paramPath),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(false),
	})
	if err != nil {
		return mapAWSError(err)
	}

	return nil
}

// Update updates existing credentials in AWS Parameter Store.
// If version > 0, optimistic concurrency is enforced.
func (r *Repository) Update(ctx context.Context, uri string, creds *domain.CredentialSet, version int64) error {
	if creds == nil {
		return fmt.Errorf("ssm: credential set must not be nil")
	}

	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	if err := r.ensureClient(ctx); err != nil {
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

	_, err = r.client.PutParameter(ctx, &awsssm.PutParameterInput{
		Name:      aws.String(paramPath),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return mapAWSError(err)
	}

	return nil
}

// Delete removes credentials from AWS Parameter Store.
// If version > 0, optimistic concurrency is enforced.
func (r *Repository) Delete(ctx context.Context, uri string, version int64) error {
	paramPath, err := parseURI(uri)
	if err != nil {
		return err
	}

	if err := r.ensureClient(ctx); err != nil {
		return err
	}

	if version > 0 {
		if err := r.checkVersion(ctx, paramPath, version); err != nil {
			return err
		}
	}

	_, err = r.client.DeleteParameter(ctx, &awsssm.DeleteParameterInput{
		Name: aws.String(paramPath),
	})
	if err != nil {
		return mapAWSError(err)
	}

	return nil
}

// List lists all credential URIs under the given prefix.
func (r *Repository) List(ctx context.Context, prefix string) ([]string, error) {
	if err := r.ensureClient(ctx); err != nil {
		return nil, err
	}

	pathPrefix := "/" + r.cfg.Namespace
	if prefix != "" {
		pathPrefix = pathPrefix + "/" + prefix
	}
	pathPrefix = strings.TrimPrefix(pathPrefix, "//")
	if !strings.HasPrefix(pathPrefix, "/") {
		pathPrefix = "/" + pathPrefix
	}

	var uris []string
	var nextToken *string

	for {
		input := &awsssm.GetParametersByPathInput{
			Path:      aws.String(pathPrefix),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		}

		page, err := r.client.GetParametersByPath(ctx, input)
		if err != nil {
			return nil, mapAWSError(err)
		}

		for _, param := range page.Parameters {
			if param.Name != nil {
				uris = append(uris, pathToURI(*param.Name))
			}
		}

		if page.NextToken == nil {
			break
		}
		nextToken = page.NextToken
	}

	return uris, nil
}

func (r *Repository) ensureClient(ctx context.Context) error {
	if r.client != nil {
		return nil
	}

	r.clientOnce.Do(func() {
		cfg, err := buildAWSConfig(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile)
		if err != nil {
			r.clientErr = err
			return
		}
		r.client = awsssm.NewFromConfig(cfg)
	})
	return r.clientErr
}

// checkVersion performs best-effort optimistic concurrency. SSM does not
// support conditional PutParameter, so there is a TOCTOU window between
// this check and the subsequent write.
func (r *Repository) checkVersion(ctx context.Context, paramPath string, version int64) error {
	result, err := r.client.GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           aws.String(paramPath),
		WithDecryption: aws.Bool(false),
	})
	if err != nil {
		return mapAWSError(err)
	}
	if result.Parameter != nil && result.Parameter.Version != version {
		return domain.ErrVersionMismatch.WithMessage(
			fmt.Sprintf("expected version %d, got %d", version, result.Parameter.Version),
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
