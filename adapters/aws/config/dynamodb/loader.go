package dynamodb

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	defaultTableName          = "gobridge-config"
	defaultBridgeID           = "default"
	defaultPollInterval       = 30 * time.Second
	defaultStreamPollInterval = 500 * time.Millisecond

	// maxStreamBackoff caps the exponential backoff applied between
	// stream calls after throttles/errors. GetRecords shares a ~5 TPS
	// per-shard budget across ALL consumers of the stream, so backing
	// off far beyond the base cadence is what lets a fleet of bridge
	// instances coexist on one config table. The actual wait is
	// equal-jittered (see jitteredBackoff) so instances throttled at
	// the same instant desynchronise instead of retrying in lockstep.
	maxStreamBackoff = 30 * time.Second

	// streamAcquireFallbackAfter is the number of consecutive shard/
	// iterator acquisition failures after which the streams consumer
	// gives up and falls back to poll mode for the remainder of this
	// Watch (e.g. the stream was disabled or deleted after Watch
	// started). One Warn marks the switch; poll mode then guarantees
	// convergence at pollInterval instead of warn-spamming forever.
	streamAcquireFallbackAfter = 5

	// streamErrorsBeforeIteratorReset bounds how many consecutive
	// GetRecords failures are retried on the SAME iterator before it is
	// discarded. Throttles keep the iterator (position preserved);
	// persistent unknown errors eventually force a re-acquire, which is
	// followed by a version reconciliation to cover the gap.
	streamErrorsBeforeIteratorReset = 3

	// pollFailureEscalateAfter is the number of consecutive poll-cycle
	// failures after which the (rate-limited) diagnostic escalates from
	// Warn to Error — a persistent IAM/table problem must not hide at
	// Warn forever.
	pollFailureEscalateAfter = 10

	// pollFailureLogInterval rate-limits repeated poll-failure logs so a
	// broken dependency does not flood the log at pollInterval cadence.
	pollFailureLogInterval = time.Minute

	attrPK      = "PK"
	attrSK      = "SK"
	attrData    = "data"
	attrVersion = "version"

	skCurrent = "current"
)

// WatchMode selects the change-detection mechanism used by Watch.
type WatchMode int

const (
	// ModePoll periodically reads the table's version attribute at
	// PollInterval. This is the default: its steady-state cost is one
	// strongly consistent GetItem per instance per interval, which
	// scales safely to any fleet size. It is also the automatic
	// fallback when ModeStreams cannot run.
	ModePoll WatchMode = iota
	// ModeStreams uses DynamoDB Streams events for low-latency
	// push-based change detection, selected via WithWatchMode. On Watch
	// the loader probes the table for an enabled stream via
	// DescribeTable; if streams are not available on the table (or no
	// streams client was configured via WithStreamsClient) the loader
	// transparently falls back to ModePoll and emits a warning through
	// the configured logger.
	//
	// A DynamoDB stream shard serves roughly 5 GetRecords calls/sec
	// shared across ALL consumers, so prefer ModePoll for clustered
	// deployments (3+ instances) unless sub-second config propagation
	// is required.
	ModeStreams
)

var (
	_ ports.Loader   = (*Loader)(nil)
	_ ports.Reloader = (*Loader)(nil)
)

// Loader implements ports.Loader and ports.Reloader using a DynamoDB
// table. The full BridgeConfig is stored as a single JSON item with an
// accompanying numeric version attribute.
//
// All AWS SDK interactions are funnelled through the unexported
// *session (see acl_session.go); this file is intentionally free of
// aws-sdk-go-v2 imports so the domain-side logic stays reviewable in
// isolation.
//
// Two change-detection modes are supported, selectable via WithWatchMode:
//
//   - ModePoll (default): periodically compare the stored version
//     attribute. Safe at any fleet size.
//   - ModeStreams: consume DynamoDB Streams records on the table for
//     push-based updates. Falls back to ModePoll if streams are not
//     enabled or no streams client is configured, and after persistent
//     stream failures at runtime.
type Loader struct {
	session            *session
	bridgeID           string
	pollInterval       time.Duration
	streamPollInterval time.Duration
	mode               WatchMode
	logger             *slog.Logger
	clk                clock.Clock
	registry           *ports.Registry

	// randFloat supplies uniform values in [0,1) for backoff jitter
	// (jitteredBackoff). Production leaves it nil, which falls back to
	// math/rand/v2.Float64; tests inject a deterministic source.
	randFloat func() float64

	mu          sync.Mutex
	lastVersion int64
}

// Option configures a Loader.
type Option func(*Loader)

// WithTableName overrides the DynamoDB table name (default: "gobridge-config").
func WithTableName(name string) Option {
	return func(l *Loader) { l.session.tableName = name }
}

// WithBridgeID sets the bridge identifier used as the partition key
// prefix (default: "default").
func WithBridgeID(id string) Option {
	return func(l *Loader) { l.bridgeID = id }
}

