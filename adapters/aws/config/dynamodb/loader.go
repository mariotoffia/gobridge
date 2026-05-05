package dynamodb

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	defaultTableName          = "gobridge-config"
	defaultBridgeID           = "default"
	defaultPollInterval       = 30 * time.Second
	defaultStreamPollInterval = 500 * time.Millisecond

	attrPK      = "PK"
	attrSK      = "SK"
	attrData    = "data"
	attrVersion = "version"

	skCurrent = "current"
)

// WatchMode selects the change-detection mechanism used by Watch.
type WatchMode int

const (
	// ModeStreams uses DynamoDB Streams events for low-latency push-based
	// change detection. This is the default. On Watch the loader probes
	// the table for an enabled stream via DescribeTable; if streams are
	// not available on the table (or no streams client was configured via
	// WithStreamsClient) the loader transparently falls back to ModePoll
	// and emits a warning through the configured logger.
	ModeStreams WatchMode = iota
	// ModePoll periodically reads the table's version attribute at
	// PollInterval (legacy behaviour). This is selected automatically as
	// a fallback when streams are unavailable, or explicitly via
	// WithWatchMode(ModePoll).
	ModePoll
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
//   - ModeStreams (default): consume DynamoDB Streams records on the
//     table for push-based updates. Falls back to ModePoll if streams
//     are not enabled or no streams client is configured.
//   - ModePoll: periodically compare the stored version attribute.
type Loader struct {
	session            *session
	bridgeID           string
	pollInterval       time.Duration
	streamPollInterval time.Duration
	mode               WatchMode
	logger             *slog.Logger
	clk                clock.Clock

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
// The default is ModeStreams, which falls back to ModePoll when streams
// are not available on the table.
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
		return nil, domain.ErrNotFound.WithMessage("config not found for bridge " + l.bridgeID)
	}

	cfg, err := config.Parse(bytes.NewReader([]byte(rawData)), config.FormatJSON)
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
// In ModeStreams (default), Watch probes the table via DescribeTable
// for an enabled stream and, when present together with a configured
// streams client, consumes stream records for push-based updates.
// If streams are not available or no streams client has been supplied
// through WithStreamsClient, Watch transparently falls back to ModePoll
// and logs a warning.
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

func (l *Loader) pollLoop(ctx context.Context, ch chan<- *ports.BridgeConfig, ticker clock.Ticker) {
	defer close(ch)
	defer ticker.Stop()

	l.mu.Lock()
	lastSeen := l.lastVersion
	l.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			v, err := l.currentVersion(ctx)
			if err != nil {
				continue
			}

			if v == lastSeen {
				continue
			}

			cfg, err := l.Load(ctx)
			if err != nil {
				continue
			}
			lastSeen = v

			select {
			case ch <- cfg:
			default:
			}
		}
	}
}

func (l *Loader) currentVersion(ctx context.Context) (int64, error) {
	return l.session.getCurrentVersion(ctx, l.pk())
}

// Save writes a BridgeConfig to DynamoDB, auto-incrementing the version.
// This is useful for tests and admin tooling.
func (l *Loader) Save(ctx context.Context, cfg *ports.BridgeConfig) error {
	data, err := config.MarshalBridgeConfigJSON(cfg)
	if err != nil {
		return fmt.Errorf("dynamodb config save: marshal: %w", err)
	}

	l.mu.Lock()
	newVersion := l.lastVersion + 1
	l.mu.Unlock()

	if err := l.session.putConfigItem(ctx, l.pk(), data, newVersion); err != nil {
		return err
	}

	l.mu.Lock()
	l.lastVersion = newVersion
	l.mu.Unlock()

	return nil
}

// EnsureTable creates the DynamoDB table if it does not already exist.
// Intended for test setup and local development.
func (l *Loader) EnsureTable(ctx context.Context) error {
	if err := l.session.ensureTable(ctx); err != nil {
		return err
	}
	return l.session.waitTableExists(ctx, 30*time.Second)
}
