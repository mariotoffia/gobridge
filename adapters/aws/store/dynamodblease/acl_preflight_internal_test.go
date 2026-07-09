package dynamodblease

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// preflightClient is a dynamoAPI seam for Preflight TTL-invariant tests. It
// always answers DescribeTable with the canonical VALID lease-table schema (so
// the schema check passes and the TTL check is reached) and returns a
// configurable DescribeTimeToLive status/error. Mutating seam methods are unused
// by Preflight and panic if called, keeping the test intent unambiguous.
type preflightClient struct {
	ttlStatus ddbtypes.TimeToLiveStatus
	ttlErr    error
	ttlNoDesc bool // return an output with a nil TimeToLiveDescription
}

func (c *preflightClient) DescribeTable(_ context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{
		Table: &ddbtypes.TableDescription{
			TableName: in.TableName,
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: aws.String(attrPK), KeyType: ddbtypes.KeyTypeHash},
			},
			AttributeDefinitions: []ddbtypes.AttributeDefinition{
				{AttributeName: aws.String(attrPK), AttributeType: ddbtypes.ScalarAttributeTypeS},
			},
		},
	}, nil
}

func (c *preflightClient) DescribeTimeToLive(_ context.Context, _ *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	if c.ttlErr != nil {
		return nil, c.ttlErr
	}
	if c.ttlNoDesc {
		return &dynamodb.DescribeTimeToLiveOutput{}, nil
	}
	return &dynamodb.DescribeTimeToLiveOutput{
		TimeToLiveDescription: &ddbtypes.TimeToLiveDescription{
			TimeToLiveStatus: c.ttlStatus,
		},
	}, nil
}

func (c *preflightClient) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	panic("preflightClient.PutItem must not be called by Preflight")
}

func (c *preflightClient) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	panic("preflightClient.UpdateItem must not be called by Preflight")
}

func (c *preflightClient) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	panic("preflightClient.GetItem must not be called by Preflight")
}

func (c *preflightClient) CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	panic("preflightClient.CreateTable must not be called by Preflight")
}

// Finding c13-lease-ttl-warn: an OBSERVED enabled (or enabling) DynamoDB TTL on
// the lease table is a correctness hazard — a reaper deleting a fence row resets
// its version and opens a split-brain window — so Preflight must FAIL FATALLY
// with shared.ErrInvalidConfig, not merely WARN.
//
// Mutation killed: downgrade the fatal ErrInvalidConfig return in
// checkLeaseTableTTL to a WARN + nil (the pre-fix warnIfTTLEnabled behaviour).
// Preflight then returns nil and this test FAILs on the "must be FATAL"
// assertion.
func TestPreflight_TTLEnabled_IsFatal(t *testing.T) {
	for _, status := range []ddbtypes.TimeToLiveStatus{
		ddbtypes.TimeToLiveStatusEnabled,
		ddbtypes.TimeToLiveStatusEnabling,
	} {
		t.Run(string(status), func(t *testing.T) {
			c := &preflightClient{ttlStatus: status}
			s := &Store{client: c, tableName: "leases-ttl-on", clk: clock.System}

			err := s.Preflight(context.Background())
			if err == nil {
				t.Fatal("an ENABLED DynamoDB TTL on the lease table must be FATAL " +
					"(fence rows must never be TTL-reaped), got nil (pre-fix WARN behaviour)")
			}
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("observed enabled TTL must classify as shared.ErrInvalidConfig, got %v", err)
			}
			if !strings.Contains(err.Error(), "leases-ttl-on") {
				t.Fatalf("fatal error must name the offending table, got %v", err)
			}
		})
	}
}

// The EXPLICIT dev/emulator opt-out: WithTTLPreflightAdvisory downgrades the SAME
// observed enabled TTL to a loud WARN and Preflight returns nil.
func TestPreflight_TTLEnabled_AdvisoryDowngradesToWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := &preflightClient{ttlStatus: ddbtypes.TimeToLiveStatusEnabled}
	s := &Store{client: c, tableName: "leases-dev", clk: clock.System, logger: logger}
	WithTTLPreflightAdvisory()(s) // exercise the exported functional option

	if err := s.Preflight(context.Background()); err != nil {
		t.Fatalf("WithTTLPreflightAdvisory must downgrade an enabled TTL to advisory (nil), got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TTL is ENABLED") {
		t.Fatalf("advisory downgrade must emit a loud WARN, got %q", out)
	}
	if !strings.Contains(out, "leases-dev") {
		t.Fatalf("advisory WARN should name the table, got %q", out)
	}
}

