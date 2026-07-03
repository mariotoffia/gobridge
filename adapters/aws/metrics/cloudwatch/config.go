package cloudwatch

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Config holds the configuration for the CloudWatch metrics exporter.
type Config struct {
	Region          string        `json:"region,omitempty"`
	Namespace       string        `json:"namespace"`
	DefaultTags     []shared.Tag  `json:"defaultTags,omitempty"`
	FlushInterval   time.Duration `json:"flushInterval,omitempty"`
	FlushRPCTimeout time.Duration `json:"flushRPCTimeout,omitempty"`
	// BufferSize is the flush-trigger threshold: when this many pending
	// samples/series accumulate, the flusher goroutine is signalled.
	// Default: 1000.
	BufferSize int `json:"bufferSize,omitempty"`
	// MaxBatchSize bounds datums per PutMetricData call. Clamped to the
	// CloudWatch API hard limit of 1,000. Default: 1000.
	MaxBatchSize int `json:"maxBatchSize,omitempty"`
	// MaxBatchBytes bounds the estimated payload per PutMetricData call.
	// Clamped to the CloudWatch API hard limit of 1 MB. Default: 900 KB
	// (safety margin under the 1 MB limit).
	MaxBatchBytes int `json:"maxBatchBytes,omitempty"`
	// MaxBufferedDatums is the HARD cap on pending in-memory state
	// (gauge samples + distinct counter/histogram series). When a slow
	// CloudWatch endpoint stalls flushing, new samples/series beyond the
	// cap are dropped and counted (MF-1) instead of growing process
	// memory without bound. Default: 10000.
	MaxBufferedDatums int `json:"maxBufferedDatums,omitempty"`
	// MaxRetryDatums bounds how many metric datums a failed PutMetricData is
	// allowed to requeue for the next flush. Beyond this the oldest requeued
	// datums are dropped (and counted) so a persistently failing CloudWatch
	// endpoint cannot grow memory without bound. Default: 10000.
	MaxRetryDatums int `json:"maxRetryDatums,omitempty"`
	// RollupMetrics lists metric names that are double-published WITHOUT
	// any dimensions (a zero-dimension fleet rollup copy) in addition to
	// their normal dimensioned emission (MF-4). CloudWatch alarms on a
	// metric without dimensions never match dimensioned data, so
	// DefaultAlarms only fire when the metrics they target are listed
	// here — see DefaultRollupMetrics.
	RollupMetrics []string `json:"rollupMetrics,omitempty"`
	// InstanceID, when set, is added to DefaultTags as the
	// "instance_id" dimension so per-instance series in a fleet do not
	// collide (MF-8). Set via WithInstanceTag. Rollup copies never
	// carry it (they aggregate the fleet by design).
	InstanceID string       `json:"instanceId,omitempty"`
	Endpoint   string       `json:"endpoint,omitempty"`
	Clock      clock.Clock  `json:"-"`
	Logger     *slog.Logger `json:"-"`
}

// Option is a functional option for configuring the exporter.
type Option func(*Exporter)

// WithRegion sets the AWS region.
func WithRegion(region string) Option {
	return func(e *Exporter) { e.config.Region = region }
}

// WithNamespace overrides the CloudWatch metric namespace.
func WithNamespace(namespace string) Option {
	return func(e *Exporter) { e.config.Namespace = namespace }
}

// WithDefaultTags sets the default tags added to all metrics as dimensions.
func WithDefaultTags(tags ...shared.Tag) Option {
	return func(e *Exporter) { e.config.DefaultTags = tags }
}

// WithFlushInterval sets how often buffered metrics are flushed. Default: 60s.
func WithFlushInterval(interval time.Duration) Option {
	return func(e *Exporter) { e.config.FlushInterval = interval }
}

// WithBufferSize sets the number of pending metrics that triggers an
// early flush by the background flusher goroutine. Default: 1000.
func WithBufferSize(size int) Option {
	return func(e *Exporter) { e.config.BufferSize = size }
}

// WithMaxBatchSize bounds datums per PutMetricData call. Values above
// the CloudWatch API hard limit (1,000) are clamped. Default: 1000.
func WithMaxBatchSize(n int) Option {
	return func(e *Exporter) { e.config.MaxBatchSize = n }
}

// WithMaxBufferedDatums sets the HARD cap on pending in-memory metric
// state; beyond it new samples/series are dropped and counted (MF-1).
// Default: 10000.
func WithMaxBufferedDatums(n int) Option {
	return func(e *Exporter) { e.config.MaxBufferedDatums = n }
}

