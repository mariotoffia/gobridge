package bridgecfg

import (
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/ports"
)

// WithDynamoDBOutbox installs a DynamoDB-backed outbox store under
// tableName. Pass an empty string to fall back to the adapter's
// built-in default table name. Calls replace any previously
// installed outbox — the bridge runtime supports exactly one outbox
// store per instance.
//
// The CDK construct is responsible for creating (or referencing) the
// table; this builder only encodes the table name into bridge.yaml.
// IAM grants are wired separately via grants.GrantDynamoDBStore.
func (b *Builder) WithDynamoDBOutbox(tableName string) *Builder {
	b.cfg.Stores.Outbox = dynamoDBStore(tableName)
	return b
}

// WithDynamoDBLease installs a DynamoDB-backed lease store. Same
// table-name semantics as WithDynamoDBOutbox.
func (b *Builder) WithDynamoDBLease(tableName string) *Builder {
	b.cfg.Stores.Lease = dynamoDBStore(tableName)
	return b
}

// WithDynamoDBDLQ installs a DynamoDB-backed DLQ store. Same
// table-name semantics as WithDynamoDBOutbox.
func (b *Builder) WithDynamoDBDLQ(tableName string) *Builder {
	b.cfg.Stores.DLQ = dynamoDBStore(tableName)
	return b
}

// WithDynamoDBManagedSubscriptions installs the exact durable MQTT
// topic-filter history store. Persistent/exclusive MQTT sessions with desired
// subscriptions require this store before broker activation.
func (b *Builder) WithDynamoDBManagedSubscriptions(tableName string) *Builder {
	b.cfg.Stores.ManagedSubscriptions = dynamoDBStore(tableName)
	return b
}

// dynamoDBStore is the shared assembly path for the four DynamoDB
// store methods. DynamoDBConfig.Validate is a no-op (table_name is
// optional) so the builder cannot fail here.
func dynamoDBStore(tableName string) *ports.StoreConfig {
	sc := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	sc.SetDecoded(&awsstore.DynamoDBConfig{TableName: tableName}, nil)
	return sc
}
