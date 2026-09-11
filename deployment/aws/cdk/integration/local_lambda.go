//go:build integration_local
// +build integration_local

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambdaeventsources"
	awssqs "github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// How a Go function is packaged for this deployment, and how it is deployed.
//
// The packaging is a measured answer, not a convention borrowed from AWS's
// documentation: ONE statically linked binary named `bootstrap`, mode 0755, at
// the root of the asset directory, on the `provided.al2023` runtime with
// `bootstrap` as the handler. CDK stages that directory as an S3 file asset —
// the same mechanism the deploy already uses for CDK's own custom-resource
// handlers — and the emulator's CloudFormation creates the function from it.
// Nothing about it is local-only: it is exactly what a credentialed deploy of
// the same stack does.
//
// The one thing a local run must NOT do is package for the test process's own
// platform. The function runs in a container the emulator launches on the
// Docker daemon, so the daemon's architecture is what the binary has to match;
// on a mismatch the runtime answers with an `exec format error` from inside a
// container nobody is watching.

// buildLambdaFunctionAsset builds the handler for the architecture the launched
// container will run on, and returns the asset directory and the matching CDK
// architecture.
//
// One directory serves every function of a topology: both ends run the same
// binary and differ only by environment, so CDK hashes one asset and publishes
// it once.
func buildLambdaFunctionAsset(t *testing.T) (string, awslambda.Architecture) {
	t.Helper()
	if localState == nil {
		t.Fatal("the function asset needs the local sandbox's run directory; call RequireSandbox first")
	}
	goarch, arch := lambdaTargetArchitecture(t)
	dir := filepath.Join(localState.runDir, "lambda-asset")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the function asset directory: %v", err)
	}
	// `bootstrap` is the file name the provided runtimes execute; the .out
	// convention that keeps built binaries out of git does not apply, because
	// this one is written outside the repository under the run's temp directory.
	cmd := exec.Command("go", "build", "-trimpath", "-tags=integration_local",
		"-o", filepath.Join(dir, "bootstrap"), "./lambdafn")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the deployed function for linux/%s: %v\n%s", goarch, err, out)
	}
	return dir, arch
}

// lambdaTargetArchitecture reports the architecture of the Docker daemon, which
// is what the launched function container runs on.
//
// It is read rather than taken from the test process, because the two are only
// the same machine by convention — a daemon emulating a foreign architecture
// would accept the deploy and then fail every invocation with an exec format
// error, which is the least diagnosable failure this topology can have.
func lambdaTargetArchitecture(t *testing.T) (string, awslambda.Architecture) {
	t.Helper()
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "info", "--format", "{{.Architecture}}")
	if err != nil {
		t.Fatalf("read the Docker daemon architecture: %v\n%s", err, out)
	}
	switch reported := strings.TrimSpace(string(out)); reported {
	case "aarch64", "arm64":
		return "arm64", awslambda.Architecture_ARM_64()
	case "x86_64", "amd64":
		return "amd64", awslambda.Architecture_X86_64()
	default:
		t.Fatalf("the Docker daemon reports the architecture %q, which this topology has no Lambda "+
			"architecture for", reported)
		return "", nil
	}
}

// localLambdaFunction declares one function of the topology.
//
// The target queue is passed by NAME and resolved inside the function. Handing
// it a URL would work on a credentialed account and fail here for a reason that
// has nothing to do with the deployment: every URL the emulator returns names
// its gateway host, which a launched container does not necessarily reach under
// that name.
func localLambdaFunction(
	stack awscdk.Stack,
	id, assetDir, role string,
	arch awslambda.Architecture,
	target awssqs.Queue,
) awslambda.Function {
	fn := awslambda.NewFunction(stack, jsii.String(id), &awslambda.FunctionProps{
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Handler:      jsii.String("bootstrap"),
		Architecture: arch,
		Code:         awslambda.Code_FromAsset(jsii.String(assetDir), nil),
		// A sixth of the source queue's 30s visibility timeout, which is the
		// ratio AWS asks for when a queue drives a function: at parity a slow
		// invocation and the queue's redelivery clock expire together and the
		// same message is handed to a second invocation. Two SQS calls have no
		// use for more, and the producer is invoked synchronously, so a function
		// that somehow needed longer says so in the test rather than silently
		// producing the duplicate the loop asserts against.
		Timeout: awscdk.Duration_Seconds(jsii.Number(5)),
		Environment: &map[string]*string{
			"GOBRIDGE_FN_ROLE":         jsii.String(role),
			"GOBRIDGE_FN_TARGET_QUEUE": target.QueueName(),
		},
	})
	target.GrantSendMessages(fn)
	return fn
}

// localLambdaSQSTrigger drives fn from source through an event source mapping.
//
// The batch size is one deliberately. The emulator stores `ScalingConfig` and
// does not enforce it, so nothing here may rest on concurrency; a batch of one
// keeps every assertion about what arrived a statement about delivery rather
// than about how the poller grouped it.
func localLambdaSQSTrigger(fn awslambda.Function, source awssqs.Queue) {
	fn.AddEventSource(awslambdaeventsources.NewSqsEventSource(source,
		&awslambdaeventsources.SqsEventSourceProps{
			BatchSize: jsii.Number(1),
			Enabled:   jsii.Bool(true),
		}))
}
