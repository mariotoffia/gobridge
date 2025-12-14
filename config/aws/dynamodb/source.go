package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// Source implements types.ConfigSource and types.ConfigWriter for DynamoDB.
type Source struct {
	config   Config
	client   *dynamodb.Client
	items    map[string]*Item // key format: pk#sk
	mu       sync.RWMutex
	stopCh   chan struct{}
	watchers []chan<- types.ConfigChange
}

// New creates a new DynamoDB config source.
func New(ctx context.Context, tableName string, opts ...Option) (*Source, error) {
	s := &Source{
		config: Config{
			TableName: tableName,
		},
		items:  make(map[string]*Item),
		stopCh: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(s)
	}

	applyDefaults(&s.config)

	// Create DynamoDB client if not provided
	if s.client == nil {
		cfg, err := s.loadAWSConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		s.client = dynamodb.NewFromConfig(cfg)
	}

	return s, nil
}

// Discover scans the DynamoDB table and loads all config items.
func (s *Source) Discover(ctx context.Context) ([]types.ConfigItem, error) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(s.config.TableName),
	}

	// Apply namespace filter if set
	if s.config.Namespace != "" {
		input.FilterExpression = aws.String("begins_with(#pk, :ns)")
		input.ExpressionAttributeNames = map[string]string{
			"#pk": s.config.PartitionKeyName,
		}
		input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":ns": &ddbtypes.AttributeValueMemberS{Value: s.config.Namespace},
		}
	}

	paginator := dynamodb.NewScanPaginator(s.client, input)

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []types.ConfigItem

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to scan DynamoDB table: %w", err)
		}

		for _, item := range page.Items {
			configItem, err := s.parseItem(item)
			if err != nil {
				continue // Skip invalid items
			}
			key := s.itemKey(configItem.pk, configItem.sk)
			s.items[key] = configItem
			result = append(result, configItem)
		}
	}

	return result, nil
}

// Watch starts watching for configuration changes.
func (s *Source) Watch(ctx context.Context) (<-chan types.ConfigChange, error) {
	ch := make(chan types.ConfigChange, 10)

	s.mu.Lock()
	s.watchers = append(s.watchers, ch)
	s.mu.Unlock()

	// Start polling in the background
	go s.pollForChanges(ctx, ch)

	return ch, nil
}

// pollForChanges polls the DynamoDB table for changes.
func (s *Source) pollForChanges(ctx context.Context, ch chan<- types.ConfigChange) {
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(ch)
			return
		case <-s.stopCh:
			close(ch)
			return
		case <-ticker.C:
			s.checkForChanges(ctx, ch)
		}
	}
}

// checkForChanges scans for new/updated/deleted items.
func (s *Source) checkForChanges(ctx context.Context, ch chan<- types.ConfigChange) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(s.config.TableName),
	}

	if s.config.Namespace != "" {
		input.FilterExpression = aws.String("begins_with(#pk, :ns)")
		input.ExpressionAttributeNames = map[string]string{
			"#pk": s.config.PartitionKeyName,
		}
		input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":ns": &ddbtypes.AttributeValueMemberS{Value: s.config.Namespace},
		}
	}

	paginator := dynamodb.NewScanPaginator(s.client, input)

	newItems := make(map[string]*Item)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return // Silently skip failed poll
		}

		for _, item := range page.Items {
			configItem, err := s.parseItem(item)
			if err != nil {
				continue
			}
			key := s.itemKey(configItem.pk, configItem.sk)
			newItems[key] = configItem
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// Detect creates and updates
	for key, newItem := range newItems {
		if oldItem, exists := s.items[key]; exists {
			if newItem.version != oldItem.version || !newItem.updatedAt.Equal(oldItem.updatedAt) {
				s.items[key] = newItem
				s.sendChange(ch, types.ConfigChange{
					Type:      types.ConfigChangeUpdate,
					Item:      newItem,
					Timestamp: now,
				})
			}
		} else {
			s.items[key] = newItem
			s.sendChange(ch, types.ConfigChange{
				Type:      types.ConfigChangeAdd,
				Item:      newItem,
				Timestamp: now,
			})
		}
	}

	// Detect deletes
	for key, oldItem := range s.items {
		if _, exists := newItems[key]; !exists {
			delete(s.items, key)
			s.sendChange(ch, types.ConfigChange{
				Type:      types.ConfigChangeDelete,
				Item:      oldItem,
				Timestamp: now,
			})
		}
	}
}

