// Package grants holds per-adapter IAM grant derivation helpers used by
// the GoBridge CDK construct facades (T10/T11/T12).
//
// Each helper accepts a CDK [github.com/aws/aws-cdk-go/awscdk/v2/awsiam.IGrantable]
// so callers may pass either an ECS task role, a Lambda role, an IAM
// user or any other principal. The functions delegate to the typed CDK
// grant methods on the resource (queue, parameter, file system, log
// group, key) — they never construct ARNs by hand.
//
// All helpers are idempotent: invoking the same grant on the same
// principal/resource pair more than once does not duplicate IAM policy
// statements because CDK coalesces identical [awsiam.Grant] calls into
// a single statement on the principal's inline policy.
//
// File layout mirrors the adapter kind layout in the bridgecfg package:
// each kind in ports.DefaultRegistry has a matching file here so a CI
// check (T27) can verify drift-free coverage when new kinds are added.
package grants
