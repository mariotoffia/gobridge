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

func failingDescribeClient() *dynamodb.Client {
	return dynamodb.New(dynamodb.Options{
		Region:      "us-east-1",
		Credentials: staticCreds{},
		Retryer:     aws.NopRetryer{},
		HTTPClient:  errRoundTripper{err: errors.New("simulated DescribeTable control-plane throttle")},
	})
}

// TestFactoryPreflightFailsOpenOnDescribeTableError is the FIX 1 end-to-end
// regression: a real store's Preflight running against a client whose
// DescribeTable ALWAYS fails (throttle/AccessDenied/network) must NOT brick
// construction. The factory logs a loud WARN and builds the store fail-open,
// because that error is transient/authz — never shared.ErrInvalidConfig, which
// only a SUCCESSFUL DescribeTable returning a wrong schema produces (and that
// stays fatal, covered by TestFactoryPreflightSchemaValidation).
//
// Counterfactual: pre-fix the factory returned the DescribeTable error fatally,
// so a pod that hit a boot-time control-plane throttle (worst during a mass
// rollout: N pods × 3 DescribeTable) — or ran under a role lacking
// dynamodb:DescribeTable — failed construction and bricked boot.
func TestFactoryPreflightFailsOpenOnDescribeTableError(t *testing.T) {
	ctx := context.Background()
	cfg := &awsstore.DynamoDBConfig{TableName: "misconfigured-or-throttled"}

	cases := []struct {
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

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			f := awsstore.NewDynamoDBStoreFactory(failingDescribeClient(), awsstore.WithLogger(logger))
			if err := tc.build(f); err != nil {
				t.Fatalf("a DescribeTable failure must fail OPEN (construction succeeds), got: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "schema preflight skipped") {
				t.Fatalf("fail-open must emit the skip WARN, got: %q", out)
			}
			if !strings.Contains(out, cfg.TableName) {
				t.Fatalf("WARN should name the table %q, got: %q", cfg.TableName, out)
			}
		})
	}
}
