package awsstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory            = (*DynamoDBStoreFactory)(nil)
	_ ports.DistributedStoreFactory = (*DynamoDBStoreFactory)(nil)
)

// DynamoDBStoreFactory creates DynamoDB-backed lease, outbox, and DLQ stores.
type DynamoDBStoreFactory struct {
	client *dynamodb.Client
	logger *slog.Logger
}

// FactoryOption configures a DynamoDBStoreFactory.
type FactoryOption func(*DynamoDBStoreFactory)

// WithLogger sets the structured logger threaded into every store the factory
// builds. The lease store uses it to WARN when DynamoDB TTL is enabled on the
// fencing table (Preflight); outbox and DLQ use it for provisioning
// diagnostics. A nil logger (the default) leaves each store's no-op sink in
// place.
func WithLogger(l *slog.Logger) FactoryOption {
	return func(f *DynamoDBStoreFactory) { f.logger = l }
}

// NewDynamoDBStoreFactory returns a factory that creates DynamoDB stores
// using the provided client.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL
// factory exists to wrap; it is injected by the composition root and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewDynamoDBStoreFactory(client *dynamodb.Client, opts ...FactoryOption) *DynamoDBStoreFactory {
	f := &DynamoDBStoreFactory{client: client}
	for _, o := range opts {
		o(f)
	}
	return f
}

// preflighter is implemented by every concrete DynamoDB store: a build-time
// schema (and, for the lease store, TTL) validation run before the store is
// handed to the runtime.
type preflighter interface {
	Preflight(ctx context.Context) error
}

// preflight runs a store's build-time validation, but only when the factory
// holds a real client. Wiring unit tests construct factories with a nil client
// (no DescribeTable is possible and the store is never used against DynamoDB),
// so skipping preflight there keeps construction pure while every production
// build path — which always injects a real client — validates the table.
//
// Posture (centralized here because every store's Preflight flows through it):
//
//   - Schema MISMATCH → FATAL. The store returns shared.ErrInvalidConfig (via
//     its schemaMismatch helper) for a genuine key-schema/GSI mismatch and for
//     that ONLY — a DescribeTable CALL error maps to a transient/auth class
//     (ErrThrottled/ErrUnavailable/ErrNotAuthorized/…), never ErrInvalidConfig.
//     This is the H3 silent-shredder guard; a store pointed at the wrong table
//     shape must never boot.
//   - ResourceNotFound → already nil inside the store (build-then-provision).
//   - Any OTHER Preflight error (a control-plane throttle during a mass rollout
//     of N pods × DescribeTable, a least-privilege role lacking
//     dynamodb:DescribeTable → AccessDenied, or DescribeTable being unsupported
//     by an emulator) → loud WARN and fail OPEN (return nil). Preflight is a
//     best-effort safety net: it catches the shredder when it can SEE the
//     schema and degrades gracefully when it cannot, mirroring the lease TTL
//     check's tolerance rather than bricking boot.
func (f *DynamoDBStoreFactory) preflight(ctx context.Context, s preflighter) error {
	if f.client == nil {
		return nil
	}
	err := s.Preflight(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, shared.ErrInvalidConfig) {
		return err
	}
	// ponytail: fail-open ceiling — a table that is genuinely mis-shaped but
	// UNREADABLE at boot (persistent AccessDenied / DescribeTable unsupported)
	// is not caught here. Grant dynamodb:DescribeTable so preflight can enforce
	// the schema; until then the missing/mismatched table fails loudly at the
	// first operation instead.
	if f.logger != nil {
		attrs := []any{"error", err.Error()}
		var be *shared.BridgeError
		if errors.As(err, &be) {
			if table, ok := be.Context["table"]; ok {
				attrs = append(attrs, "table", table)
			}
		}
		f.logger.Warn(
			"awsstore: DynamoDB schema preflight skipped (DescribeTable failed); "+
				"ensure dynamodb:DescribeTable is granted and the table schema matches doc.go",
			attrs...,
		)
	}
	return nil
}

// IsDistributed marks DynamoDB stores as cross-process coordination capable.
func (f *DynamoDBStoreFactory) IsDistributed() bool { return true }

