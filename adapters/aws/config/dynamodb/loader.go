package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

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

// streamsAPI is the minimal slice of the dynamodbstreams.Client API the
// loader consumes. It is declared as an interface so that unit tests can
// substitute a fake client without standing up a real DynamoDB Streams
// backend. *dynamodbstreams.Client satisfies this interface structurally.
type streamsAPI interface {
	DescribeStream(ctx context.Context, params *dynamodbstreams.DescribeStreamInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.DescribeStreamOutput, error)
	GetShardIterator(ctx context.Context, params *dynamodbstreams.GetShardIteratorInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetShardIteratorOutput, error)
	GetRecords(ctx context.Context, params *dynamodbstreams.GetRecordsInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetRecordsOutput, error)
}

// ddbAPI is the minimal slice of the dynamodb.Client API the loader
// consumes. It exists so that internal tests can substitute an in-memory
// fake for the whole Load/Save/probe path without requiring DynamoDB
// Local. *dynamodb.Client satisfies it structurally.
type ddbAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

var (
	_ ports.ConfigLoader   = (*Loader)(nil)
	_ ports.ConfigReloader = (*Loader)(nil)
)

// Loader implements ports.ConfigLoader and ports.ConfigReloader using a
// DynamoDB table. The full BridgeConfig is stored as a single JSON item
// with an accompanying numeric version attribute.
//
// Two change-detection modes are supported, selectable via WithWatchMode:
//
//   - ModeStreams (default): consume DynamoDB Streams records on the
//     table for push-based updates. Falls back to ModePoll if streams
//     are not enabled or no streams client is configured.
//   - ModePoll: periodically compare the stored version attribute.
type Loader struct {
	client             ddbAPI
	clientConcrete     *dynamodb.Client
	streams            streamsAPI
	tableName          string
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
	return func(l *Loader) { l.tableName = name }
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

// WithStreamsClient configures the DynamoDB Streams client used by
// ModeStreams. If not set, Watch falls back to ModePoll with a warning.
func WithStreamsClient(c *dynamodbstreams.Client) Option {
	return func(l *Loader) {
		if c != nil {
			l.streams = c
		}
	}
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

// NewLoader creates a DynamoDB-backed ConfigLoader.
func NewLoader(client *dynamodb.Client, opts ...Option) *Loader {
	l := &Loader{
		client:             client,
		clientConcrete:     client,
		tableName:          defaultTableName,
		bridgeID:           defaultBridgeID,
		pollInterval:       defaultPollInterval,
		streamPollInterval: defaultStreamPollInterval,
		mode:               ModeStreams,
		clk:                clock.System,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

func (l *Loader) pk() string { return "config#" + l.bridgeID }

// Load retrieves the current BridgeConfig from DynamoDB.
func (l *Loader) Load(ctx context.Context) (*config.BridgeConfig, error) {
	out, err := l.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &l.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: l.pk()},
			attrSK: &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb config load: %w", err)
	}
	if out.Item == nil {
		return nil, domain.ErrNotFound.WithMessage("config not found for bridge " + l.bridgeID)
	}

	dataAttr, ok := out.Item[attrData].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("dynamodb config load: missing or invalid %q attribute", attrData)
	}

	cfg, err := config.Parse(bytes.NewReader([]byte(dataAttr.Value)), config.FormatJSON)
	if err != nil {
		return nil, fmt.Errorf("dynamodb config load: parse: %w", err)
	}

	if vAttr, ok := out.Item[attrVersion].(*ddbtypes.AttributeValueMemberN); ok {
		if v, err := strconv.ParseInt(vAttr.Value, 10, 64); err == nil {
			l.mu.Lock()
			l.lastVersion = v
			l.mu.Unlock()
		}
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
func (l *Loader) Watch(ctx context.Context) (<-chan *config.BridgeConfig, error) {
	ch := make(chan *config.BridgeConfig, 1)

	effective := l.mode
	if effective == ModeStreams {
		arn, reason := l.resolveStreamArn(ctx)
		switch {
		case reason != "":
			if l.logger != nil {
				l.logger.Warn("dynamodb config loader: falling back to poll mode",
					"reason", reason,
					"table", l.tableName,
				)
			}
			effective = ModePoll
		default:
			go l.streamLoop(ctx, ch, arn)
			return ch, nil
		}
	}

	go l.pollLoop(ctx, ch)
	return ch, nil
}

// resolveStreamArn returns the LatestStreamArn for the configured table
// when streams are enabled and usable. The second return value is a
// human-readable reason when streams cannot be used; when empty, the
// arn is valid.
func (l *Loader) resolveStreamArn(ctx context.Context) (string, string) {
	if l.streams == nil {
		return "", "streams client not configured (use WithStreamsClient)"
	}
	out, err := l.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: &l.tableName,
	})
	if err != nil {
		return "", fmt.Sprintf("describe table: %v", err)
	}
	if out.Table == nil {
		return "", "describe table returned no table"
	}
	spec := out.Table.StreamSpecification
	if spec == nil || spec.StreamEnabled == nil || !*spec.StreamEnabled {
		return "", "stream not enabled on table"
	}
	if out.Table.LatestStreamArn == nil || *out.Table.LatestStreamArn == "" {
		return "", "table has no LatestStreamArn"
	}
	return *out.Table.LatestStreamArn, ""
}

func (l *Loader) pollLoop(ctx context.Context, ch chan<- *config.BridgeConfig) {
	defer close(ch)

	l.mu.Lock()
	lastSeen := l.lastVersion
	l.mu.Unlock()

	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
	out, err := l.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &l.tableName,
		Key: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: l.pk()},
			attrSK: &ddbtypes.AttributeValueMemberS{Value: skCurrent},
		},
		ProjectionExpression: aws.String("#v"),
		ExpressionAttributeNames: map[string]string{
			"#v": attrVersion,
		},
	})
	if err != nil {
		return 0, err
	}
	if out.Item == nil {
		return 0, nil
	}

	vAttr, ok := out.Item[attrVersion].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	return strconv.ParseInt(vAttr.Value, 10, 64)
}

