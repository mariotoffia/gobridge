// Package sqlitemanagedsubscriptions implements the connectivity-owned
// ManagedSubscriptionStore using two minimal tables: a durable identity baseline
// and exact (identity, filter) rows. The baseline survives an empty filter set so
// missing migration history is distinguishable from a deliberately empty set.
package sqlitemanagedsubscriptions
