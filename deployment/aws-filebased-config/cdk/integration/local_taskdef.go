//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Restoring the storage CloudFormation declared and the emulator dropped.
//
// The emulator's CloudFormation maps AWS::ECS::TaskDefinition without its
// Volumes, and without the MountPoints that refer to them: a task definition
// that CloudFormation created comes back from DescribeTaskDefinition with
// `volumes: null` and every container's mount points empty. Deployed that way
// the cohort has no shared config document at all — the seeder writes into its
// own container filesystem and every member boots the empty default config.
//
// So the harness re-registers each deployed task definition with exactly what
// the synthesized template declared for it, and points the service at that
// revision. Nothing is invented here: the volume names, the container paths and
// the read-only flags are read out of the assembly, and the only substitution
// is the one the assembly already made — an EFS filesystem the emulator cannot
// back becomes a host bind mount of one directory.
//
// Container dependencies are NOT restored, because the emulator has no model
// for them: it neither stores `dependsOn` (a definition registered with one
// reads back without it) nor honours it (a gated container starts beside the
// container it is gated on, not after it). So a local member can start before
// its seeder has written the shared document, boot on no config, exit, and be
// replaced by its service until the document is there. The cohort still reaches
// a steady state, which is what the proof is about — but the SEEDER GATE itself
// is not exercised locally, and no claim may rest on it.
//
// What this costs the proof, stated plainly: the running tasks are one revision
// removed from the ones CloudFormation registered, so a local run does NOT
// prove that the declared task definition reaches ECS intact, nor that the
// deployment's start ordering holds. Both rest on the construct's synth
// assertions and the credentialed run. What it does prove is the shared
// document and the cohort protocol that runs on top of it.

// localMountSpec is one container's mount of one volume, as declared.
type localMountSpec struct {
	Container     string
	SourceVolume  string
	ContainerPath string
	ReadOnly      bool
}

// localTaskSpec is the storage one member slot's task definition declared.
type localTaskSpec struct {
	// Volumes names only the volumes the assembly bound to the run's shared
	// directory. A volume of any other kind is NOT restored — binding one to the
	// config directory would put a second writer in the document the cohort
	// reads — and declaredTaskSpec refuses the template rather than guess.
	Volumes []string
	Mounts  []localMountSpec
}

// restoreTaskVolumes re-registers every deployed task definition with the
// volumes and mount points the assembly declared, and rolls each service onto
// the restored revision.
func restoreTaskVolumes(t *testing.T, outputs StackOutputs) {
	t.Helper()
	state := localState
	if state == nil {
		t.Fatal("task definition restore ran before the local sandbox was stood up")
	}
	if state.currentConfigDir == "" {
		t.Fatal("no shared config directory was bound for this stack; the assembly rewrite and the " +
			"storage restore disagree about which deployment is being restored")
	}
	if len(state.taskSpecs) == 0 {
		t.Fatal("the assembly declared no task storage to restore; the rewrite and the restore disagree")
	}
	cluster := outputs["ClusterArn"]
	if cluster == "" {
		t.Fatal("the deployment published no cluster ARN, so its services cannot be addressed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := ecs.NewFromConfig(localAWSConfig(t))

	listed, err := client.ListServices(ctx, &ecs.ListServicesInput{Cluster: aws.String(cluster)})
	if err != nil {
		t.Fatalf("list deployed services: %v", err)
	}
	restored := 0
	restoredFamilies := map[string]struct{}{}
	state.deployedTaskDefs = map[string]string{}
	for _, serviceARN := range listed.ServiceArns {
		described, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster: aws.String(cluster), Services: []string{serviceARN},
		})
		if err != nil || len(described.Services) != 1 {
			t.Fatalf("describe service %s: %v", serviceARN, err)
		}
		definition, err := client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: described.Services[0].TaskDefinition,
		})
		if err != nil {
			t.Fatalf("describe the task definition of %s: %v", serviceARN, err)
		}
		family := aws.ToString(definition.TaskDefinition.Family)
		spec, ok := state.taskSpecs[family]
		if !ok {
			t.Fatalf("deployed task definition belongs to family %q, which the assembly never declared "+
				"(it declared %v)", family, sortedSpecKeys(state.taskSpecs))
		}
		restoredFamilies[family] = struct{}{}
		// Remember what CloudFormation registered before rolling off it. The
		// restore is invisible to CloudFormation, so a proof that asks whether
		// re-deploying the same template is a no-op has to put the service back
		// on the revision the template declares first.
		state.deployedTaskDefs[serviceARN] = aws.ToString(described.Services[0].TaskDefinition)
		revision := registerRestoredTaskDefinition(t, ctx, client, definition.TaskDefinition, spec, state.currentConfigDir)
		if _, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(cluster), Service: aws.String(serviceARN),
			TaskDefinition: aws.String(revision),
		}); err != nil {
			t.Fatalf("roll service %s onto the restored task definition: %v", serviceARN, err)
		}
		restored++
	}
	if len(restoredFamilies) != len(state.taskSpecs) {
		t.Fatalf("restored the storage of %v but the assembly declared it for %v: a task definition "+
			"nothing rolled onto still runs without the shared config document",
			sortedKeySet(restoredFamilies), sortedSpecKeys(state.taskSpecs))
	}
	t.Logf("restored task storage on %d deployed services, bound to %s", restored, state.currentConfigDir)
}

