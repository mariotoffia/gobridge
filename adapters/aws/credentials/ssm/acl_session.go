package ssm

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// ssmAPI is the unexported subset of the SSM SDK client used by the
// adapter. The real *ssm.Client satisfies this interface; tests supply
// a mock.
//
// This file is the lifecycle/orchestration half of the SSM ACL: every
// AWS SDK construction the adapter performs flows through this file.
// Per-call request/response translation lives in acl_params.go and
// SDK error classification lives in acl_errors.go.
type ssmAPI interface {
	GetParameter(ctx context.Context, params *awsssm.GetParameterInput, optFns ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, params *awsssm.PutParameterInput, optFns ...func(*awsssm.Options)) (*awsssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, params *awsssm.DeleteParameterInput, optFns ...func(*awsssm.Options)) (*awsssm.DeleteParameterOutput, error)
	GetParametersByPath(ctx context.Context, params *awsssm.GetParametersByPathInput, optFns ...func(*awsssm.Options)) (*awsssm.GetParametersByPathOutput, error)
}

var _ ssmAPI = (*awsssm.Client)(nil)

// session wraps the SDK client handle and lazy-init wiring. It is the
// only place in the package allowed to import the AWS SDK config /
// client constructors. Repository holds a *session and never touches
// SDK types directly.
type session struct {
	region   string
	endpoint string
	profile  string

	preset ssmAPI

	// build overrides client construction; nil selects the default AWS
	// SDK path (see newClient). Injected only by tests so init-failure
	// recovery can be exercised without real AWS calls.
	build func(ctx context.Context) (ssmAPI, error)

	// mu guards client. Construction is retried on every failed attempt:
	// a transient failure is never memoised, only a successfully built
	// client is cached.
	mu     sync.Mutex
	client ssmAPI
}

// newSession builds a lazily-initialised session. If preset is non-nil
// the session will use it verbatim and skip AWS config loading; this
// is the test-injection path (WithClient).
func newSession(region, endpoint, profile string, preset ssmAPI) *session {
	return &session{
		region:   region,
		endpoint: endpoint,
		profile:  profile,
		preset:   preset,
	}
}

// ensure resolves the underlying ssmAPI handle, lazily building one
// from ambient AWS config on first use. Safe for concurrent use.
//
// A construction failure is deliberately NOT cached: a one-time init
// blip (transient IMDS/token/deadline error, or IAM-propagation delay
// on a cold/standby start) would otherwise poison credential
// resolution for the entire process lifetime and break failover — the
// standby's routes stay down until the pod restarts. Each call retries
// construction until it succeeds; only the successful client is cached.
func (s *session) ensure(ctx context.Context) (ssmAPI, error) {
	if s.preset != nil {
		return s.preset, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		return s.client, nil
	}

	client, err := s.newClient(ctx)
	if err != nil {
		return nil, err
	}
	s.client = client
	return s.client, nil
}

// newClient builds the underlying ssmAPI from ambient AWS config. The
// build hook is overridable in tests; production always takes the
// default SDK path.
func (s *session) newClient(ctx context.Context) (ssmAPI, error) {
	if s.build != nil {
		return s.build(ctx)
	}

	cfg, err := buildAWSConfig(ctx, s.region, s.endpoint, s.profile)
	if err != nil {
		return nil, err
	}
	return awsssm.NewFromConfig(cfg), nil
}

// buildAWSConfig loads the default AWS configuration with optional
// region / profile / endpoint overrides applied.
func buildAWSConfig(ctx context.Context, region, endpoint, profile string) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error

	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return cfg, shared.ErrUnavailable.Wrap(fmt.Errorf("ssm: load AWS config: %w", err))
	}

	if endpoint != "" {
		cfg.BaseEndpoint = aws.String(endpoint)
	}
	return cfg, nil
}
