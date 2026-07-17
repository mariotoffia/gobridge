// Package dynamodbmanagedsubscriptions implements the connectivity-owned
// ManagedSubscriptionStore using one item per secret-safe durable identity.
//
// Table schema: string HASH key `storage_identity`; `baseline` is a BOOL marker
// and `filters` is an optional String Set of exact MQTT topic filters. Reads are
// strongly consistent and updates use atomic ADD/DELETE expressions.
package dynamodbmanagedsubscriptions
