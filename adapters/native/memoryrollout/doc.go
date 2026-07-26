// Package memoryrollout provides an in-memory ports.ClusterRolloutStore for a
// single process: a single-point coordinator holding one active rollout at a
// time under a mutex, applying the domain's rollout state machine as its
// compare-and-set. It exists for tests, single-node deployments, and as the
// reference implementation the shared conformance suite validates; a
// distributed backend (e.g. DynamoDB) implements the same port for real
// multi-node coordination.
package memoryrollout