// WithPollInterval sets the interval for Watch polling in ModePoll
// (default: 30s). Ignored in ModeStreams.
func WithPollInterval(d time.Duration) Option {
	return func(l *Loader) { l.pollInterval = d }
}

// WithWatchMode selects the change-detection mechanism used by Watch.
// The default is ModePoll. ModeStreams offers sub-second propagation
// but shares a ~5 GetRecords/sec per-shard budget across all consumers
// — prefer ModePoll for clustered deployments. ModeStreams falls back
// to ModePoll when streams are not available on the table.
func WithWatchMode(m WatchMode) Option {
	return func(l *Loader) { l.mode = m }
}

// WithStreamPollInterval sets the cadence at which the streams consumer
// issues GetRecords calls between shard polls (default: 500ms). This is
// the inter-GetRecords pause, not a full table poll — DynamoDB Streams
// is itself a polling API but at much higher frequency than a table
// version poll. Typical values are 100ms–1s.
func WithStreamPollInterval(d time.Duration) Option {
	return func(l *Loader) {
		if d > 0 {
			l.streamPollInterval = d
		}
	}
}

// WithRegistry sets the *ports.Registry used to decode the JSON
// payload stored in DynamoDB into typed PluginConfig values. The
// registry MUST be supplied (no global default exists); the loader
// surfaces a parse error if Load runs without one.
func WithRegistry(r *ports.Registry) Option {
	return func(l *Loader) { l.registry = r }
}

// WithLogger sets the logger used for diagnostic messages (mode
// fallbacks, stream errors). Nil is safe: diagnostics are suppressed.
func WithLogger(logger *slog.Logger) Option {
	return func(l *Loader) { l.logger = logger }
}

// WithClock overrides the clock used by the streams consumer for
// inter-poll cadence. Primarily intended for tests; production code
// should rely on the default clock.System.
func WithClock(c clock.Clock) Option {
	return func(l *Loader) {
		if c != nil {
			l.clk = c
		}
	}
}

func (l *Loader) pk() string { return "config#" + l.bridgeID }

// Load retrieves the current BridgeConfig from DynamoDB.
func (l *Loader) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	rawData, version, found, err := l.session.getConfigItem(ctx, l.pk())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, shared.ErrNotFound.WithMessage("config not found for bridge " + l.bridgeID)
	}

	cfg, err := parser.Parse(bytes.NewReader([]byte(rawData)), parser.FormatJSON, l.registry)
	if err != nil {
		return nil, fmt.Errorf("dynamodb config load: parse: %w", err)
	}

	if version > 0 {
		l.mu.Lock()
		l.lastVersion = version
		l.mu.Unlock()
	}

	return cfg, nil
}

// Watch observes the configured table for changes and emits updated
// configurations on the returned channel. The channel is closed when
// ctx is cancelled. The initial config is NOT emitted; call Load
// separately for the first load.
//
// In ModeStreams, Watch probes the table via DescribeTable for an
// enabled stream and, when present together with a configured streams
// client, consumes stream records for push-based updates. If streams
// are not available or no streams client has been supplied through
// WithStreamsClient, Watch transparently falls back to ModePoll and
// logs a warning. Persistent stream failures at runtime (e.g. the
// stream is disabled while watching) also degrade to poll mode after
// streamAcquireFallbackAfter consecutive acquisition failures.
func (l *Loader) Watch(ctx context.Context) (<-chan *ports.BridgeConfig, error) {
	ch := make(chan *ports.BridgeConfig, 1)

	if l.mode == ModeStreams {
		arn, reason := l.resolveStreamArn(ctx)
		if reason != "" {
			if l.logger != nil {
				l.logger.Warn("dynamodb config loader: falling back to poll mode",
					"reason", reason,
					"table", l.session.tableName,
				)
			}
		} else {
			go l.streamLoop(ctx, ch, arn)
			return ch, nil
		}
	}

	ticker := l.clk.NewTicker(l.pollInterval)
	go l.pollLoop(ctx, ch, ticker)
	return ch, nil
}

// resolveStreamArn returns the LatestStreamArn for the configured table
// when streams are enabled and usable. The second return value is a
// human-readable reason when streams cannot be used; when empty, the
// arn is valid.
func (l *Loader) resolveStreamArn(ctx context.Context) (string, string) {
	arn, reason, err := l.session.describeStreamArn(ctx)
	if err != nil {
		return "", err.Error()
	}
	return arn, reason
}