// restoreDeployedTaskDefinitions rolls every service back onto the revision
// CloudFormation registered, undoing the harness's own storage restore.
//
// It exists for one caller: the proof that re-deploying the same assembly
// changes nothing. CloudFormation compares the template against the state it
// recorded at deploy, and the harness moved the services off that state after
// the deploy, so without this the second deploy is answering a question about
// the harness rather than about the profile.
func restoreDeployedTaskDefinitions(t *testing.T, ctx context.Context, cluster string) {
	t.Helper()
	state := localState
	if state == nil || len(state.deployedTaskDefs) == 0 {
		t.Fatal("no deployed task definitions were recorded, so the services cannot be put back on them")
	}
	client := ecs.NewFromConfig(localAWSConfig(t))
	for service, definition := range state.deployedTaskDefs {
		if _, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(cluster), Service: aws.String(service),
			TaskDefinition: aws.String(definition),
		}); err != nil {
			t.Fatalf("roll service %s back onto the task definition CloudFormation registered: %v",
				service, err)
		}
	}
}

// quiesceServices scales every service in the deployed cluster to zero and waits
// for its tasks to go, so the stack can actually be destroyed.
func quiesceServices(t *testing.T, outputs StackOutputs) {
	t.Helper()
	cluster := outputs["ClusterArn"]
	if cluster == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 3*time.Minute)
	defer cancel()
	client := ecs.NewFromConfig(localAWSConfig(t))
	listed, err := client.ListServices(ctx, &ecs.ListServicesInput{Cluster: aws.String(cluster)})
	if err != nil {
		return
	}
	for _, service := range listed.ServiceArns {
		if _, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(cluster), Service: aws.String(service), DesiredCount: aws.Int32(0),
		}); err != nil {
			t.Logf("quiesce %s before destroy: %v", service, err)
		}
	}
	_ = pollUntil(ctx, 2*time.Second, 2*time.Minute, func() (bool, error) {
		tasks, err := client.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster: aws.String(cluster), DesiredStatus: ecstypes.DesiredStatusRunning,
		})
		if err != nil {
			return false, nil
		}
		return len(tasks.TaskArns) == 0, nil
	})
}

