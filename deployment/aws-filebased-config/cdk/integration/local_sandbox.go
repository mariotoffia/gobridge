//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// The local backend of the deployment harness: the same CDK profile, the same
// deploy/outputs/destroy contract, standing on emulators instead of an account.
//
// The emulation split is deliberate and must not be blurred. Every AWS API
// except DynamoDB is served by one emulator; DynamoDB is served by Amazon's own
// DynamoDB Local, because the HA slot and lease design is compare-and-swap end
// to end and an emulator that silently accepted a failing ConditionExpression
// would turn those assertions into a false green. The routing is the SDK's own:
// AWS_ENDPOINT_URL_DYNAMODB outranks AWS_ENDPOINT_URL, and a DynamoDB store
// config cannot carry an endpoint at all, so no bridge code changes.

const (
	localAccount = "000000000000"
	localRegion  = "us-east-1"

	// Hostnames every container in the run answers to on the shared Docker
	// network. A deployed task reaches its AWS endpoint and its broker by these
	// names, which is why one network is created rather than relying on the
	// published loopback ports the test process uses.
	localFlociHost  = "floci"
	localDynamoHost = "ddblocal"
	localBrokerHost = "mosquitto"

	// In-network ports. The published host ports differ per run (every container
	// helper takes a free port), so these are the container-side ones only.
	localFlociPort  = 4566
	localDynamoPort = 8000
	localBrokerPort = 1883

	// proberImage is how the test process reaches a deployed member. The
	// emulator's ECS returns no ENI attachment, and on a Docker-for-Mac host the
	// container network is not routable from the test process, so calls go
	// through a container that IS on the network.
	proberImage  = "curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69"
	proberPrefix = "gobridge-local-prober-"

	// responderImage terminates TLS on port 443 of the emulator's own address.
	// See startCloudFormationResponder for why a deploy cannot complete without
	// it.
	responderImage  = "alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e"
	responderPrefix = "gobridge-local-cfnresponder-"

	// seederDir holds the deployment's seeder image definition, relative to this
	// package. seederImageRepo is what the local run tags its build as.
	seederDir       = "../constructs/internal/seeder"
	seederImageRepo = "gobridge-seeder-local"

	// The task metadata service a clustered member resolves its own advertised
	// address from. See startTaskMetadata.
	metadataHost   = "ecsmeta"
	metadataPort   = 51679
	metadataPrefix = "gobridge-local-ecsmeta-"

	// emulatorRegistryContainer is the image registry the emulator starts for
	// itself. It has a fixed name and a fixed host port, which also means two
	// local deployment runs cannot share a machine.
	emulatorRegistryContainer = "floci-ecr-registry"
)

// localBackend is everything the local run stands on. One per test binary,
// built by the first RequireSandbox and torn down by TestMain.
type localBackend struct {
	network        string
	runDir         string
	configDir      string
	flociEndpoint  string
	dynamoEndpoint string
	brokerURL      string
	prober         string
	responder      string
	metadata       string
	seederPinned   string
	seederLocal    string
	taskSpecs      map[string]localTaskSpec
	env            SandboxEnv
}

var localState *localBackend

func init() {
	localSandboxHook = localSandbox
	cdkBinaryName = "cdklocal"
	postSynthHook = rewriteLocalAssembly
	postDeployHook = afterLocalDeploy
}

// afterLocalDeploy is everything the deployed stack needs before it can be
// driven: the DynamoDB schemas mirrored to where the data plane runs, and the
// task storage the emulator's CloudFormation dropped put back.
func afterLocalDeploy(t *testing.T, outputs StackOutputs) {
	t.Helper()
	mirrorDeployedTables(t, outputs)
	restoreTaskVolumes(t, outputs)
	// Registered AFTER DeployStack registered its destroy, so it runs BEFORE it:
	// the emulator refuses to delete a cluster that still has running tasks, and
	// a failed destroy is logged rather than failed, so it would otherwise be a
	// permanent line of noise across every run.
	t.Cleanup(func() { quiesceServices(t, outputs) })
}

// localSandbox stands the emulators up on one Docker network, points the test
// process at them, bootstraps the CDK environment and returns the synthesized
// sandbox. It is idempotent for the life of the test binary.
//
// Nothing here skips. GOBRIDGE_INT_LOCAL=1 is a request for the proof, so a
// missing prerequisite fails where it can be named rather than passing as a
// skip — the same rule the credentialed branch follows for GOBRIDGE_INT_HA=1.
// runScopedName names a harness container after the run's network, so two runs
// on one machine cannot collide and a sweep by network membership reclaims them.
func runScopedName(prefix, network string) string {
	return prefix + strings.TrimPrefix(network, localRunPrefix)
}

