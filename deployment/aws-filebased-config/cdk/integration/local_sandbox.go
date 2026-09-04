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

	// emulatorTaskPrefix names every container the emulator launches for an ECS
	// task. Nothing else on the machine can own one: floci launches task
	// containers only when it is given the Docker socket, and only this harness
	// gives it one.
	emulatorTaskPrefix = "floci-ecs-"
)

// localBackend is everything the local run stands on. One per test binary,
// built by the first RequireSandbox and torn down by TestMain.
type localBackend struct {
	network string
	runDir  string
	// configDir is the run's BASE directory for shared config filesystems;
	// stackConfigDir carves one subdirectory out of it per deployed stack.
	configDir string
	// currentConfigDir is the directory the stack being deployed right now binds
	// its shared config volume to. Every stack gets its own, because the seeder
	// writes the config document ONCE and does not overwrite an existing one: two
	// stacks sharing a directory means the second one's tasks boot the first
	// one's config and address resources that no longer exist.
	currentConfigDir string
	flociEndpoint    string
	dynamoEndpoint   string
	brokerURL        string
	prober           string
	responder        string
	metadata         string
	seederPinned     string
	seederLocal      string
	taskSpecs        map[string]localTaskSpec
	// deployedTaskDefs records, per deployed service, the task definition
	// CloudFormation put it on before the harness rolled it onto a restored one.
	deployedTaskDefs map[string]string
	mirrored         []string
	env              SandboxEnv
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
	localState.mirrored = mirrorDeployedTables(t, outputs)
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

	// Cap the broker at QoS 1. It is what gives the suite a config change every
	// member can ACCEPT and none can RUN: a subscription that asks for QoS 2 is
	// built and validated by every member, and then granted at QoS 1 by the
	// broker, so no member ever reports its subscriptions satisfied and the
	// confirm window takes the whole cohort back. Nothing else here asks for
	// QoS 2, so the cap is invisible to every other topology.
	mqttlocal.Configure(mqttlocal.WithExtraConfig("max_qos 1\n"))
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
		// And by name, for anything that exited and was disconnected before the
		// membership sweep ran — an init container that has finished is the
		// common case, and it would otherwise outlive the network it named.
		removeLaunchedTaskContainers()
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", state.network)
	}
	if state.seederLocal != "" {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rmi", "-f", state.seederLocal)
	}
	if state.runDir != "" && strings.Contains(state.runDir, localRunPrefix) {
		// The per-stack config directories were handed to the container user to
		// match the deployed EFS mount; hand them back before deleting, or on a
		// host where that chown was real the removal fails and the next run
		// inherits them.
		if state.configDir != "" {
			releaseDeployedMountOwnership(state.configDir)
		}
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

// mountOwnerUID and mountOwnerGID are the uid:gid the deployed containers run as,
// and therefore the ownership the shipped EFS access point creates the config
// mount with. See matchDeployedMountOwnership.
const (
	mountOwnerUID = 65532
	mountOwnerGID = 65532
	// mountOwnerImage only has to provide a shell with chown and chmod, and it is
	// already pulled by this suite for the CloudFormation responder. Its own
	// entrypoint is socat, so every use below overrides it.
	mountOwnerImage = "alpine/socat:1.8.0.3"
)

// matchDeployedMountOwnership gives a stack's config directory the ownership and
// mode the shipped EFS access point creates, rather than the blanket 0777 a host
// directory is easiest to create with.
//
// It matters because a SQLite store refuses a parent directory that is group- or
// other-writable, or that the process user does not own — a real security check,
// not a quirk. The access point creates the mount `755` owned by the container
// user, which satisfies it; a 0777 host directory does not, so a deployed task
// carrying a SQLite store could never start against this harness while every
// other topology passed. Matching AWS here is the whole point: a rig that is more
// permissive than production hides exactly the failures it exists to find.
//
// It is done from a container rather than from the test process because the two
// see the mount differently. On a uid-mapping Docker host the chown is a no-op
// against the host inode and the container is shown the file as its own user
// anyway; on a plain Linux host the chown is real and is the only thing that
// makes the container the owner. Doing it from inside covers both without the
// harness having to know which it is on.
func matchDeployedMountOwnership(t *testing.T, dir string) {
	t.Helper()
	if _, err := dockerexec.Run(dockerexec.RunTimeout, "run", "--rm", "--entrypoint", "sh",
		"-v", dir+":/mnt/gobridge", "--user", "0:0", mountOwnerImage,
		"-c", fmt.Sprintf("chown -R %d:%d /mnt/gobridge && chmod 0755 /mnt/gobridge",
			mountOwnerUID, mountOwnerGID)); err != nil {
		t.Fatalf("give %s the ownership the deployed EFS mount has: %v", dir, err)
	}
}

// deployedMountHolds reports whether a file exists at relative under a stack's
// config directory, as a deployed task sees it. It looks from inside a root
// container for the same reason matchDeployedMountOwnership acts from one: a
// store that owns its directory keeps it 0700, so on a plain Linux host the
// test process — neither root nor the container user — cannot look inside it
// from the outside.
func deployedMountHolds(t *testing.T, dir, relative string) bool {
	t.Helper()
	// `test -f` exits 1 for a missing file and the container exits 0 for a
	// present one, so only a NON-1 failure is the harness rather than the
	// answer. Say which, or a pull failure reads as a bridge that wrote nothing.
	out, err := dockerexec.Run(dockerexec.RunTimeout, "run", "--rm", "--entrypoint", "sh",
		"-v", dir+":/mnt/gobridge:ro", "--user", "0:0", mountOwnerImage,
		"-c", "test -f /mnt/gobridge/"+relative+" || exit 1")
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("cannot look inside the deployed mount %s for %s: %v\n%s", dir, relative, err, out)
	}
	return err == nil
}

// releaseDeployedMountOwnership hands a stack's config directory back to whoever
// has to delete it. On a uid-mapping host nothing changed and this is inert; on a
// plain Linux host the directory is now owned by the container user, and the test
// process — which is not root and not that user — could not otherwise remove what
// the deployment wrote into it.
func releaseDeployedMountOwnership(dir string) {
	_, _ = dockerexec.Run(dockerexec.RunTimeout, "run", "--rm", "--entrypoint", "sh",
		"-v", dir+":/mnt/gobridge", "--user", "0:0", mountOwnerImage,
		"-c", "chmod -R 0777 /mnt/gobridge")
}

// stackConfigDir creates and returns the shared config directory one stack's
// tasks bind to.
//
// One per stack, and this is load-bearing. Every task of a stack must see the
// SAME document — that is what the control-writes/workers-read profile is — and
// no task may see a DIFFERENT stack's, because the seeder writes the document
// once and leaves an existing one alone. A shared directory across stacks means
// the second deployment's tasks boot the first deployment's config and address
// queues that were destroyed with it.
func stackConfigDir(t *testing.T, state *localBackend, stackName string) string {
	t.Helper()
	dir := filepath.Join(state.configDir, stackName)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("create the shared config directory of %s: %v", stackName, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("open the shared config directory of %s: %v", stackName, err)
	}
	matchDeployedMountOwnership(t, dir)
	return dir
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
