package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// rebuildClientWithStatic loads a fresh AWS config with the given
// static credentials provider applied. SQS is stateless per-request,
// so swapping the client on the next call picks up rotated keys with
// no connection churn.
//
// The session token is left empty: gobridge's connectivity.CredentialSet
// models only username/password; SSO/STS rotation requires richer
// material, which should be added alongside a CredentialKind when
// needed. This keeps the MVP safe: existing static-key users see
// rotation work; SSO users keep using the SDK's own provider chain.
func rebuildSQSClient(ctx context.Context, region, endpoint, profile string, creds *connectivity.PasswordCredential) (sqsAPI, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if creds != nil {
		provider := credentials.NewStaticCredentialsProvider(creds.Username, creds.Password, "")
		opts = append(opts, awsconfig.WithCredentialsProvider(provider))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, shared.ErrTemporaryAuthFailure.Wrap(fmt.Errorf("sqs: load AWS config: %w", err))
	}
	if endpoint != "" {
		cfg.BaseEndpoint = aws.String(endpoint)
	}
	return awssqs.NewFromConfig(cfg), nil
}

// ApplyCredentials rotates the Sender's AWS credentials. A new *sqs.Client
// is built with the new static provider and atomically swapped in. Calls
// already in flight continue to use the previous client, since they hold
// a local reference; subsequent sends pick up the new credentials on the
// next SendMessage call.
//
// TLS material on CredentialSet is ignored: SQS runs over HTTPS with
// the SDK's default trust store. Leaf-cert pinning would be configured
// on the awshttp client and is out of scope here.
func (s *Sender) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil || set.Password == nil {
		return nil
	}
	client, err := rebuildSQSClient(ctx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile, set.Password)
	if err != nil {
		return err
	}

	s.initMu.Lock()
	s.client = client
	s.initMu.Unlock()
	return nil
}

// ApplyCredentials rotates the Receiver's AWS credentials. Same
// contract as Sender.ApplyCredentials.
func (r *Receiver) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil || set.Password == nil {
		return nil
	}
	client, err := rebuildSQSClient(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile, set.Password)
	if err != nil {
		return err
	}

	r.initMu.Lock()
	r.client = client
	r.initMu.Unlock()
	return nil
}
