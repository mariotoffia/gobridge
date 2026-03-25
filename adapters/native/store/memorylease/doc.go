// Package memorylease implements ports.LeaseStore as an in-memory store.
//
// This adapter is suitable only for unit tests and single-process mode.
// It is not safe for clustered production deployments.
package memorylease
