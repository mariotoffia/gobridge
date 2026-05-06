package awsstore

import (
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.PluginConfig = (*DynamoDBConfig)(nil)

// DynamoDBKind is the registry discriminator for DynamoDB-backed
// lease, outbox, and DLQ stores.
const DynamoDBKind = "dynamodb"

// DynamoDBConfig is the typed PluginConfig for DynamoDB-backed
// stores. It is shared across lease/outbox/DLQ since the same
// table-name knob applies to all three roles.
type DynamoDBConfig struct {
	// TableName overrides the default DynamoDB table name. When
	// empty, the underlying store uses its built-in default.
	TableName string `mapstructure:"table_name" yaml:"table_name" json:"table_name"`
}

// Kind reports the registry discriminator.
func (DynamoDBConfig) Kind() string { return DynamoDBKind }

// Validate is a no-op: TableName is optional.
func (DynamoDBConfig) Validate() error { return nil }