// WithRollupMetrics double-publishes the listed metric names WITHOUT
// dimensions (zero-dimension fleet rollup) in addition to the normal
// dimensioned emission (MF-4). Required for DefaultAlarms to fire; see
// DefaultRollupMetrics for the canonical list.
func WithRollupMetrics(names ...string) Option {
	return func(e *Exporter) { e.config.RollupMetrics = append(e.config.RollupMetrics, names...) }
}

// WithInstanceTag adds an "instance_id" dimension (TagKeyInstanceID) to
// every dimensioned metric so per-instance series in a fleet do not
// collide (MF-8). Pass the bridge's configured InstanceID
// (ports.BridgeSettings.InstanceID); an empty id derives
// "<hostname>-<pid>". Rollup copies never carry the tag.
func WithInstanceTag(id string) Option {
	return func(e *Exporter) {
		if id == "" {
			id = deriveInstanceID()
		}
		e.config.InstanceID = id
	}
}

// deriveInstanceID builds a best-effort instance identity from
// hostname and pid, for callers that do not configure an explicit
// ports.BridgeSettings.InstanceID.
func deriveInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// WithClient sets a pre-configured CloudWatch client, bypassing automatic
// credential resolution. Useful for testing with LocalStack or mocks.
func WithClient(client cloudWatchAPI) Option {
	return func(e *Exporter) {
		e.client = client
	}
}

// WithFlushRPCTimeout sets the per-RPC timeout used when flushing
// metrics to CloudWatch. Defaults to FlushInterval / 2.
func WithFlushRPCTimeout(d time.Duration) Option {
	return func(e *Exporter) { e.config.FlushRPCTimeout = d }
}

// WithClock overrides the clock used for flush tickers and timeouts.
// Primarily intended for tests; production code should rely on the
// default clock.System.
func WithClock(c clock.Clock) Option {
	return func(e *Exporter) {
		if c != nil {
			e.config.Clock = c
		}
	}
}

// WithEndpoint sets a custom endpoint URL (e.g. for LocalStack).
func WithEndpoint(endpoint string) Option {
	return func(e *Exporter) { e.config.Endpoint = endpoint }
}

// WithLogger sets the structured logger used to warn about dropped/requeued
// metrics, rejected values, and invalid dimensions. Defaults to
// slog.Default() so self-loss is never silent (MF-5); pass nil to
// explicitly suppress diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(e *Exporter) {
		e.config.Logger = l
		e.loggerSet = true
	}
}

// WithMaxRetryDatums bounds how many datums a failed PutMetricData may
// requeue for the next flush before the oldest are dropped. Default: 10000.
func WithMaxRetryDatums(n int) Option {
	return func(e *Exporter) { e.config.MaxRetryDatums = n }
}

func applyDefaults(cfg *Config) {
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 60 * time.Second
	}
	if cfg.FlushRPCTimeout <= 0 {
		cfg.FlushRPCTimeout = min(cfg.FlushInterval/2, 30*time.Second)
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = 1000
	}
	if cfg.MaxBatchSize <= 0 || cfg.MaxBatchSize > apiMaxBatchDatums {
		// The CloudWatch PutMetricData limit has been 1,000 datums /
		// 1 MB since 2022 (MF-6); the historical default of 20 caused
		// 50x the necessary API calls.
		cfg.MaxBatchSize = apiMaxBatchDatums
	}
	if cfg.MaxBatchBytes <= 0 || cfg.MaxBatchBytes > apiMaxBatchBytes {
		cfg.MaxBatchBytes = 900_000
	}
	if cfg.MaxBufferedDatums <= 0 {
		cfg.MaxBufferedDatums = 10000
	}
	if cfg.MaxRetryDatums <= 0 {
		// Zero is the unset default; a negative value must not be interpreted
		// as "disable the bound" (the requeue guard is maxRetry > 0), which
		// would let the retry buffer grow without limit (N5).
		cfg.MaxRetryDatums = 10000
	}
	if cfg.InstanceID != "" && !hasTagKey(cfg.DefaultTags, TagKeyInstanceID) {
		cfg.DefaultTags = append(cfg.DefaultTags, shared.Tag{Key: TagKeyInstanceID, Value: cfg.InstanceID})
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
}

func hasTagKey(tags []shared.Tag, key string) bool {
	for _, t := range tags {
		if t.Key == key {
			return true
		}
	}
	return false
}
