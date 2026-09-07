// Package configstoretest provides reusable conformance checks for
// ports.ConfigStore and its optional ports.ConditionalConfigStore capability.
// Each factory call must return an empty store with cleanup registered on t.
// Fixtures use no plugin configuration, so an empty registry is sufficient.
package configstoretest