// registerRestoredTaskDefinition re-registers definition with the declared
// volumes and mount points, and returns the new revision's ARN.
func registerRestoredTaskDefinition(
	t *testing.T,
	ctx context.Context,
	client *ecs.Client,
	definition *ecstypes.TaskDefinition,
	spec localTaskSpec,
	hostDir string,
) string {
	t.Helper()
	volumes := make([]ecstypes.Volume, 0, len(spec.Volumes))
	for _, name := range spec.Volumes {
		volumes = append(volumes, ecstypes.Volume{
			Name: aws.String(name),
			Host: &ecstypes.HostVolumeProperties{SourcePath: aws.String(hostDir)},
		})
	}
	containers := make([]ecstypes.ContainerDefinition, 0, len(definition.ContainerDefinitions))
	for _, container := range definition.ContainerDefinitions {
		name := aws.ToString(container.Name)
		for _, mount := range spec.Mounts {
			if mount.Container != name {
				continue
			}
			container.MountPoints = append(container.MountPoints, ecstypes.MountPoint{
				SourceVolume:  aws.String(mount.SourceVolume),
				ContainerPath: aws.String(mount.ContainerPath),
				ReadOnly:      aws.Bool(mount.ReadOnly),
			})
		}
		containers = append(containers, container)
	}
	registered, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  definition.Family,
		ContainerDefinitions:    containers,
		Volumes:                 volumes,
		Cpu:                     definition.Cpu,
		Memory:                  definition.Memory,
		NetworkMode:             definition.NetworkMode,
		RequiresCompatibilities: definition.RequiresCompatibilities,
		TaskRoleArn:             definition.TaskRoleArn,
		ExecutionRoleArn:        definition.ExecutionRoleArn,
	})
	if err != nil {
		t.Fatalf("re-register task definition %s: %v", aws.ToString(definition.Family), err)
	}
	// Restoring is only half the job: if the re-registration drops these the way
	// the deploy did, the cohort has no shared document and every symptom points
	// somewhere else.
	if mounted := countMountPoints(registered.TaskDefinition); mounted != len(spec.Mounts) {
		t.Fatalf("the restored task definition for %s mounts the shared config on %d containers but the "+
			"assembly declared %d: the cohort would have no shared document",
			aws.ToString(definition.Family), mounted, len(spec.Mounts))
	}
	return aws.ToString(registered.TaskDefinition.TaskDefinitionArn)
}

// countMountPoints totals the mount points a task definition carries.
func countMountPoints(definition *ecstypes.TaskDefinition) int {
	total := 0
	for _, container := range definition.ContainerDefinitions {
		total += len(container.MountPoints)
	}
	return total
}

// declaredTaskSpec extracts the storage one synthesized task definition declares,
// keyed by the FAMILY it declares.
//
// The family, not the member id: the static member-slot profile is the only
// topology that stamps a member id, and every topology needs its storage put
// back. CDK always emits a family — it derives one from the construct path when
// the operator supplies none — and it is what DescribeTaskDefinition reports
// back for the deployed revision, so it matches the assembly to the deployment
// without depending on a CloudFormation-generated physical name.
func declaredTaskSpec(properties map[string]any) (string, localTaskSpec, error) {
	family, _ := properties["Family"].(string)
	if family == "" {
		return "", localTaskSpec{}, fmt.Errorf(
			"declares no literal Family, so the storage the emulator drops cannot be matched back " +
				"to the deployed revision")
	}
	spec := localTaskSpec{}
	for _, value := range asList(properties["Volumes"]) {
		volume, _ := value.(map[string]any)
		name, _ := volume["Name"].(string)
		if _, bound := volume["Host"]; !bound || name == "" {
			// bindVolumesToHost rewrote every EFS volume to a host bind mount, so
			// anything else here is a volume this harness has no way to back.
			return "", localTaskSpec{}, fmt.Errorf(
				"declares a volume this harness cannot back (%v)", value)
		}
		spec.Volumes = append(spec.Volumes, name)
	}
	for _, value := range asList(properties["ContainerDefinitions"]) {
		container, _ := value.(map[string]any)
		name, _ := container["Name"].(string)
		for _, entry := range asList(container["MountPoints"]) {
			mount, _ := entry.(map[string]any)
			readOnly, _ := mount["ReadOnly"].(bool)
			source, _ := mount["SourceVolume"].(string)
			path, _ := mount["ContainerPath"].(string)
			spec.Mounts = append(spec.Mounts, localMountSpec{
				Container: name, SourceVolume: source, ContainerPath: path, ReadOnly: readOnly,
			})
		}
	}
	return family, spec, nil
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

func sortedKeySet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedSpecKeys(specs map[string]localTaskSpec) []string {
	out := make([]string, 0, len(specs))
	for key := range specs {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
