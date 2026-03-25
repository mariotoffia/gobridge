package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	defaultTableName    = "gobridge-config"
	defaultBridgeID     = "default"
	defaultPollInterval = 30 * time.Second

	attrPK      = "PK"
	attrSK      = "SK"
	attrData    = "data"
	attrVersion = "version"

	skCurrent = "current"
)

var (
	_ ports.ConfigLoader   = (*Loader)(nil)
	_ ports.ConfigReloader = (*Loader)(nil)
)

// Loader implements ports.ConfigLoader and ports.ConfigReloader using a
// DynamoDB table. The full BridgeConfig is stored as a single JSON item
// with poll-based version change detection.
type Loader struct {
	client       *dynamodb.Client
	tableName    string
	bridgeID     string
	pollInterval time.Duration

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

// WithPollInterval sets the interval for Watch polling (default: 30s).
func WithPollInterval(d time.Duration) Option {
	return func(l *Loader) { l.pollInterval = d }
}

// NewLoader creates a DynamoDB-backed ConfigLoader.
func NewLoader(client *dynamodb.Client, opts ...Option) *Loader {
	l := &Loader{
		client:       client,
		tableName:    defaultTableName,
		bridgeID:     defaultBridgeID,
		pollInterval: defaultPollInterval,
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

// Watch polls DynamoDB for version changes and emits updated configs on the
// returned channel. The channel is closed when ctx is cancelled. The initial
// config is NOT emitted; call Load separately for the first load.
func (l *Loader) Watch(ctx context.Context) (<-chan *config.BridgeConfig, error) {
	ch := make(chan *config.BridgeConfig, 1)
	go l.pollLoop(ctx, ch)
	return ch, nil
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

	waiter := dynamodb.NewTableExistsWaiter(l.client)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: &l.tableName,
	}, 30*time.Second)
}
