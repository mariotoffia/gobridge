//go:build integration_local
// +build integration_local

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The two adjustments a synthesized stack needs before an emulator can run it.
//
// Both are made here, on the cloud assembly, and never inside the shipped
// constructs: a test-mode branch in a construct would mean the thing under test
// is not the thing that deploys. What the deployment declares is unchanged —
// CloudFormation still provisions the filesystem, the access points and the
// task definitions, so the deploy path stays proven; only the two properties the
// emulator cannot back are rewritten on the way to it.
//
//  1. The shared config filesystem. The emulator's ECS does not honour
//     EFSVolumeConfiguration — a task declaring one starts with no mount at all
//     — but it does honour a host bind mount. So every EFS-backed volume becomes
//     a bind mount of one host directory, which gives the cohort the same thing
//     EFS gives it: one document all members read and the control slot writes.
//
//  2. The endpoint environment. A task definition carries a fixed environment
//     map with no operator escape hatch, and the emulator endpoints are exactly
//     what has to differ locally. The variables are the SDK's own, so the
//     container runs the identical binary through the identical code path —
//     AWS_ENDPOINT_URL_DYNAMODB outranks AWS_ENDPOINT_URL, which is what keeps
//     the compare-and-swap data plane on DynamoDB Local.

const (
	taskDefinitionType = "AWS::ECS::TaskDefinition"
	lambdaFunctionType = "AWS::Lambda::Function"
)

// rewriteLocalAssembly applies both adjustments to the synthesized template.
func rewriteLocalAssembly(t *testing.T, asmDir, stackName string) {
	t.Helper()
	state := localState
	if state == nil {
		t.Fatal("local assembly rewrite ran before the local sandbox was stood up")
	}
	path := filepath.Join(asmDir, stackName+".template.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synthesized template: %v", err)
	}
	var template map[string]any
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatalf("parse synthesized template: %v", err)
	}

	resources, _ := template["Resources"].(map[string]any)
	if len(resources) == 0 {
		t.Fatalf("synthesized template %s declares no resources", path)
	}
	rewritten, substituted := 0, 0
	state.taskSpecs = map[string]localTaskSpec{}
	state.currentConfigDir = stackConfigDir(t, state, stackName)
	containerEnv := localContainerEnvironment()
	functionEnv := localFunctionEnvironment()
	for logicalID, value := range resources {
		resource, ok := value.(map[string]any)
		if !ok {
			continue
		}
		properties, _ := resource["Properties"].(map[string]any)
		switch resource["Type"] {
		case taskDefinitionType:
			if properties == nil {
				t.Fatalf("task definition %s has no properties", logicalID)
			}
			bindVolumesToHost(t, logicalID, properties, state.currentConfigDir)
			addContainerEnvironment(t, logicalID, properties, containerEnv)
			substituted += useLocalSeederImage(properties, state.seederPinned, state.seederLocal)
			family, spec, err := declaredTaskSpec(properties)
			if err != nil {
				t.Fatalf("task definition %s: %v", logicalID, err)
			}
			if len(spec.Volumes) == 0 || len(spec.Mounts) == 0 {
				t.Fatalf("task definition %s declares no shared storage after the rewrite (%d volumes, "+
					"%d mounts): the deployment would have no config document and every member would "+
					"boot the empty default", logicalID, len(spec.Volumes), len(spec.Mounts))
			}
			if _, clash := state.taskSpecs[family]; clash {
				t.Fatalf("two task definitions declare the family %q, so the storage restored after "+
					"deploy could be the wrong one", family)
			}
			state.taskSpecs[family] = spec
			rewritten++
		case lambdaFunctionType:
			// The custom resources CloudFormation runs during the deploy are
			// Lambda functions, and they call AWS too: without this they resolve
			// the public endpoint and the deploy fails on a refused connection.
			if properties != nil {
				addFunctionEnvironment(properties, functionEnv)
			}
		}
	}
	if rewritten == 0 {
		t.Fatalf("synthesized template %s declares no %s: nothing would run", path, taskDefinitionType)
	}
	if substituted != rewritten {
		t.Fatalf("substituted the seeder image on %d of %d task definitions: the rest still name %q, "+
			"which has no canonicalizer and exits before writing a config", substituted, rewritten, state.seederPinned)
	}
	out, err := json.MarshalIndent(template, "", " ")
	if err != nil {
		t.Fatalf("encode rewritten template: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write rewritten template: %v", err)
	}
	t.Logf("local assembly: %d task definitions bound to %s", rewritten, state.currentConfigDir)
}