// Save writes a BridgeConfig to DynamoDB, auto-incrementing the version.
// This is useful for tests and admin tooling.
func (l *Loader) Save(ctx context.Context, cfg *config.BridgeConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("dynamodb config save: marshal: %w", err)
	}

	l.mu.Lock()
	newVersion := l.lastVersion + 1
	l.mu.Unlock()

	_, err = l.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &l.tableName,
		Item: map[string]ddbtypes.AttributeValue{
			attrPK:      &ddbtypes.AttributeValueMemberS{Value: l.pk()},
			attrSK:      &ddbtypes.AttributeValueMemberS{Value: skCurrent},
			attrData:    &ddbtypes.AttributeValueMemberS{Value: string(data)},
			attrVersion: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(newVersion, 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("dynamodb config save: %w", err)
	}

	l.mu.Lock()
	l.lastVersion = newVersion
	l.mu.Unlock()

	return nil
}

// EnsureTable creates the DynamoDB table if it does not already exist.
// Intended for test setup and local development.
func (l *Loader) EnsureTable(ctx context.Context) error {
	_, err := l.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: &l.tableName,
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(attrPK), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String(attrSK), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(attrPK), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrSK), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		var inUse *ddbtypes.ResourceInUseException
		if errors.As(err, &inUse) {
			return nil
		}
		return fmt.Errorf("dynamodb config ensure table: %w", err)
	}

	waiterClient := dynamodb.DescribeTableAPIClient(l.client)
	if l.clientConcrete != nil {
		waiterClient = l.clientConcrete
	}
	waiter := dynamodb.NewTableExistsWaiter(waiterClient)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: &l.tableName,
	}, 30*time.Second)
}