func localSandbox(t *testing.T) SandboxEnv {
	t.Helper()
	if localState != nil {
		return localState.env
	}
	if !dockerexec.DockerAvailable() {
		t.Fatal("GOBRIDGE_INT_LOCAL=1 requested a local deployment proof but Docker is not available")
	}
	if _, err := exec.LookPath(cdkBinaryName); err != nil {
		t.Fatalf("GOBRIDGE_INT_LOCAL=1 requires %q on PATH (run `make test-local-deploy`, which installs it): %v",
			cdkBinaryName, err)
	}

	// A run that crashed — a panic, a test timeout, a Ctrl-C — never reaches
	// localShutdown, and everything it stranded is still attached to ITS network.
	// Reclaiming by network membership takes exactly those and cannot reach
	// another project's containers or another suite's helpers.
	reclaimStrandedRuns()

	// The name carries a timestamp as well as the pid: pids are reused, and a
	// collision here would otherwise fail the run rather than reclaim it.
	state := &localBackend{network: fmt.Sprintf("%s%d-%d", localRunPrefix, os.Getpid(), time.Now().UnixNano())}
	if out, err := dockerexec.Run(dockerexec.RunTimeout, "network", "create", state.network); err != nil {
		t.Fatalf("create docker network %s: %v\n%s", state.network, err, out)
	}
	// Published from here on, so TestMain can tear the run down even if the
	// remaining setup fails.
	localState = state

	// The emulator joins the network first and is given the socket, so the ECS
	// tasks it launches land beside the broker and DynamoDB Local.
	// No orphan sweep on any of the three helpers. Their sweep matches a
	// container name as a SUBSTRING and is unconditional, so enabling it here
	// would force-remove the emulators of any other test binary running on the
	// same machine. reclaimStrandedRuns above already covers this suite's own
	// leftovers, precisely.
	flocilocal.Configure(
		flocilocal.WithServicesNetwork(state.network, localFlociHost),
	)
	state.flociEndpoint = flocilocal.Endpoint(t)

	state.dynamoEndpoint = ddblocal.Endpoint(t)
	attachToNetwork(t, state.network, ddblocal.ContainerName(t), localDynamoHost)

	state.brokerURL = mqttlocal.BrokerURL(t)
	attachToNetwork(t, state.network, mqttlocal.ContainerName(t), localBrokerHost)

	localRunDirectories(t, state)
	state.prober = startProber(t, state.network)
	state.responder = startCloudFormationResponder(t, state)
	buildLocalSeederImage(t, state)
	state.metadata = startTaskMetadata(t, state)

	// The test process talks to the same two endpoints the containers do, by
	// their published loopback ports. cdklocal refuses to start when
	// AWS_ENDPOINT_URL is set without AWS_ENDPOINT_URL_S3, and it inherits this
	// environment, so both are set here rather than in the exec.
	//
	// Deliberately os.Setenv and not t.Setenv: this backend is built once and
	// serves the whole binary, but t.Setenv restores at the end of whichever
	// test happened to build it. A later test would then get the cached backend
	// back with these unset — and a client built from the SDK chain with no
	// endpoint resolves the real AWS, which is precisely what a proof that
	// advertises "no account, no credentials" must never do.
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":         "test",
		"AWS_SECRET_ACCESS_KEY":     "test",
		"AWS_SESSION_TOKEN":         "",
		"AWS_REGION":                localRegion,
		"AWS_DEFAULT_REGION":        localRegion,
		"AWS_ENDPOINT_URL":          state.flociEndpoint,
		"AWS_ENDPOINT_URL_S3":       state.flociEndpoint,
		"AWS_ENDPOINT_URL_DYNAMODB": state.dynamoEndpoint,
	} {
		if err := os.Setenv(name, value); err != nil {
			t.Fatalf("publish %s for the local backend: %v", name, err)
		}
	}

	bootstrapLocalEnvironment(t)
	state.env = localVpc(t)
	return state.env
}

// localShutdown tears down everything localSandbox created. TestMain calls it
// after the last test, because the network can only be removed once every
// container has left it.
func localShutdown() {
	state := localState
	if state == nil {
		return
	}
	// GOBRIDGE_INT_KEEP already tells the harness to leave the deployed stack in
	// place for a post-mortem; leaving the stack while removing everything it
	// runs on would make that useless.
	if state.env.Keep {
		return
	}
	// The emulator goes first: while it is up its service reconciler replaces
	// every task container that is removed.
	flocilocal.Shutdown()
	for _, container := range []string{state.prober, state.responder, state.metadata} {
		if container != "" {
			_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", container)
		}
	}
	mqttlocal.Shutdown()
	ddblocal.Shutdown()
	// The emulator also starts a registry container of its own, off the run's
	// network and on a fixed host port, so neither the network sweep below nor
	// any prefix the helpers own would ever reclaim it.
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", emulatorRegistryContainer)
	if state.network != "" {
		// Everything the emulator launched — ECS task containers, the Lambda
		// containers behind the custom resources — joined this network and is
		// nobody else's to clean up. Removing them by network membership takes
		// exactly this run's containers and cannot reach another run's.
		removeNetworkMembers(state.network)
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", state.network)
	}
	if state.seederLocal != "" {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rmi", "-f", state.seederLocal)
	}
	if state.runDir != "" && strings.Contains(state.runDir, localRunPrefix) {
		_ = os.RemoveAll(state.runDir)
	}
	localState = nil
}