// NewLeaseStore creates a DynamoDB-backed lease store from the typed config.
func (f *DynamoDBStoreFactory) NewLeaseStore(ctx context.Context, cfg ports.PluginConfig) (ports.LeaseStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodblease.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodblease.WithTableName(dc.TableName))
	}
	if f.logger != nil {
		opts = append(opts, dynamodblease.WithLogger(f.logger))
	}
	store := dynamodblease.NewStore(f.client, opts...)
	if err := f.preflight(ctx, store); err != nil {
		return nil, err
	}
	return store, nil
}

// NewOutboxStore creates a DynamoDB-backed outbox store from the typed config
// and runtime tuning options. The typed config's stale_claim_duration (when
// > 0) overrides the runtime-derived value; compaction_grace (when > 0)
// overrides the store's default item-TTL grace.
func (f *DynamoDBStoreFactory) NewOutboxStore(ctx context.Context, cfg ports.PluginConfig, runtime ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodboutbox.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodboutbox.WithTableName(dc.TableName))
	}
	if f.logger != nil {
		opts = append(opts, dynamodboutbox.WithLogger(f.logger))
	}
	staleClaim := runtime.StaleClaimDuration
	if dc.StaleClaimDuration > 0 {
		staleClaim = dc.StaleClaimDuration
	}
	if staleClaim > 0 {
		opts = append(opts, dynamodboutbox.WithStaleClaimDuration(staleClaim))
	}
	if dc.CompactionGrace > 0 {
		opts = append(opts, dynamodboutbox.WithCompactionGrace(dc.CompactionGrace))
	}
	// Thread the runtime exporter so store-level counters (e.g.
	// shared.MetricOutboxClaimConflicts) are actually emitted in
	// config-driven deployments. A nil exporter leaves the store's default
	// no-op meter in place (WithMetrics ignores nil), so this never installs
	// a dereferenceable-nil exporter.
	if runtime.Metrics != nil {
		opts = append(opts, dynamodboutbox.WithMetrics(runtime.Metrics))
	}
	store := dynamodboutbox.NewStore(f.client, opts...)
	if err := f.preflight(ctx, store); err != nil {
		return nil, err
	}
	return store, nil
}

// NewDLQStore creates a DynamoDB-backed DLQ store from the typed config.
// A positive retention enables item TTL on dead-letter entries; a
// non-zero max_scan_pages overrides the default scan bound (negative
// disables the bound).
func (f *DynamoDBStoreFactory) NewDLQStore(ctx context.Context, cfg ports.PluginConfig) (ports.DLQStore, error) {
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	var opts []dynamodbdlq.Option
	if dc.TableName != "" {
		opts = append(opts, dynamodbdlq.WithTableName(dc.TableName))
	}
	if f.logger != nil {
		opts = append(opts, dynamodbdlq.WithLogger(f.logger))
	}
	if dc.Retention > 0 {
		opts = append(opts, dynamodbdlq.WithRetention(dc.Retention))
	}
	if dc.MaxScanPages != 0 {
		opts = append(opts, dynamodbdlq.WithMaxScanPages(max(dc.MaxScanPages, 0)))
	}
	store := dynamodbdlq.NewStore(f.client, opts...)
	if err := f.preflight(ctx, store); err != nil {
		return nil, err
	}
	return store, nil
}

// dynamoDBConfigFromOrZero accepts a *DynamoDBConfig, DynamoDBConfig,
// or nil. Other concrete types are an error: an unexpected
// PluginConfig is a programming error in the composition root.
func dynamoDBConfigFromOrZero(cfg ports.PluginConfig) (DynamoDBConfig, error) {
	switch v := cfg.(type) {
	case nil:
		return DynamoDBConfig{}, nil
	case *DynamoDBConfig:
		if v == nil {
			return DynamoDBConfig{}, nil
		}
		return *v, nil
	case DynamoDBConfig:
		return v, nil
	default:
		return DynamoDBConfig{}, fmt.Errorf("awsstore: DynamoDB store requires a *DynamoDBConfig, got %T", cfg)
	}
}
