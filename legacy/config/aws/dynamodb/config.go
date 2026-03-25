package dynamodb

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Config holds the configuration for the DynamoDB config source.
type Config struct {
	// Region is the AWS region to use. If empty, uses default from environment.
	Region string `json:"region,omitempty"`
	// TableName is the DynamoDB table name (required).
	TableName string `json:"tableName"`
	// Namespace is an optional filter. Only items with PartitionKey starting with this
	// namespace will be returned.
	Namespace string `json:"namespace,omitempty"`
	// PartitionKeyName is the name of the partition key attribute. Default: "pk".
	PartitionKeyName string `json:"partitionKeyName,omitempty"`
	// SortKeyName is the name of the sort key attribute. Default: "sk".
	SortKeyName string `json:"sortKeyName,omitempty"`
	// TypeAttributeName is the name of the type attribute. Default: "type".
	TypeAttributeName string `json:"typeAttributeName,omitempty"`
	// VersionAttributeName is the name of the version attribute. Default: "version".
	VersionAttributeName string `json:"versionAttributeName,omitempty"`
	// DataAttributeName is the name of the data attribute. Default: "data".
	DataAttributeName string `json:"dataAttributeName,omitempty"`
	// UpdatedAtAttributeName is the name of the timestamp attribute. Default: "updatedAt".
	UpdatedAtAttributeName string `json:"updatedAtAttributeName,omitempty"`
	// PollInterval is the interval for polling changes. Default: 30 seconds.
	PollInterval time.Duration `json:"pollInterval,omitempty"`
	// Client is an optional pre-configured DynamoDB client.
	Client *dynamodb.Client `json:"-"`
	// Endpoint is an optional custom endpoint (for LocalStack, etc).
	Endpoint string `json:"endpoint,omitempty"`
}

// Option is a functional option for configuring the source.
type Option func(*Source)

// WithRegion sets the AWS region.
func WithRegion(region string) Option {
	return func(s *Source) {
		s.config.Region = region
	}
}

// WithNamespace sets the namespace filter.
func WithNamespace(namespace string) Option {
	return func(s *Source) {
		s.config.Namespace = namespace
	}
}

// WithPollInterval sets the polling interval.
func WithPollInterval(interval time.Duration) Option {
	return func(s *Source) {
		s.config.PollInterval = interval
	}
}

// WithClient sets a pre-configured DynamoDB client.
func WithClient(client *dynamodb.Client) Option {
	return func(s *Source) {
		s.config.Client = client
		s.client = client
	}
}

// WithEndpoint sets a custom endpoint (for LocalStack, etc).
func WithEndpoint(endpoint string) Option {
	return func(s *Source) {
		s.config.Endpoint = endpoint
	}
}

// applyDefaults fills in default values for config.
func applyDefaults(cfg *Config) {
	if cfg.PartitionKeyName == "" {
		cfg.PartitionKeyName = "pk"
	}
	if cfg.SortKeyName == "" {
		cfg.SortKeyName = "sk"
	}
	if cfg.TypeAttributeName == "" {
		cfg.TypeAttributeName = "type"
	}
	if cfg.VersionAttributeName == "" {
		cfg.VersionAttributeName = "version"
	}
	if cfg.DataAttributeName == "" {
		cfg.DataAttributeName = "data"
	}
	if cfg.UpdatedAtAttributeName == "" {
		cfg.UpdatedAtAttributeName = "updatedAt"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
}
