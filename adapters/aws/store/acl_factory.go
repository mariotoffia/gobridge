package awsstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.StoreFactory                    = (*DynamoDBStoreFactory)(nil)
	_ ports.ManagedSubscriptionStoreFactory = (*DynamoDBStoreFactory)(nil)
	_ ports.DistributedStoreFactory         = (*DynamoDBStoreFactory)(nil)
)

// DynamoDBStoreFactory creates DynamoDB-backed lease, outbox, DLQ, and managed-subscription stores.
type DynamoDBStoreFactory struct {
	client *dynamodb.Client
	logger *slog.Logger
	// preflightAdvisory downgrades an UNVERIFIABLE schema preflight (a
	// DescribeTable that throttles / AccessDenies / is unsupported by an
	// emulator) from FAIL-CLOSED to a WARN-and-continue. Default false =
	// production fail-closed posture; set only via WithSchemaPreflightAdvisory.
	preflightAdvisory bool
	// ttlPreflightAdvisory downgrades the lease store's build-time TTL-invariant
	// check from FAIL-CLOSED to a WARN-and-continue. It is threaded into the
	// dynamodblease store as WithTTLPreflightAdvisory. Default false = production
	// fail-closed posture; set only via WithTTLPreflightAdvisory. This is the
	// factory-level parity lever for the TTL check: the SCHEMA advisory
	// (preflightAdvisory) does NOT relax the TTL check, because an OBSERVED enabled
	// TTL and an unverifiable TTL state are both returned as shared.ErrInvalidConfig
	// (matched at the top of preflight, before the schema-advisory branch).
	ttlPreflightAdvisory bool
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

// WithSchemaPreflightAdvisory downgrades the build-time DynamoDB schema
// preflight from FAIL-CLOSED (the production default) to advisory: when
// DescribeTable cannot VERIFY the target table — a control-plane throttle, a
// least-privilege role lacking dynamodb:DescribeTable (AccessDenied), or an
// emulator that does not implement DescribeTable — the factory logs a loud WARN
// and builds the store anyway instead of blocking startup.
//
// This is an EXPLICIT dev/emulator opt-out ONLY. Leave it unset in production:
// an inability to verify the schema is NOT proof the table is valid, and a role
// missing dynamodb:DescribeTable pointed at a mis-shaped table is precisely the
// silent-shredder scenario the preflight exists to catch (the first record per
// partition writes, the rest are acked-and-dropped as "duplicates"). A confirmed
// schema mismatch (shared.ErrInvalidConfig) stays FATAL regardless of this flag.
func WithSchemaPreflightAdvisory() FactoryOption {
	return func(f *DynamoDBStoreFactory) { f.preflightAdvisory = true }
}

// WithTTLPreflightAdvisory downgrades the lease store's build-time TTL-invariant
// preflight from FAIL-CLOSED (the production default) to advisory: an OBSERVED
// enabled/enabling DynamoDB TTL on the lease table — and a DescribeTimeToLive
// call that cannot verify the TTL state (missing dynamodb:DescribeTimeToLive, a
// throttle, or an emulator that does not implement it) — is logged as a loud WARN
// and the store is built instead of blocking startup.
//
// This is the factory-level parity lever for the lease store's
// dynamodblease.WithTTLPreflightAdvisory option, mirroring WithSchemaPreflightAdvisory
// for the schema check. It is an EXPLICIT dev/emulator opt-out ONLY. Leave it
// unset in production: the lease row IS the monotonic fencing counter of record,
// and a TTL reaper deleting a fence row resets its version to 1 while the outbox
// high-water mark sits at v≫1 — every subsequent claim then fails
// ErrStaleFencingToken and the partition splits/stalls. An inability to VERIFY the
// TTL state is likewise NOT proof it is disabled, so it fails closed too.
//
// Note it is INTENTIONALLY independent of WithSchemaPreflightAdvisory: the schema
// advisory does not relax the TTL check (the TTL failures surface as
// shared.ErrInvalidConfig, which the factory treats as always fatal before the
// schema-advisory branch), so ONLY this option can bridge a lease role that lacks
// dynamodb:DescribeTimeToLive during an upgrade.
func WithTTLPreflightAdvisory() FactoryOption {
	return func(f *DynamoDBStoreFactory) { f.ttlPreflightAdvisory = true }
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
// Posture (centralized here because every store's Preflight flows through it).
// The pivotal distinction is VERIFIED vs. COULD-NOT-VERIFY:
//
//   - Schema VERIFIED VALID → nil. DescribeTable succeeded and the key
//     schema/GSIs match. Also nil when the table is VERIFIED ABSENT
//     (ResourceNotFoundException → nil inside the store's Preflight) so a
//     build-then-provision flow stays valid; a genuinely missing table then
//     fails loudly at the first operation, not silently.
//
//   - Schema VERIFIED INVALID → FATAL. The store returns shared.ErrInvalidConfig
//     (via its schemaMismatch helper) for a genuine key-schema/GSI mismatch and
//     for that ONLY. This is the silent-shredder guard; a store pointed at
//     the wrong table shape must never boot.
//
//   - Schema COULD NOT BE VERIFIED → FATAL (fail CLOSED). A DescribeTable CALL
//     error — a control-plane throttle during a mass rollout of N pods ×
//     DescribeTable, a least-privilege role lacking dynamodb:DescribeTable
//     (AccessDenied), or DescribeTable being unsupported by an emulator — maps
//     to a transient/auth class (ErrThrottled/ErrUnavailable/ErrNotAuthorized/…)
//     but NEVER to shared.ErrInvalidConfig. Such an error proves NOTHING about
//     the table's shape, so it must NOT be swallowed as success: an unreadable +
//     mis-shaped table is exactly the shredder this preflight exists to catch.
//     The build fails with the classified error (wrapped for operator context)
//     so boot is blocked until dynamodb:DescribeTable is granted or the table is
//     confirmed correct out of band.
//
//     The one escape hatch is WithSchemaPreflightAdvisory(), an EXPLICIT
//     dev/emulator opt-out: with it set, an unverifiable table downgrades to a
//     loud WARN and fails OPEN. Production leaves it unset.
func (f *DynamoDBStoreFactory) preflight(ctx context.Context, s preflighter) error {
	if f.client == nil {
		return nil
	}
	err := s.Preflight(ctx)
	if err == nil {
		// Verified valid, or verified absent (build-then-provision): the store
		// maps ResourceNotFound to nil.
		return nil
	}
	if errors.Is(err, shared.ErrInvalidConfig) {
		// Verified present with the WRONG shape → hard fail (the shredder guard).
		return err
	}
	// Could-not-verify: DescribeTable failed (throttle / AccessDenied / emulator
	// gap). This is NOT evidence the table is valid.
	if !f.preflightAdvisory {
		// FAIL CLOSED: block boot rather than swallow an unverifiable schema. The
		// classified sentinel is preserved (%w) so callers can still match
		// ErrThrottled/ErrNotAuthorized/… ; it never becomes ErrInvalidConfig.
		return fmt.Errorf(
			"awsstore: DynamoDB schema preflight could not verify the table; "+
				"refusing to start (grant dynamodb:DescribeTable, or set "+
				"WithSchemaPreflightAdvisory for a dev/emulator that cannot "+
				"DescribeTable): %w", err)
	}
	// Explicit dev/emulator opt-out: WARN and fail OPEN.
	if f.logger != nil {
		attrs := []any{"error", err.Error()}
		var be *shared.BridgeError
		if errors.As(err, &be) {
			if table, ok := be.Context["table"]; ok {
				attrs = append(attrs, "table", table)
			}
		}
		f.logger.Warn(
			"awsstore: DynamoDB schema preflight skipped (DescribeTable failed) "+
				"under WithSchemaPreflightAdvisory; production must grant "+
				"dynamodb:DescribeTable so preflight can enforce the schema",
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
	tableName, err := ResolveDynamoDBTableName("lease", dc.TableName)
	if err != nil {
		return nil, err
	}
	opts := []dynamodblease.Option{dynamodblease.WithTableName(tableName)}
	if f.logger != nil {
		opts = append(opts, dynamodblease.WithLogger(f.logger))
	}
	if f.ttlPreflightAdvisory {
		// Factory-level parity with the lease store's TTL opt-out (dev/emulator
		// only); keeps the default fail-closed posture when unset.
		opts = append(opts, dynamodblease.WithTTLPreflightAdvisory())
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
	tableName, err := ResolveDynamoDBTableName("outbox", dc.TableName)
	if err != nil {
		return nil, err
	}
	opts := []dynamodboutbox.Option{dynamodboutbox.WithTableName(tableName)}
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

// NewManagedSubscriptionStore creates a dedicated exact-filter history and
// verifies its one-hash-key schema before exposing it to the runtime.
func (f *DynamoDBStoreFactory) NewManagedSubscriptionStore(ctx context.Context, cfg ports.PluginConfig) (ports.ManagedSubscriptionStore, error) { //nolint:ireturn // Factory port intentionally returns the narrow role interface.
	dc, err := dynamoDBConfigFromOrZero(cfg)
	if err != nil {
		return nil, err
	}
	tableName, err := ResolveDynamoDBTableName("managed_subscriptions", dc.TableName)
	if err != nil {
		return nil, err
	}
	opts := []dynamodbmanagedsubscriptions.Option{dynamodbmanagedsubscriptions.WithTableName(tableName)}
	if dc.OperationTimeout > 0 {
		opts = append(opts, dynamodbmanagedsubscriptions.WithOperationTimeout(dc.OperationTimeout))
	}
	store := dynamodbmanagedsubscriptions.NewStore(f.client, opts...)
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
	tableName, err := ResolveDynamoDBTableName("dlq", dc.TableName)
	if err != nil {
		return nil, err
	}
	opts := []dynamodbdlq.Option{dynamodbdlq.WithTableName(tableName)}
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