// A DescribeTimeToLive that FAILS proves nothing about the TTL state and must
// NOT silently pass: Preflight fails CLOSED. Crucially it returns the FACTORY's
// always-fatal marker (shared.ErrInvalidConfig) so that the SCHEMA-level
// WithSchemaPreflightAdvisory cannot silently relax this TTL check — only
// WithTTLPreflightAdvisory can. The classified transport cause is preserved for
// diagnostics.
//
// Mutation killed: revert the describe-error branch to `wrapErr(err, ...)` (a
// bare transient/auth sentinel). Then errors.Is(err, ErrInvalidConfig) is false,
// the factory would funnel it through its could-not-verify path (downgradable by
// WithSchemaPreflightAdvisory), and this test FAILs on the ErrInvalidConfig
// assertion.
func TestPreflight_DescribeTTLError_IsFatalFailClosed(t *testing.T) {
	c := &preflightClient{ttlErr: errors.New("AccessDeniedException: not authorized to DescribeTimeToLive")}
	s := &Store{client: c, tableName: "leases-noperm", clk: clock.System}

	err := s.Preflight(context.Background())
	if err == nil {
		t.Fatal("a DescribeTimeToLive error must FAIL CLOSED (it is not proof TTL is disabled), got nil")
	}
	if !strings.Contains(err.Error(), "could not verify") {
		t.Fatalf("fail-closed error must explain it could not verify the TTL state, got %v", err)
	}
	// MUST be the factory's always-fatal marker: acl_factory.go checks
	// errors.Is(err, ErrInvalidConfig) BEFORE its advisory branch, so this is what
	// prevents WithSchemaPreflightAdvisory from downgrading the TTL check.
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("an unverifiable TTL state must surface as the factory's always-fatal "+
			"marker shared.ErrInvalidConfig (so schema-advisory cannot relax it), got %v", err)
	}
	// The classified transport cause is preserved (wrapped) for diagnostics.
	if !errors.Is(err, shared.ErrNotAuthorized) {
		t.Fatalf("AccessDenied cause must remain matchable via errors.Is, got %v", err)
	}
}

// The same DescribeTimeToLive failure downgrades to a loud WARN (fail OPEN) under
// the explicit dev/emulator opt-out, naming the required IAM action.
func TestPreflight_DescribeTTLError_AdvisoryDowngradesToWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := &preflightClient{ttlErr: errors.New("emulator does not implement DescribeTimeToLive")}
	s := &Store{client: c, tableName: "leases-emu", clk: clock.System, logger: logger}
	WithTTLPreflightAdvisory()(s)

	if err := s.Preflight(context.Background()); err != nil {
		t.Fatalf("WithTTLPreflightAdvisory must fail OPEN on a DescribeTimeToLive error, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TTL preflight skipped") {
		t.Fatalf("advisory fail-open must emit the skip WARN, got %q", out)
	}
	if !strings.Contains(out, "dynamodb:DescribeTimeToLive") {
		t.Fatalf("advisory WARN must name the required IAM action, got %q", out)
	}
}

// A lease table with TTL DISABLED (or an emulator that answers with no TTL
// description) passes preflight cleanly — the fix must not turn a healthy table
// into a false positive.
func TestPreflight_TTLDisabled_Succeeds(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		c := &preflightClient{ttlStatus: ddbtypes.TimeToLiveStatusDisabled}
		s := &Store{client: c, tableName: "leases-ok", clk: clock.System}
		if err := s.Preflight(context.Background()); err != nil {
			t.Fatalf("a lease table with TTL DISABLED must pass preflight, got %v", err)
		}
	})
	t.Run("no_description", func(t *testing.T) {
		c := &preflightClient{ttlNoDesc: true}
		s := &Store{client: c, tableName: "leases-ok", clk: clock.System}
		if err := s.Preflight(context.Background()); err != nil {
			t.Fatalf("a nil TimeToLiveDescription must pass preflight (nothing observed enabled), got %v", err)
		}
	})
}
