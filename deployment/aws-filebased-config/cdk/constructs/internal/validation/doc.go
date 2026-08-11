// Package validation hosts the tier-B (synth-time) Phase 1 fast-fail
// validators for the AWS file-based-config CDK constructs.
//
// # Two-phase design
//
// The design doc splits synth-time validation in two phases:
//
//   - Phase 1 (this package): cheap, deterministic, fast-fail. Runs
//     against the already-parsed *ports.BridgeConfig (carried by
//     internal/source.Materialized). Returns the FIRST error it
//     encounters as a typed Go error. No CDK Annotations are emitted
//     here — Annotations belong to Phase 2 which surfaces
//     warnings and aggregates non-fatal findings without aborting
//     synth.
//   - Phase 2 (separate package, not implemented here): Annotation-
//     based, aggregated, walks the construct tree to validate
//     references that only exist after the registry has been
//     populated (e.g. SQS queue ARN cross-checks, SSM URI presence).
//
// Phase 1 typed errors let the construct that owns the validation
// surface (`GoBridge{Single,Cluster}`) panic cleanly with a single
// actionable message; tests assert on the typed error via
// errors.Is / errors.As.
//
// # Rows covered
//
// This package implements the Phase 1 rows of the Validation Matrix:
//
//  1. yaml unparseable                       — enforced upstream by
//     config.ParseFile (called by source.Materialize), which wraps the
//     failure in source.ErrYamlParse so the operator-facing message
//     carries the matrix-required "bridge.yaml: " prefix ahead of the
//     yaml line/col detail. If Materialize succeeded, this row is past;
//     Phase 1 here does not re-parse.
//  2. stage-1 validator failure              — same boundary and same
//     source.ErrYamlParse wrap as row 1; bundled into config.ParseFile.
//  3. plaintext credential at field path    — bridgecfg.ScanForPlaintextSecrets.
//  4. filesystem topology + delivery_mode = shared_outbox.
//  5. filesystem topology + route.session lease.
//  6. store path outside EFS mount.
//  7. worker referencing RW-only path on a clustered topology.
//  8. bridge.id (called bridge.name in the matrix) regex.
//  9. bridge.cluster.endpoints malformed URL.
//
// Rows 9 (priority collision), 10 (subnet selection), 11 (multiple
// GoBridge in a stack) and the Phase-2 SQS/SSM URI cross-checks live
// in other constructs (attachment ctor, Efs construct, singleton).
//
// # Validation order (deterministic)
//
// The Phase 1 walker runs validators in this order; cheapest first,
// secret scan last because it walks every plugin payload:
//
//  1. bridge.id regex
//  2. bridge.cluster.endpoints URL parse
//  3. filesystem profile (rows 4 + 5)
//  4. store paths (row 6)
//  5. worker / control-only path (row 7)
//  6. plaintext secret scan (row 3)
//
// # Store-path extraction
//
// Phase 1 cannot statically know every plugin's schema. The store-
// path checks therefore look at StoreConfig.Raw() — the stage-1
// RawConfig retained by the parser — and decode it into
// map[string]any. Any key whose lower-case form is one of "path",
// "file", "filename", "filepath", "db_path", "database_path" is
// treated as a filesystem path. A nested map is walked recursively;
// other shapes are ignored.
//
// When Raw() returns nil (hand-built configs in tests, future code
// paths that bypass the parser), the store-path checks are skipped
// silently. Phase 2 will catch semantic drift via a deeper
// reflection-based pass.
//
// # No CDK / jsii imports
//
// This package intentionally avoids importing aws-cdk-go so it can
// be unit-tested without jsii and without the heavy CDK runtime.
// Construct integration (annotation emission, panic conversion,
// parse-error prefixing) is the caller's responsibility.
package validation
