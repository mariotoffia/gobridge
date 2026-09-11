// Package registry exposes name → CDK-handle maps used by the
// gobridge CDK constructs (GoBridgeSingle / GoBridgeCluster).
//
// Two registries live here:
//
//   - QueueRegistry maps logical SQS queue names (matching the `name:`
//     field in bridge.yaml SQS receivers/senders) to awssqs.IQueue
//     handles. Both newly-created queues and imports
//     (Queue.fromQueueArn / fromQueueName) are accepted.
//   - SsmParamRegistry maps logical SSM parameter URIs/paths
//     (matching `pms://<name>` URIs in bridge.yaml credential fields)
//     to awsssm.IParameter handles. Both newly-created parameters and
//     imports (Parameter.fromParameterName) are accepted.
//
// Both registries are populated explicitly by the consumer before
// instantiating a GoBridgeSingle / GoBridgeCluster construct. Phase 2
// of the construct's tier-B validator iterates over the names
// referenced by the parsed BridgeConfig and resolves each one via
// the registry; misses are reported via Annotations.addError so synth
// surfaces every missing entry in a single pass.
//
// # Reference resolution
//
// Each registry exposes a Ref(name) accessor that returns a thin
// value-object (QueueRef / ParamRef) capturing the logical name and
// the underlying CDK handle. Ref always returns a value so it can be
// used inline in builder chains:
//
//	cfg.WithSQSReceiver("orders-in", queueRegistry.Ref("orders-in"))
//
// Resolution failure is deferred — the returned ref reports
// IsResolved() == false when the name was not registered. The Phase 2
// validator inspects Has(name) directly and emits a
// CDK-Annotations error with an actionable message; consumers must
// not panic on missing references at builder-construction time.
//
// # Duplicate registration
//
// AddQueue / AddParameter panic on a duplicate logical name. Synth is
// single-threaded and a duplicate is a programmer error that should
// surface immediately at the offending call site rather than silently
// overwriting a previously-registered handle. The behavior is locked
// by tests in this package.
//
// # Concurrency
//
// The registries are NOT safe for concurrent use. CDK synth is
// single-threaded; callers should not share a registry across
// goroutines.
package registry
