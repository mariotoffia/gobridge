// Package gobridgecdk is the top-level facade of the
// aws-filebased-config CDK profile. It exposes the two
// BridgeConfigSource factories (BridgeYamlAsset and BridgeYamlInline)
// with their sealed BridgeConfigSource return type, and the
// consumer-side cross-stack lookup helpers (LookupBridge / BridgeRef)
// for the GoBridgeALBAttachment SSM-export contract.
//
// This facade deliberately does NOT re-export the constructs. The
// public constructors live in their own sub-packages under
// cdk/constructs/ and are imported directly by consumers:
//
//   - GoBridgeSingle       — cdk/constructs/gobridgesingle
//   - GoBridgeCluster      — cdk/constructs/gobridgecluster
//   - GoBridgeAlarms       — cdk/constructs/gobridgealarms
//   - GoBridgeALBAttachment — cdk/constructs/gobridgealbattachment
//
// Keeping the constructs in sub-packages (rather than aliasing them
// here) avoids an import cycle: the constructs need typed access to
// the internal source contract this facade wraps, so the dependency
// must point from constructs → facade, never the reverse.
//
// # Sealed BridgeConfigSource
//
// BridgeConfigSource is a type alias for the sealed
// internal/source.Source interface. The seal — an unexported
// method on the interface — guarantees that the only values of
// this type are the ones produced by the factories in this
// package. That invariant is what lets the GoBridge{Single,
// Cluster} constructs run a single tier-B validation code path
// regardless of how the operator authored the bridge config: both
// factories converge on the same *Materialized contract (asset
// path on disk + parsed *ports.BridgeConfig + cleanup hook), and
// the validator never branches on the source variant.
//
// # Why an internal sub-package
//
// The concrete Source implementations live under
// cdk/internal/source/ rather than as unexported types in this
// file. The constructs/ sub-tree (GoBridgeSingle / GoBridgeCluster)
// needs typed access to Materialize, and this facade itself imports
// a construct (gobridgealbattachment, via LookupBridge). Housing the
// sealed Source contract in the facade would therefore risk an
// import cycle the moment a construct imported it back. Putting the
// contract in an internal sub-package gives every cdk/... package
// access while keeping it invisible to consumers of the module.
//
// # Deferred bundling
//
// BridgeYamlAsset and BridgeYamlInline are pure value
// constructors: they capture their arguments and do nothing else.
// Materialization (file reads, YAML marshalling, temp-file
// writes, parsing) is deferred until the construct that owns the
// CDK scope calls Materialize at synth time. That keeps prop
// construction side-effect free, localises all I/O to a single
// construct, and lets tests build SingleProps / ClusterProps
// values without touching disk.
//
// # Soft coupling (LookupBridge)
//
// Producer and consumer stacks are deliberately decoupled: the
// producer publishes a small, versioned set of SSM parameters under a
// caller-chosen prefix (see GoBridgeALBAttachment.WithSSMExports),
// and the consumer resolves the same prefix through LookupBridge.
// There is no CloudFormation Export/Import, no stack dependency, and
// no shared CDK construct graph between the two sides — the only
// contract is the SSM parameter set and the manifest-version
// sentinel.
//
// # ValueFromLookup vs FromStringParameterName
//
// The URL / ARN / EFS-id parameters are imported with
// awsssm.StringParameter_FromStringParameterName, which yields a
// deploy-time CloudFormation token. Tokens are opaque at synth time
// — they cannot be string-compared in Go. That is fine for values
// that flow into other resources (target group attributes, IAM
// statements, etc.) but it makes them useless for synth-time schema
// validation.
//
// The manifest-version sentinel is therefore imported with
// awsssm.StringParameter_ValueFromLookup, which performs an actual
// AWS API call during synth and caches the result in
// cdk.context.json. That gives us a real Go string we can compare
// against the gobridgealbattachment.ManifestVersion constant baked
// into the consumer's gobridgecdk module version, and surface a
// clear actionable error via Annotations when the producer is on a
// schema the consumer does not understand.
//
// On the very first synth (before cdk.context.json is populated)
// ValueFromLookup returns a sentinel string of the form
// "dummy-value-for-..." — we tolerate that case so the first synth
// of a fresh checkout can complete and populate the cache.
//
// # Why BridgeRef is concrete
//
// BridgeRef is a plain struct (not an interface) for two reasons:
//
//  1. It is constructed exclusively by LookupBridge, so there is no
//     legitimate alternative implementation a consumer would supply.
//  2. The accessor surface is part of the cross-stack contract; an
//     interface would invite consumer-side fakes that drift from the
//     real producer/consumer wire shape. A concrete struct keeps the
//     contract a single source of truth.
package gobridgecdk
