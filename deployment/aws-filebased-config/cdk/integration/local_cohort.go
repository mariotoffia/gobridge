//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// A deployed static member-slot cohort, standing on local emulation.
//
// It is the SAME stack the credentialed proof deploys — built by newHAFixture
// from the same construct, with the same roster cross-check and the same
// outputs. Only what it stands on differs, so a shape that would be rejected on
// AWS is rejected here too.

const (
	localAdminParam = "/gobridge/local/admin-key"
	localMQTTParam  = "/gobridge/local/mqtt-credentials"
	localAdminKey   = "local-deployment-proof-key"
	localImageEnv   = "GOBRIDGE_LOCAL_IMAGE"
	localImage      = "gobridge-filebased:local"

	// bootstrapDocumentVariable is the container environment variable the
	// deployment stamps its bootstrap document into.
	bootstrapDocumentVariable = "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON"
)

// LocalCohort is the deployed cohort and the handles a proof needs to drive it.
type LocalCohort struct {
	LocalStack

	// services maps a member id to the ECS service that runs that slot.
	services map[string]string
}

// DeployLocalCohort deploys the static member-slot HA stack against local
// emulation and returns it ready to drive.
func DeployLocalCohort(t *testing.T, env SandboxEnv, slots *ha.MemberSlots) LocalCohort {
	t.Helper()
	sandbox := haSandbox{
		SandboxEnv:          env,
		Image:               localBridgeImage(),
		BrokerURL:           fmt.Sprintf("tcp://%s:%d", localBrokerHost, localBrokerPort),
		MQTTClientID:        "gobridge-local-ha",
		MQTTCredentialParam: localMQTTParam,
		AdminParam:          localAdminParam,
		// The emulator does not enforce security groups, but the fixture still
		// opens the probe ingress it opens on AWS so the synthesized stack is the
		// same one either way.
		ProbeCIDR:       "10.0.0.0/8",
		Samples:         1,
		PlaintextBroker: true,
	}

	deployed := DeployLocal(t, env, "local-ha-slots", func(stack awscdk.Stack) {
		_ = newHAFixture(t, stack, sandbox, slots)
	})
	if err := missingHAOutput(deployed.Outputs,
		"ClusterArn", "ControlServiceName", "WorkerServiceNames", "MemberSlotIDs", "RolloutTableName"); err != nil {
		t.Fatal(err)
	}
	// The cohort's whole protocol is compare-and-swap against this table, and it
	// runs on DynamoDB Local rather than where CloudFormation created it. A table
	// the mirror missed would surface as a cohort that never converges, which
	// names nothing.
	requireMirroredTable(t, deployed.Outputs["RolloutTableName"])

	cohort := LocalCohort{LocalStack: deployed}
	cohort.services = cohort.mapSlotServices(t, deployed.Outputs, slots)
	return cohort
}

// localBridgeImage is the runtime image the slots run. It has to exist on the
// Docker host: the emulator launches task definitions as real containers, so an
// image that is not there is a task that never starts.
func localBridgeImage() string {
	if override := strings.TrimSpace(os.Getenv(localImageEnv)); override != "" {
		return override
	}
	return localImage
}

// mapSlotServices resolves each member id to the ECS service that runs it, by
// reading the identity out of each service's own task definition.
//
// It deliberately does not parse service names: the physical name is
// CloudFormation's, and matching on it would pass whenever the naming happened
// to look right. Reading the bootstrap document instead asserts the thing the
// static member-slot profile actually claims — that one slot's identity is baked
// into that slot's task definition — before a single task has started.
func (c LocalCohort) mapSlotServices(t *testing.T, outputs StackOutputs, slots *ha.MemberSlots) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	names := []string{outputs["ControlServiceName"]}
	names = append(names, strings.Split(outputs["WorkerServiceNames"], ",")...)
	services := map[string]string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		memberID := c.serviceMemberID(t, ctx, name)
		if memberID == "" {
			t.Fatalf("deployed service %q bakes in no member_id: nothing would give its task a "+
				"restart-stable identity", name)
		}
		if existing, taken := services[memberID]; taken {
			t.Fatalf("services %q and %q both bake in member_id %q: two tasks would share one cohort "+
				"seat", existing, name, memberID)
		}
		services[memberID] = name
	}
	for _, id := range haMemberIDs(slots) {
		if services[id] == "" {
			t.Fatalf("no deployed service bakes in member slot %q; the deployment provisions %v",
				id, services)
		}
	}
	return services
}

// serviceMemberID reads the member id a service's task definition stamps into
// its bootstrap document.
func (c LocalCohort) serviceMemberID(t *testing.T, ctx context.Context, service string) string {
	t.Helper()
	described, err := c.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(c.clusterARN), Services: []string{service},
	})
	if err != nil || len(described.Services) != 1 {
		t.Fatalf("describe service %s: %v", service, err)
	}
	definition, err := c.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: described.Services[0].TaskDefinition,
	})
	if err != nil {
		t.Fatalf("describe task definition of %s: %v", service, err)
	}
	for _, container := range definition.TaskDefinition.ContainerDefinitions {
		for _, pair := range container.Environment {
			if aws.ToString(pair.Name) != bootstrapDocumentVariable {
				continue
			}
			var bootstrap infra.BootstrapConfig
			if err := json.Unmarshal([]byte(aws.ToString(pair.Value)), &bootstrap); err != nil {
				t.Fatalf("parse the bootstrap document of %s: %v", service, err)
			}
			return bootstrap.MemberID
		}
	}
	return ""
}

