// Package bridgecfg is the programmatic, in-Go counterpart to the
// hand-edited bridge.yaml. It exposes a fluent builder that emits a
// *ports.BridgeConfig and an explicitly callable secret-scanning utility.
//
// # Mission
//
// The CDK profile in this module composes the deployment artifacts
// (Fargate services, optional EFS volumes, parameter store entries, queues)
// and an initial bridge configuration. Two complementary pieces live here:
//
//   - Builder — a fluent API that produces a
//     *ports.BridgeConfig from typed plugin configs without forcing
//     the operator to write yaml by hand. The builder is the single
//     source of truth that the CDK constructs feed when generating
//     bridge.yaml.
//   - Optional secret scanner —
//     ScanForPlaintextSecrets walks the produced *ports.BridgeConfig
//     and reports sensitive fields carrying literal values rather than
//     credential URIs, when a consumer explicitly chooses that policy.
//
// # Secrets policy
//
// Consumers choose literal credentials or credential references. Neither
// Builder.Build nor the CDK Phase1 validator automatically scans or rejects
// literal secret content. This does not disable existing credential/key
// validation or the shared.Secret display-redaction contract.
//
// Consumers wanting a URI-only policy can explicitly invoke
// ScanForPlaintextSecrets after building their config. Its diagnostics name
// field paths without disclosing values and suggest credential URI alternatives.
// Multiple findings are aggregated with errors.Join. The caller decides whether
// to report them or reject its own deployment.
//
// # Allow-list extensibility
//
// The scanner consults a small allow-list of credential URI schemes
// (currently "pms" for AWS SSM Parameter Store and "file" for the
// native file-backed credential store — the two repositories shipped
// in the gobridge tree today). Consumers register additional schemes via
// RegisterCredentialScheme; the call is idempotent. The list lives
// here, next to the scanner, on purpose: scanner correctness depends
// on the same view of "what counts as a credential URI" that the
// runtime credential resolver uses, and co-locating the registration
// API with the scanner makes that coupling visible.
//
// # Sensitive field names
//
// The scanner additionally tracks a curated list of field names that
// are presumed to carry credentials whenever they appear in a plugin
// config payload (password, secret, api_key, client_secret,
// bearer_token, private_key, token, …). The list is exposed for
// inspection (SensitiveFieldNames) and may be extended with
// RegisterSensitiveField; matches are case-insensitive against the
// yaml key name encountered while walking the marshaled plugin
// config.
//
// # Concurrency
//
// The allow-list and sensitive-field registries are guarded by a
// mutex. Synth is single-threaded in practice, but the registries are
// process-globals and adapter init() may run from arbitrary
// goroutines, so the locking is conservative rather than optional.
package bridgecfg
