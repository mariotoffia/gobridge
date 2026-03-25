// Package dynamodblease implements the ports.LeaseStore interface using
// Amazon DynamoDB. It uses conditional writes with fencing tokens to
// enforce single-active lease ownership semantics.
//
// Table: gobridge-leases (configurable)
// Key: PK = "LEASE#<lease_id>"
//
// See ARCHITECTURE_NEW-STORES.md for table schema and operational guidance.
package dynamodblease