// seedLocalParameters writes the two secure parameters the deployment reads at
// boot. The stack imports them by name, so they must exist before a member
// starts; the broker allows anonymous connections, so the MQTT document only has
// to be well-formed.
func seedLocalParameters(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := ssm.NewFromConfig(localAWSConfig(t))
	for name, value := range map[string]string{
		localAdminParam: localAdminKey,
		localMQTTParam:  `{"username":"gobridge","password":"gobridge"}`,
	} {
		if _, err := client.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String(name), Value: aws.String(value),
			Type: ssmtypes.ParameterTypeSecureString, Overwrite: aws.Bool(true),
		}); err != nil {
			t.Fatalf("seed parameter %s: %v", name, err)
		}
	}
}

// Probe reaches the cohort the only way a local run can: tasks located through
// the emulator's Docker labels, calls issued from a container on the same
// network. See local_docker.go for why neither can be done directly.
func (c LocalCohort) Probe() cohortProbe {
	return cohortProbe{
		Members: func(ctx context.Context) ([]cohortMember, error) {
			tasks, err := c.runningTasks(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]cohortMember, 0, len(tasks))
			for _, task := range tasks {
				taskID := task.arn[strings.LastIndex(task.arn, "/")+1:]
				for name, container := range taskContainers(taskID) {
					// Every running container of the task is offered; the seeder
					// has exited by then and anything that is not the runtime
					// simply does not answer deep health.
					_ = name
					if host := containerIP(c.backend.network, container); host != "" {
						out = append(out, cohortMember{TaskARN: task.arn, Service: task.service, Host: host})
					}
				}
			}
			return out, nil
		},
		Call: func(
			ctx context.Context,
			method, url string,
			header map[string]string,
			body []byte,
		) (int, []byte, error) {
			return proberCall(ctx, c.backend.prober, method, url, header, body)
		},
	}
}

// MemberHost is the address of the task currently announcing memberID.
func (c LocalCohort) MemberHost(t *testing.T, ctx context.Context, memberID string) string {
	t.Helper()
	host, err := memberHost(ctx, c.Probe(), c.AdminKey, memberID)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// ReplaceMemberTask stops the task currently holding memberID and waits until
// its slot's service has put a new one in its place.
//
// The slot is one single-task service, so the replacement is the same seat
// coming back: its member id belongs to that slot's task definition, not to the
// task that was stopped.
func (c LocalCohort) ReplaceMemberTask(t *testing.T, ctx context.Context, memberID string) {
	t.Helper()
	victim, err := memberTaskARN(ctx, c.Probe(), c.AdminKey, memberID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(c.clusterARN), Task: aws.String(victim),
		Reason: aws.String("local static member-slot proof: restart-stability of member_id"),
	}); err != nil {
		t.Fatalf("stop task for slot %s: %v", memberID, err)
	}
	c.waitServiceTasks(t, ctx, memberID, 1, victim, 5*time.Minute)
}

// ScaleMember sets the desired count of one slot's service, so a proof can take
// a member away and give it back.
func (c LocalCohort) ScaleMember(t *testing.T, ctx context.Context, memberID string, desired int32) {
	t.Helper()
	if _, err := c.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster: aws.String(c.clusterARN), Service: aws.String(c.services[memberID]),
		DesiredCount: aws.Int32(desired),
	}); err != nil {
		t.Fatalf("scale slot %s to %d: %v", memberID, desired, err)
	}
	c.waitServiceTasks(t, ctx, memberID, int(desired), "", 5*time.Minute)
}

// waitServiceTasks waits until one slot's service is running exactly want tasks,
// none of them excluded.
func (c LocalCohort) waitServiceTasks(
	t *testing.T,
	ctx context.Context,
	memberID string,
	want int,
	excluded string,
	timeout time.Duration,
) {
	t.Helper()
	service := c.services[memberID]
	err := pollUntil(ctx, 2*time.Second, timeout, func() (bool, error) {
		listed, err := c.ecs.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster: aws.String(c.clusterARN), ServiceName: aws.String(service),
			DesiredStatus: ecstypes.DesiredStatusRunning,
		})
		if err != nil {
			return false, nil
		}
		for _, arn := range listed.TaskArns {
			if excluded != "" && arn == excluded {
				return false, nil
			}
		}
		return len(listed.TaskArns) == want, nil
	})
	if err != nil {
		t.Fatalf("slot %s service %s did not settle on %d running tasks: %v", memberID, service, want, err)
	}
}

type localTask struct {
	arn     string
	service string
}

func (c LocalCohort) runningTasks(ctx context.Context) ([]localTask, error) {
	listed, err := c.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(c.clusterARN), DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("ListTasks: %w", err)
	}
	if len(listed.TaskArns) == 0 {
		return nil, nil
	}
	described, err := c.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(c.clusterARN), Tasks: listed.TaskArns,
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTasks: %w", err)
	}
	out := make([]localTask, 0, len(described.Tasks))
	for _, task := range described.Tasks {
		if aws.ToString(task.LastStatus) != "RUNNING" {
			continue
		}
		out = append(out, localTask{
			arn:     aws.ToString(task.TaskArn),
			service: strings.TrimPrefix(aws.ToString(task.Group), "service:"),
		})
	}
	return out, nil
}