// localContainerEnvironment is what every container of the deployment's own
// workload needs to reach the emulated services by name on the shared network.
//
// This is where the emulation split lives: the DynamoDB variable outranks the
// general one in the SDK chain, so the bridge's compare-and-swap data plane runs
// against the reference emulator while everything else runs against floci.
func localContainerEnvironment() map[string]string {
	env := localFunctionEnvironment()
	env["AWS_ENDPOINT_URL_DYNAMODB"] = fmt.Sprintf("http://%s:%d", localDynamoHost, localDynamoPort)
	env["AWS_ACCESS_KEY_ID"] = "test"
	env["AWS_SECRET_ACCESS_KEY"] = "test"
	// Without this the SDK spends its connect budget probing an instance
	// metadata service that is not there before falling back to the static
	// credentials above.
	env["AWS_EC2_METADATA_DISABLED"] = "true"
	// A clustered member resolves the address it advertises to its peers from
	// the ECS task metadata service, and refuses to start without one rather
	// than run with cluster forwarding silently disabled. The emulator provides
	// none; startTaskMetadata does.
	env["ECS_CONTAINER_METADATA_URI_V4"] = taskMetadataURI()
	return env
}

// localFunctionEnvironment is what a deploy-time custom resource needs, which is
// deliberately less.
//
// It gets NO DynamoDB override: a custom resource writes during the deploy, to
// the table CloudFormation just created, which exists in the emulator and not
// yet in DynamoDB Local — so pointing it at the data plane would write to a
// table that is not there. The mirror carries what it wrote across afterwards.
// It gets no credentials either: the emulator hands its Lambda containers
// credentials its own IAM recognises, and overriding those would break the
// authorization the deploy is meant to exercise.
func localFunctionEnvironment() map[string]string {
	floci := fmt.Sprintf("http://%s:%d", localFlociHost, localFlociPort)
	return map[string]string{
		"AWS_ENDPOINT_URL":    floci,
		"AWS_ENDPOINT_URL_S3": floci,
		"AWS_REGION":          localRegion,
		"AWS_DEFAULT_REGION":  localRegion,
		// The custom-resource handler reports its result over HTTPS to a listener
		// the harness stands up beside the emulator with a self-signed
		// certificate. That callback carries a status inside one test run and is
		// not a trust boundary; see startCloudFormationResponder.
		"NODE_TLS_REJECT_UNAUTHORIZED": "0",
	}
}

// bindVolumesToHost replaces every EFS-backed volume with a bind mount of dir,
// leaving the volume NAME — and therefore every mount point that refers to it,
// read-only ones included — exactly as synthesized.
func bindVolumesToHost(t *testing.T, logicalID string, properties map[string]any, dir string) {
	t.Helper()
	volumes, ok := properties["Volumes"].([]any)
	if !ok {
		return
	}
	for i, value := range volumes {
		volume, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, isEFS := volume["EFSVolumeConfiguration"]; !isEFS {
			continue
		}
		name, ok := volume["Name"].(string)
		if !ok || name == "" {
			t.Fatalf("task definition %s declares an EFS volume with no name", logicalID)
		}
		volumes[i] = map[string]any{
			"Name": name,
			"Host": map[string]any{"SourcePath": dir},
		}
	}
}

// useLocalSeederImage points the seeder container at the image the local run
// built. See buildLocalSeederImage for why the pinned one cannot run, and for
// what this substitution does and does not prove.
// It returns 1 when it substituted at least one container, so the caller can
// require that every task definition got one rather than discover the miss as an
// unexplained timeout a quarter of an hour later.
func useLocalSeederImage(properties map[string]any, pinned, local string) int {
	if pinned == "" || local == "" {
		return 0
	}
	containers, _ := properties["ContainerDefinitions"].([]any)
	matched := 0
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if image, _ := container["Image"].(string); image == pinned {
			container["Image"] = local
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	return 1
}

// addFunctionEnvironment adds the given variables to a Lambda function, again
// without disturbing any the deployment already set.
func addFunctionEnvironment(properties map[string]any, add map[string]string) {
	env, ok := properties["Environment"].(map[string]any)
	if !ok {
		env = map[string]any{}
		properties["Environment"] = env
	}
	variables, ok := env["Variables"].(map[string]any)
	if !ok {
		variables = map[string]any{}
		env["Variables"] = variables
	}
	for _, name := range sortedKeys(add) {
		if _, present := variables[name]; !present {
			variables[name] = add[name]
		}
	}
}

// addContainerEnvironment adds the given variables to every container, without
// disturbing any the deployment already set: the bootstrap document and the node
// role are the deployment's own and must reach the runtime untouched.
func addContainerEnvironment(t *testing.T, logicalID string, properties map[string]any, add map[string]string) {
	t.Helper()
	containers, ok := properties["ContainerDefinitions"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("task definition %s declares no containers", logicalID)
	}
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}
		existing, _ := container["Environment"].([]any)
		present := map[string]bool{}
		for _, entry := range existing {
			if pair, ok := entry.(map[string]any); ok {
				if name, ok := pair["Name"].(string); ok {
					present[name] = true
				}
			}
		}
		for _, name := range sortedKeys(add) {
			if present[name] {
				continue
			}
			existing = append(existing, map[string]any{"Name": name, "Value": add[name]})
		}
		container["Environment"] = existing
	}
}