// pollLoop compares the stored version each tick and delivers a fresh
// config when it advanced. The version cursor (lastSeen) only moves
// after the corresponding config has been enqueued — deliverLatest
// cannot fail — so a change is never marked "seen" without being
// observable by the consumer (the file watcher's latest-wins fix,
// ported here).
//
// Failures (version read or Load) are logged with rate limiting and
// escalate from Warn to Error after pollFailureEscalateAfter
// consecutive failures: a silently broken IAM policy or deleted table
// must be visible in operations, not just "config stops updating".
func (l *Loader) pollLoop(ctx context.Context, ch chan *ports.BridgeConfig, ticker clock.Ticker) {
	defer close(ch)
	defer ticker.Stop()

	l.mu.Lock()
	lastSeen := l.lastVersion
	l.mu.Unlock()

	var consecutiveFailures int
	var lastFailureLog time.Time

	logFailure := func(stage string, err error) {
		consecutiveFailures++
		if l.logger == nil {
			return
		}
		now := l.clk.Now()
		first := consecutiveFailures == 1
		// The first escalation must not be swallowed by rate limiting:
		// it is the operational signal that config updates have stopped.
		escalating := consecutiveFailures == pollFailureEscalateAfter
		if !first && !escalating && now.Sub(lastFailureLog) < pollFailureLogInterval {
			return
		}
		lastFailureLog = now
		msg := "dynamodb config loader: poll cycle failed"
		attrs := []any{
			"stage", stage,
			"error", err,
			"table", l.session.tableName,
			"consecutive_failures", consecutiveFailures,
		}
		if consecutiveFailures >= pollFailureEscalateAfter {
			l.logger.Error(msg+"; config updates are NOT being applied", attrs...)
			return
		}
		l.logger.Warn(msg, attrs...)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			v, err := l.currentVersion(ctx)
			if err != nil {
				logFailure("version check", err)
				continue
			}

			if v == lastSeen {
				consecutiveFailures = 0
				continue
			}

			cfg, err := l.Load(ctx)
			if err != nil {
				logFailure("load", err)
				continue
			}
			consecutiveFailures = 0

			l.deliverLatest(ch, cfg)
			lastSeen = v
		}
	}
}

// deliverLatest enqueues cfg on the 1-buffered watch channel with
// latest-wins coalescing: when the consumer has not yet drained the
// previous update, that stale pending config is evicted and the newest
// enqueued in its place. Only a single producer goroutine (poll OR
// stream loop, never both) touches the send side, so the evict/enqueue
// pair cannot race another writer and the loop terminates in at most a
// couple of iterations. This guarantees the consumer always converges
// on the newest committed config — an update is never silently
// dropped.
func (l *Loader) deliverLatest(ch chan *ports.BridgeConfig, cfg *ports.BridgeConfig) {
	for {
		select {
		case ch <- cfg:
			return
		default:
		}
		select {
		case <-ch:
			if l.logger != nil {
				l.logger.Warn("dynamodb config loader: superseded a queued config before the consumer read it",
					"table", l.session.tableName)
			}
		default:
			// Consumer drained the slot concurrently; retry the send.
		}
	}
}

func (l *Loader) currentVersion(ctx context.Context) (int64, error) {
	return l.session.getCurrentVersion(ctx, l.pk())
}

// jitteredBackoff applies equal-jitter to a computed backoff base d:
// the returned wait is uniformly distributed in [d/2, d). Without
// jitter, every instance of a fleet throttled by the shared ~5 TPS
// GetRecords shard budget retries at the same deterministic instant
// and the fleet re-throttles itself in lockstep forever; equal-jitter
// spreads the retries across the window while still guaranteeing at
// least half the base backoff is honoured. A nil randFloat (loaders
// constructed as struct literals in tests) falls back to
// math/rand/v2.Float64.
func (l *Loader) jitteredBackoff(d time.Duration) time.Duration {
	rf := l.randFloat
	if rf == nil {
		rf = rand.Float64
	}
	half := d / 2
	return half + time.Duration(rf()*float64(half))
}

// Save writes a BridgeConfig to DynamoDB using an optimistic
// compare-and-set so concurrent admin writers cannot silently lose
// updates. It reads the current committed version with a strongly
// consistent read, then conditionally writes version+1 guarded on the
// stored version being unchanged. A concurrent write that advanced the
// version causes a shared.ErrVersionMismatch, which the caller should
// resolve by reloading and retrying.
func (l *Loader) Save(ctx context.Context, cfg *ports.BridgeConfig) error {
	data, err := parser.MarshalBridgeConfigJSON(cfg)
	if err != nil {
		return fmt.Errorf("dynamodb config save: marshal: %w", err)
	}

	current, err := l.currentVersion(ctx)
	if err != nil {
		return err
	}
	newVersion := current + 1

	if err := l.session.putConfigItem(ctx, l.pk(), data, newVersion, current); err != nil {
		if isConditionFailed(err) {
			return shared.ErrVersionMismatch.
				WithMessage("dynamodb config save: concurrent update detected; reload and retry").
				With("expectedVersion", current)
		}
		return err
	}

	l.mu.Lock()
	l.lastVersion = newVersion
	l.mu.Unlock()

	return nil
}

// EnsureTable creates the DynamoDB table if it does not already exist.
// When the loader runs in ModeStreams the table is provisioned with a
// KEYS_ONLY stream so self-provisioned deployments actually get the
// streams-based Watch they configured instead of silently degrading to
// poll mode. Intended for test setup and local development.
func (l *Loader) EnsureTable(ctx context.Context) error {
	if err := l.session.ensureTable(ctx, l.mode == ModeStreams); err != nil {
		return err
	}
	return l.session.waitTableExists(ctx, 30*time.Second)
}