// sendChange sends a change event to a channel (non-blocking).
func (s *Source) sendChange(ch chan<- types.ConfigChange, change types.ConfigChange) {
	select {
	case ch <- change:
	default:
		// Channel full, skip
	}
}

// Get retrieves a single config item by partition key and sort key.
func (s *Source) Get(ctx context.Context, partitionKey, sortKey string) (types.ConfigItem, error) {
	s.mu.RLock()
	item, exists := s.items[s.itemKey(partitionKey, sortKey)]
	s.mu.RUnlock()

	if exists {
		return item, nil
	}

	// Try to fetch from DynamoDB
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.config.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			s.config.PartitionKeyName: &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			s.config.SortKeyName:      &ddbtypes.AttributeValueMemberS{Value: sortKey},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("item not found: %s/%s", partitionKey, sortKey)
	}

	return s.parseItem(result.Item)
}

// List returns all config items matching the partition key prefix.
func (s *Source) List(ctx context.Context, partitionKeyPrefix string) ([]types.ConfigItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []types.ConfigItem
	for key, item := range s.items {
		if strings.HasPrefix(key, partitionKeyPrefix) {
			result = append(result, item)
		}
	}

	return result, nil
}

// Write writes a config item to DynamoDB with optimistic locking.
func (s *Source) Write(ctx context.Context, item types.ConfigItem) error {
	data := item.GetData()
	dataStr, err := serializeData(data)
	if err != nil {
		return fmt.Errorf("failed to serialize data: %w", err)
	}

	dynamoItem := map[string]ddbtypes.AttributeValue{
		s.config.PartitionKeyName:       &ddbtypes.AttributeValueMemberS{Value: item.GetPartitionKey()},
		s.config.SortKeyName:            &ddbtypes.AttributeValueMemberS{Value: item.GetSortKey()},
		s.config.TypeAttributeName:      &ddbtypes.AttributeValueMemberS{Value: string(item.GetType())},
		s.config.DataAttributeName:      &ddbtypes.AttributeValueMemberS{Value: dataStr},
		s.config.UpdatedAtAttributeName: &ddbtypes.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
	}

	input := &dynamodb.PutItemInput{
		TableName: aws.String(s.config.TableName),
		Item:      dynamoItem,
	}

	version := item.GetVersion()

	// Apply optimistic locking if version > 0
	if version > 0 {
		dynamoItem[s.config.VersionAttributeName] = &ddbtypes.AttributeValueMemberN{
			Value: strconv.FormatInt(version+1, 10),
		}
		input.ConditionExpression = aws.String("#v = :v")
		input.ExpressionAttributeNames = map[string]string{
			"#v": s.config.VersionAttributeName,
		}
		input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":v": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
		}
	} else {
		dynamoItem[s.config.VersionAttributeName] = &ddbtypes.AttributeValueMemberN{Value: "1"}
	}

	_, err = s.client.PutItem(ctx, input)
	if err != nil {
		if isConditionCheckFailed(err) {
			return &ErrVersionConflict{
				PartitionKey: item.GetPartitionKey(),
				SortKey:      item.GetSortKey(),
				Version:      version,
			}
		}
		return fmt.Errorf("failed to write item: %w", err)
	}

	// Update local cache
	s.mu.Lock()
	key := s.itemKey(item.GetPartitionKey(), item.GetSortKey())
	s.items[key] = &Item{
		pk:        item.GetPartitionKey(),
		sk:        item.GetSortKey(),
		itemType:  item.GetType(),
		version:   version + 1,
		data:      data,
		updatedAt: time.Now(),
	}
	s.mu.Unlock()

	return nil
}

