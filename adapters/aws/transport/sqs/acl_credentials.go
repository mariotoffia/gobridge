package sqs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// SetAuthFailureCallback wires the reactive-recovery hook (HIGH-3) on the
// Sender, satisfying the bridge.AuthFailureReporter capability (matched
// structurally by the CredentialRefresher in another module). A nil callback
// clears it.
func (s *Sender) SetAuthFailureCallback(cb func(error)) {
	if cb == nil {
		s.authFailureCB.Store(nil)
		return
	}
	s.authFailureCB.Store(&cb)
}

// reportAuthFailure invokes the injected reactive-recovery callback iff err is
// an authorization failure. Called on the classified verdict of a live send: an
// error still inside the auth-grace window is ErrTemporaryAuthFailure and is
// filtered here, so only a genuine revocation (escalated to ErrNotAuthorized)
// forces a re-resolve. The callback is auth-gated and per-URI rate-limited.
func (s *Sender) reportAuthFailure(err error) {
	if err == nil || !errors.Is(err, shared.ErrNotAuthorized) {
		return
	}
	if cb := s.authFailureCB.Load(); cb != nil {
		(*cb)(err)
	}
}

// classify maps a live send error through the bounded auth-grace and reports a
// permanent authorization failure to the reactive-recovery hook (HIGH-3). It is
// the single chokepoint for both the single-send and batch-send paths.
func (s *Sender) classify(err error) *shared.BridgeError {
	be := s.authGrace.classify(err)
	s.reportAuthFailure(be)
	return be
}

// SetAuthFailureCallback wires the reactive-recovery hook (HIGH-3) on the
// Receiver. See Sender.SetAuthFailureCallback.
func (r *Receiver) SetAuthFailureCallback(cb func(error)) {
	if cb == nil {
		r.authFailureCB.Store(nil)
		return
	}
	r.authFailureCB.Store(&cb)
}

// reportAuthFailure mirrors Sender.reportAuthFailure for the receive path.
func (r *Receiver) reportAuthFailure(err error) {
	if err == nil || !errors.Is(err, shared.ErrNotAuthorized) {
		return
	}
	if cb := r.authFailureCB.Load(); cb != nil {
		(*cb)(err)
	}
}

// classify maps a live receive error through the bounded auth-grace and reports
// a permanent authorization failure to the reactive-recovery hook (HIGH-3).
func (r *Receiver) classify(err error) *shared.BridgeError {
	be := r.authGrace.classify(err)
	r.reportAuthFailure(be)
	return be
}

// ErrTemporaryCredentialsUnsupported is returned when a credential set
// carries STS/temporary material (an access-key id with the "ASIA"
// prefix, which is only valid together with a session token) that
// gobridge's username/password connectivity.CredentialSet cannot
// represent. Building a static provider with an empty session token
// would produce a client that fails every request with
// InvalidClientTokenId, so rotation would *degrade* a previously working
// ambient-role client. The adapter rejects the material up front and
// leaves the existing client in place instead (Finding 6). Surface it as
// shared.ErrNotAuthorized so callers classify it consistently.
var ErrTemporaryCredentialsUnsupported = errors.New(
	"sqs: temporary/STS credentials (ASIA-prefixed access key) are unsupported; " +
		"the credential set carries no session token")

// isTemporaryAccessKeyID reports whether an AWS access-key id denotes
// temporary (STS-issued) credentials. AWS long-term keys are prefixed
// "AKIA"; temporary keys use "ASIA" and require an accompanying session
// token that gobridge's password credential cannot carry.
//
// Detection is PREFIX-based (ASIA), not token-based: it cannot see a
// session token because connectivity.PasswordCredential has no field for
// one. A temporary credential presented without the "ASIA" prefix would
// therefore slip through and produce a client that fails every request —
// but AWS always issues STS keys with the "ASIA" prefix, so in practice
// the prefix check catches every temporary credential shape.
func isTemporaryAccessKeyID(id string) bool {
	return strings.HasPrefix(id, "ASIA")
}

// rebuildSQSClient loads a fresh AWS config with the given static
// credentials provider applied. SQS is stateless per-request, so
// swapping the client on the next call picks up rotated keys with no
// connection churn.
//
// Temporary (STS) material is rejected — see
// ErrTemporaryCredentialsUnsupported — because the connectivity model
// carries only username/password, so a session token cannot be applied
// and a static provider with an empty token would brick the client.
// Long-term static-key users see rotation work; ambient-role/SSO users
// keep using the SDK's own provider chain (creds == nil).
func rebuildSQSClient(ctx context.Context, region, endpoint, profile string, creds *connectivity.PasswordCredential) (sqsAPI, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if creds != nil {
		if isTemporaryAccessKeyID(creds.Username()) {
			return nil, shared.ErrNotAuthorized.Wrap(ErrTemporaryCredentialsUnsupported)
		}
		provider := credentials.NewStaticCredentialsProvider(creds.Username(), creds.Password().Reveal(), "")
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
// already in flight continue to use the previous client, since they read
// the client through an atomic snapshot; subsequent sends pick up the new
// credentials on the next SendMessage call. The swap is serialised under
// initMu so it cannot race the lazy-init/queue-URL resolution sequence
// and lose a rotation.
//
// TLS material on CredentialSet is ignored: SQS runs over HTTPS with
// the SDK's default trust store. Leaf-cert pinning would be configured
// on the awshttp client and is out of scope here.
func (s *Sender) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil || set.Password() == nil {
		return nil
	}
	client, err := rebuildSQSClient(ctx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile, set.Password())
	if err != nil {
		return err
	}

	s.initMu.Lock()
	s.storeClient(client)
	s.initMu.Unlock()
	return nil
}

// ApplyCredentials rotates the Receiver's AWS credentials. Same
// contract as Sender.ApplyCredentials.
func (r *Receiver) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil || set.Password() == nil {
		return nil
	}
	client, err := rebuildSQSClient(ctx, r.cfg.Region, r.cfg.Endpoint, r.cfg.Profile, set.Password())
	if err != nil {
		return err
	}

	r.initMu.Lock()
	r.storeClient(client)
	r.initMu.Unlock()
	return nil
}