// attachToNetwork joins an already-running helper container to the shared
// network under the name the deployed config refers to it by.
func attachToNetwork(t *testing.T, network, container, alias string) {
	t.Helper()
	if container == "" {
		t.Fatalf("cannot attach %q to %s: the helper is pointing at an externally managed endpoint, "+
			"so there is no container to put on the deployment network", alias, network)
	}
	if out, err := dockerexec.Run(dockerexec.RunTimeout,
		"network", "connect", "--alias", alias, network, container); err != nil {
		t.Fatalf("attach %s to %s as %q: %v\n%s", container, network, alias, err, out)
	}
}

// localRunPrefix names this run: its Docker network, its host directory, its
// harness containers and the seeder image it builds all carry it, so one run's
// artefacts are identifiable as a set and reclaimable as one.
const localRunPrefix = "gobridge-local-deploy-"

// localRunDirectories creates the host directories the containers mount.
//
// They have to be reachable from the Docker daemon as a bind source AND from the
// test process, so they live under the OS temp directory, which Docker shares by
// default on every platform the suite runs on.
func localRunDirectories(t *testing.T, state *localBackend) {
	t.Helper()
	state.runDir = filepath.Join(os.TempDir(), state.network)
	state.configDir = filepath.Join(state.runDir, "config")
	if err := os.MkdirAll(state.configDir, 0o777); err != nil {
		t.Fatalf("create shared config directory: %v", err)
	}
	// The seeder container writes as root and the bridge reads as an unprivileged
	// user; MkdirAll honours the umask, so the mode is set explicitly.
	if err := os.Chmod(state.configDir, 0o777); err != nil {
		t.Fatalf("open shared config directory: %v", err)
	}
}

// bootstrapLocalEnvironment runs `cdklocal bootstrap` once per binary. Without
// it every deploy fails on the missing /cdk-bootstrap/hnb659fds/version
// parameter; the emulator starts empty, so it is never already bootstrapped.
func bootstrapLocalEnvironment(t *testing.T) {
	t.Helper()
	args := []string{"bootstrap", fmt.Sprintf("aws://%s/%s", localAccount, localRegion), "--ci"}
	t.Logf("%s %s", cdkBinaryName, strings.Join(args, " "))
	cmd := exec.Command(cdkBinaryName, args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s bootstrap: %v", cdkBinaryName, err)
	}
}

// localVpc creates the VPC and the two subnets the stacks are placed in, the
// same way the credentialed run consumes a pre-existing sandbox VPC: by
// attributes, so synthesis stays a single source-safe pass with no context
// provider and no lookup.
func localVpc(t *testing.T) SandboxEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := ec2.NewFromConfig(localAWSConfig(t))

	vpc, err := client.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatalf("create local VPC: %v", err)
	}
	vpcID := aws.ToString(vpc.Vpc.VpcId)

	zones := []string{localRegion + "a", localRegion + "b"}
	private := make([]string, 0, len(zones))
	public := make([]string, 0, len(zones))
	for i, zone := range zones {
		private = append(private, createLocalSubnet(t, ctx, client, vpcID, zone, fmt.Sprintf("10.0.%d.0/24", i)))
		public = append(public, createLocalSubnet(t, ctx, client, vpcID, zone, fmt.Sprintf("10.0.%d.0/24", i+len(zones))))
	}
	return SandboxEnv{
		Account: localAccount, Region: localRegion, VpcID: vpcID,
		AvailabilityZones: zones, SubnetIDs: private, PublicSubnetIDs: public,
		StackPrefix: "gobridge-local",
		Keep:        strings.TrimSpace(os.Getenv("GOBRIDGE_INT_KEEP")) == "1",
	}
}

func createLocalSubnet(
	t *testing.T,
	ctx context.Context,
	client *ec2.Client,
	vpcID, zone, cidr string,
) string {
	t.Helper()
	out, err := client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: aws.String(vpcID), AvailabilityZone: aws.String(zone), CidrBlock: aws.String(cidr),
	})
	if err != nil {
		t.Fatalf("create local subnet in %s: %v", zone, err)
	}
	var subnet *ec2types.Subnet = out.Subnet
	return aws.ToString(subnet.SubnetId)
}
