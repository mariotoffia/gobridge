package awsstore_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// errRoundTripper fails every HTTP request, simulating a DescribeTable that
// never gets a useful answer from DynamoDB — a control-plane throttle, a network
// partition, or an AccessDenied on a least-privilege role.
type errRoundTripper struct{ err error }

func (c errRoundTripper) Do(*http.Request) (*http.Response, error) { return nil, c.err }

// staticCreds is a hermetic, offline credentials provider so the SDK signs the
// request and actually calls the fake HTTP client instead of probing the
// environment or IMDS (which would make the test non-deterministic).
type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", Source: "test"}, nil
}

// failingDescribeClient returns a fully offline *dynamodb.Client whose every
// call (DescribeTable included) fails with a throttle-classified transport
// error, so preflight sees a COULD-NOT-VERIFY result without any network.
//
// Coupling note: the literal "throttle" in the transport error is what makes
// mapError classify the failure as ErrThrottled (substring match). If the SDK
// ever stops surfacing that substring, classification falls back to the safe
// ErrUnavailable and only the ErrThrottled sentinel assertion below would need
// relaxing — the fail-closed guarantees (err != nil, "could not verify",
// not ErrInvalidConfig) hold regardless. The deterministic sentinel taxonomy is
// pinned separately in the internal posture test.
func failingDescribeClient() *dynamodb.Client {
	return dynamodb.New(dynamodb.Options{
		Region:      "us-east-1",
		Credentials: staticCreds{},
		Retryer:     aws.NopRetryer{},
		HTTPClient:  errRoundTripper{err: errors.New("simulated DescribeTable control-plane throttle")},
	})
}

// factoryBuilders is every store role driven through the factory, so each
// posture test exercises all three (outbox, dlq, lease) preflight seams.
func factoryBuilders(ctx context.Context, cfg *awsstore.DynamoDBConfig) []struct {
	name  string
	build func(*awsstore.DynamoDBStoreFactory) error
} {
	return []struct {
		name  string
		build func(*awsstore.DynamoDBStoreFactory) error
	}{
		{"outbox", func(f *awsstore.DynamoDBStoreFactory) error {
			_, err := f.NewOutboxStore(ctx, cfg, ports.OutboxRuntimeOptions{})
			return err
		}},
		{"dlq", func(f *awsstore.DynamoDBStoreFactory) error {
			_, err := f.NewDLQStore(ctx, cfg)
			return err
		}},
		{"lease", func(f *awsstore.DynamoDBStoreFactory) error {
			_, err := f.NewLeaseStore(ctx, cfg)
			return err
		}},
	}
}

// TestFactoryPreflightFailsClosedOnDescribeTableError is the c13-preflight-failopen
// regression, end-to-end through a real (but offline) *dynamodb.Client whose
// DescribeTable ALWAYS fails. An inability to VERIFY the schema is NOT proof the
// table is valid, so by default the factory must FAIL CLOSED — block startup —
// rather than swallow the error and build the store as if the table were sound.
//
// Mutation reasoning: before the fix the factory logged a WARN and returned nil
// on any non-ErrInvalidConfig Preflight error, so a role missing
// dynamodb:DescribeTable pointed at a mis-shaped table booted cleanly and then
// shredded messages (first record per partition writes, the rest ack-and-drop as
// "duplicates"). This test asserts a NON-nil build error, so it fails on the
// unfixed (fail-open) code and passes only once the swallow is closed. The
// classified sentinel (ErrThrottled) is preserved and the error is NOT
// ErrInvalidConfig (which stays reserved for a CONFIRMED schema mismatch).
func TestFactoryPreflightFailsClosedOnDescribeTableError(t *testing.T) {
	ctx := context.Background()
	cfg := &awsstore.DynamoDBConfig{TableName: "misconfigured-or-throttled"}

	for _, tc := range factoryBuilders(ctx, cfg) {
		t.Run(tc.name, func(t *testing.T) {
			f := awsstore.NewDynamoDBStoreFactory(failingDescribeClient())
			err := tc.build(f)
			if err == nil {
				t.Fatal("an unverifiable DescribeTable must FAIL CLOSED (construction fails), got nil " +
					"(pre-fix behaviour: fail-open build → silent message shredder on a mis-shaped table)")
			}
			if !errors.Is(err, shared.ErrThrottled) {
				t.Fatalf("fail-closed error must preserve the classified transport sentinel, got: %v", err)
			}
			if errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("a could-not-verify error must NOT be reported as a confirmed schema mismatch, got: %v", err)
			}
			if !strings.Contains(err.Error(), "could not verify") {
				t.Fatalf("fail-closed error must explain it could not verify the schema, got: %v", err)
			}
		})
	}
}

// TestFactoryPreflightAdvisoryFailsOpenOnDescribeTableError proves the EXPLICIT
// dev/emulator opt-out: with WithSchemaPreflightAdvisory the SAME unverifiable
// DescribeTable downgrades to a loud WARN and builds the store (fail open), so a
// local emulator that cannot answer DescribeTable is not bricked. The WARN names
// the required IAM action and the offending table.
func TestFactoryPreflightAdvisoryFailsOpenOnDescribeTableError(t *testing.T) {
	ctx := context.Background()
	cfg := &awsstore.DynamoDBConfig{TableName: "emulator-no-describe"}

	for _, tc := range factoryBuilders(ctx, cfg) {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			f := awsstore.NewDynamoDBStoreFactory(
				failingDescribeClient(),
				awsstore.WithLogger(logger),
				awsstore.WithSchemaPreflightAdvisory(),
			)
			if err := tc.build(f); err != nil {
				t.Fatalf("advisory opt-out must fail OPEN (construction succeeds), got: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "schema preflight skipped") {
				t.Fatalf("advisory fail-open must emit the skip WARN, got: %q", out)
			}
			if !strings.Contains(out, "dynamodb:DescribeTable") {
				t.Fatalf("WARN must name the required IAM action, got: %q", out)
			}
			if !strings.Contains(out, cfg.TableName) {
				t.Fatalf("WARN should name the table %q, got: %q", cfg.TableName, out)
			}
		})
	}
}