// Delete removes a config item from DynamoDB.
func (s *Source) Delete(ctx context.Context, partitionKey, sortKey string, version int64) error {
	input := &dynamodb.DeleteItemInput{
		TableName: aws.String(s.config.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			s.config.PartitionKeyName: &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			s.config.SortKeyName:      &ddbtypes.AttributeValueMemberS{Value: sortKey},
		},
	}

	// Apply optimistic locking if version > 0
	if version > 0 {
		input.ConditionExpression = aws.String("#v = :v")
		input.ExpressionAttributeNames = map[string]string{
			"#v": s.config.VersionAttributeName,
		}
		input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":v": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
		}
	}

	_, err := s.client.DeleteItem(ctx, input)
	if err != nil {
		if isConditionCheckFailed(err) {
			return &ErrVersionConflict{
				PartitionKey: partitionKey,
				SortKey:      sortKey,
				Version:      version,
			}
		}
		return fmt.Errorf("failed to delete item: %w", err)
	}

	// Update local cache
	s.mu.Lock()
	delete(s.items, s.itemKey(partitionKey, sortKey))
	s.mu.Unlock()

	return nil
}

// Close stops watching and releases resources.
func (s *Source) Close() error {
	close(s.stopCh)
	return nil
}

// parseItem parses a DynamoDB item into a ConfigItem.
func (s *Source) parseItem(item map[string]ddbtypes.AttributeValue) (*Item, error) {
	var pk, sk, itemType, dataStr, updatedAtStr string
	var version int64

	if v, ok := item[s.config.PartitionKeyName]; ok {
		attributevalue.Unmarshal(v, &pk)
	}
	if v, ok := item[s.config.SortKeyName]; ok {
		attributevalue.Unmarshal(v, &sk)
	}
	if v, ok := item[s.config.TypeAttributeName]; ok {
		attributevalue.Unmarshal(v, &itemType)
	}
	if v, ok := item[s.config.VersionAttributeName]; ok {
		attributevalue.Unmarshal(v, &version)
	}
	if v, ok := item[s.config.DataAttributeName]; ok {
		attributevalue.Unmarshal(v, &dataStr)
	}
	if v, ok := item[s.config.UpdatedAtAttributeName]; ok {
		attributevalue.Unmarshal(v, &updatedAtStr)
	}

	updatedAt, _ := time.Parse(time.RFC3339, updatedAtStr)

	return &Item{
		pk:        pk,
		sk:        sk,
		itemType:  types.ConfigItemType(itemType),
		version:   version,
		data:      parseData(dataStr),
		updatedAt: updatedAt,
	}, nil
}

// itemKey creates a composite key for the items map.
func (s *Source) itemKey(pk, sk string) string {
	return pk + "#" + sk
}

// loadAWSConfig loads AWS configuration.
func (s *Source) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}

	if s.config.Region != "" {
		opts = append(opts, awsconfig.WithRegion(s.config.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}

	// Apply custom endpoint if provided
	if s.config.Endpoint != "" {
		cfg.BaseEndpoint = aws.String(s.config.Endpoint)
	}

	return cfg, nil
}

// isConditionCheckFailed checks if an error is a condition check failure.
func isConditionCheckFailed(err error) bool {
	// Simple string-based check since errors.As might not work correctly
	return strings.Contains(err.Error(), "ConditionalCheckFailed")
}

// ErrVersionConflict represents an optimistic locking failure.
type ErrVersionConflict struct {
	PartitionKey string
	SortKey      string
	Version      int64
}

func (e *ErrVersionConflict) Error() string {
	return fmt.Sprintf("version conflict for %s/%s at version %d", e.PartitionKey, e.SortKey, e.Version)
}

// serializeData converts data to a JSON string.
func serializeData(data any) (string, error) {
	if data == nil {
		return "{}", nil
	}
	if s, ok := data.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Ensure Source implements types.ConfigSource and types.ConfigWriter
var _ types.ConfigSource = (*Source)(nil)
var _ types.ConfigWriter = (*Source)(nil)
