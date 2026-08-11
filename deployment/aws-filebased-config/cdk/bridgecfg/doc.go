// Package bridgecfg is the programmatic, in-Go counterpart to the
// hand-edited bridge.yaml. It exposes (in upcoming tasks) a fluent
// builder that emits a *ports.BridgeConfig and (in this task) a
// secret-scanning helper that runs at CDK synth time.
//
// # Mission
//
// The CDK profile in this module composes the deployment artifacts
// (Fargate service, EFS volume, parameter store entries, queues) and
// bakes a bridge.yaml that the runtime container will mount. To keep
// blueprint authoring type-safe and to keep accidental secrets out of
// version control, two complementary pieces live in this package:
//
//   - Builder — a fluent API that produces a
//     *ports.BridgeConfig from typed plugin configs without forcing
//     the operator to write yaml by hand. The builder is the single
//     source of truth that the CDK constructs feed when generating
//     bridge.yaml.
//   - Secret scanner (this task) —
//     ScanForPlaintextSecrets walks the produced *ports.BridgeConfig
//     and refuses to synthesize when any sensitive field carries a
//     literal value rather than a credential URI.
//
// # Secrets policy
//
// Plaintext credentials in bridge.yaml are a hard error at synth.
// There is no opt-out and no "warning" mode: the only way to thread a
// credential through the CDK pipeline is to register an SSM
// parameter (or another supported credential backend) and reference
// it via a credential URI such as pms://<param-path>. The scanner
// emits one error per offending field, naming the dotted JSON-pointer
// path of the field and pointing the operator at the SSM URI
// alternative. Multiple violations are aggregated via errors.Join so
// the operator sees every problem in a single synth pass instead of
// playing whack-a-mole one error at a time.
//
// # Allow-list extensibility
//
// The scanner consults a small allow-list of credential URI schemes
// (currently "pms" for AWS SSM Parameter Store and "file" for the
// native file-backed credential store — the two repositories shipped
// in the gobridge tree today). New credential adapters self-register
// their scheme via RegisterCredentialScheme; the call is idempotent so
// adapter init() functions may register defensively. The list lives
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
