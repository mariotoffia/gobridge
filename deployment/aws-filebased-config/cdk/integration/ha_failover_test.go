//go:build integration_aws
// +build integration_aws

package integration

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
)

type haLeaseSnapshot struct {
	owner    string
	version  int64
	endpoint string
}

type haTask struct {
	arn        string
	service    string
	privateIP  string
	lastStatus string
}

type haAWSClients struct {
	ecs        *ecs.Client
	dynamodb   *dynamodb.Client
	cloudwatch *cloudwatch.Client
}

func TestHA_FailoverStopsVerifiedLeaseholder(t *testing.T) {
	env := requireHAFailoverSandbox(t)
	app := NewApp(t, env.SandboxEnv)
	stackName := StackName(env.SandboxEnv, "dynamodb-ha")
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{Env: StackEnv(env.SandboxEnv)})
	_ = newHAFixture(t, stack, env)
	ApplyDestroyAspect(stack)
	outputs := DeployStack(t, app, env.SandboxEnv, stackName)
	if err := missingHAOutput(outputs,
		"ClusterArn", "ControlServiceName", "WorkerServiceName", "LeaseTableName",
		"LeaseID", "MetricsNamespace", "FailoverObjectiveMilliseconds",
	); err != nil {
		t.Fatal(err)
	}

	proofTimeout := 20*time.Minute + time.Duration(env.Samples)*20*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), proofTimeout)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		t.Fatalf("load credentialed AWS config: %v", err)
	}
	clients := haAWSClients{
		ecs:        ecs.NewFromConfig(awsCfg),
		dynamodb:   dynamodb.NewFromConfig(awsCfg),
		cloudwatch: cloudwatch.NewFromConfig(awsCfg),
	}
	objectiveMS, err := strconv.ParseFloat(outputs["FailoverObjectiveMilliseconds"], 64)
	if err != nil || objectiveMS <= 0 {
		t.Fatalf("invalid FailoverObjectiveMilliseconds output %q: %v", outputs["FailoverObjectiveMilliseconds"], err)
	}

	fixture := haRuntimeFixture{
		clients:        clients,
		clusterARN:     outputs["ClusterArn"],
		controlService: outputs["ControlServiceName"],
		workerService:  outputs["WorkerServiceName"],
		leaseTable:     outputs["LeaseTableName"],
		leaseID:        outputs["LeaseID"],
		namespace:      outputs["MetricsNamespace"],
		objectiveMS:    objectiveMS,
	}
	fixture.registerRestoreCleanup(t)
	fixture.restoreFleet(t, ctx)

	warm := make([]time.Duration, 0, env.Samples)
	cold := make([]time.Duration, 0, env.Samples)
	maxWarmAttempts := env.Samples * 3
	for attempt := 0; len(warm) < env.Samples && attempt < maxWarmAttempts; attempt++ {
		t.Logf("warm failover attempt %d (verified samples %d/%d)", attempt+1, len(warm), env.Samples)
		holder := fixture.waitVerifiedHolder(t, ctx, 8*time.Minute)
		warmSnapshot := fixture.snapshotRunningStandbys(t, ctx, holder.task.arn)
		duration, path := fixture.stopVerifiedHolderAndMeasure(t, ctx, holder, warmSnapshot, nil)
		if path == "warm" {
			warm = append(warm, duration)
		} else {
			cold = append(cold, duration)
		}
		fixture.restoreFleet(t, ctx)
	}
	if len(warm) < env.Samples {
		t.Fatalf("collected %d/%d warm samples after %d attempts; replacement winners were correctly classified cold", len(warm), env.Samples, maxWarmAttempts)
	}

	for sample := 0; sample < env.Samples; sample++ {
		t.Logf("cold failover sample %d/%d", sample+1, env.Samples)
		fixture.scaleServices(t, ctx, 0, 1)
		fixture.waitRunningTaskCount(t, ctx, 1, 8*time.Minute)
		coldHolder := fixture.waitVerifiedHolder(t, ctx, 8*time.Minute)
		duration, path := fixture.stopVerifiedHolderAndMeasure(t, ctx, coldHolder, nil, func() {
			fixture.scaleServices(t, ctx, 1, 2)
		})
		if path != "cold" {
			t.Fatalf("replacement-only failover classified as %q, want cold", path)
		}
		cold = append(cold, duration)
		fixture.restoreFleet(t, ctx)
	}

	reportHAPercentiles(t, "warm", warm)
	reportHAPercentiles(t, "cold", cold)
}

type haRuntimeFixture struct {
	clients        haAWSClients
	clusterARN     string
	controlService string
	workerService  string
	leaseTable     string
	leaseID        string
	namespace      string
	objectiveMS    float64
}

func (f haRuntimeFixture) registerRestoreCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, controlErr := f.clients.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(f.clusterARN), Service: aws.String(f.controlService), DesiredCount: aws.Int32(1),
		})
		_, workerErr := f.clients.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(f.clusterARN), Service: aws.String(f.workerService), DesiredCount: aws.Int32(2),
		})
		if controlErr != nil || workerErr != nil {
			t.Logf("restore HA desired counts during cleanup: control=%v worker=%v", controlErr, workerErr)
		}
	})
}

func (f haRuntimeFixture) restoreFleet(t *testing.T, ctx context.Context) {
	t.Helper()
	f.scaleServices(t, ctx, 1, 2)
	f.waitRunningTaskCount(t, ctx, 3, 8*time.Minute)
	_ = f.waitVerifiedHolder(t, ctx, 8*time.Minute)
}

func (f haRuntimeFixture) scaleServices(t *testing.T, ctx context.Context, control, worker int32) {
	t.Helper()
	for _, update := range []struct {
		service string
		desired int32
	}{{f.controlService, control}, {f.workerService, worker}} {
		if _, err := f.clients.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(f.clusterARN), Service: aws.String(update.service), DesiredCount: aws.Int32(update.desired),
		}); err != nil {
			t.Fatalf("UpdateService %s desired=%d: %v", update.service, update.desired, err)
		}
	}
}

func (f haRuntimeFixture) waitRunningTaskCount(t *testing.T, ctx context.Context, want int, timeout time.Duration) {
	t.Helper()
	err := pollUntil(ctx, 5*time.Second, timeout, func() (bool, error) {
		tasks, err := f.runningTasks(ctx)
		if err != nil {
			return false, nil
		}
		t.Logf("HA running tasks=%d want=%d", len(tasks), want)
		return len(tasks) == want, nil
	})
	if err != nil {
		f.logDiagnostics(t, ctx)
		t.Fatalf("running task count did not become %d: %v", want, err)
	}
}

func (f haRuntimeFixture) waitVerifiedHolder(t *testing.T, ctx context.Context, timeout time.Duration) verifiedHolder {
	t.Helper()
	var holder verifiedHolder
	err := pollUntil(ctx, 3*time.Second, timeout, func() (bool, error) {
		lease, err := f.readLease(ctx)
		if err != nil || lease.owner == "" || lease.version <= 0 || lease.endpoint == "" {
			return false, nil
		}
		task, err := f.taskForLeaseEndpoint(ctx, lease.endpoint)
		if err != nil || task.lastStatus != "RUNNING" {
			return false, nil
		}
		if err := exactTaskFull(ctx, lease.endpoint); err != nil {
			t.Logf("leaseholder %s not Full yet: %v", task.arn, err)
			return false, nil
		}
		holder = verifiedHolder{lease: lease, task: task}
		return true, nil
	})
	if err != nil {
		f.logDiagnostics(t, ctx)
		t.Fatalf("could not verify exact leaseholder at ServiceLevelFull: %v", err)
	}
	t.Logf("verified leaseholder owner=%s version=%d task=%s endpoint=%s", holder.lease.owner, holder.lease.version, holder.task.arn, holder.lease.endpoint)
	return holder
}

type verifiedHolder struct {
	lease haLeaseSnapshot
	task  haTask
}

func (f haRuntimeFixture) stopVerifiedHolderAndMeasure(
	t *testing.T, ctx context.Context, holder verifiedHolder, warmSnapshot map[string]struct{}, afterAccepted func(),
) (time.Duration, string) {
	t.Helper()
	current, err := f.readLease(ctx)
	if err != nil {
		t.Fatalf("re-read lease before StopTask: %v", err)
	}
	if current.owner != holder.lease.owner || current.version != holder.lease.version {
		t.Fatalf("verified holder changed before StopTask: old=%+v current=%+v; refusing arbitrary task kill", holder.lease, current)
	}
	mapped, err := f.taskForLeaseEndpoint(ctx, current.endpoint)
	if err != nil || mapped.arn != holder.task.arn {
		t.Fatalf("lease endpoint no longer maps to verified task: mapped=%+v err=%v; refusing arbitrary task kill", mapped, err)
	}

	// Conservative timestamp: include StopTask request latency by sampling just
	// before the request. The task is killed only after the exact lease mapping
	// above is re-verified.
	detectedAt := time.Now()
	reason := "GoBridge Task 11 credentialed exact-leaseholder failover proof"
	stopped, err := f.clients.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(f.clusterARN), Task: aws.String(holder.task.arn), Reason: aws.String(reason),
	})
	if err != nil {
		t.Fatalf("StopTask exact leaseholder %s: %v", holder.task.arn, err)
	}
	if stopped.Task == nil || aws.ToString(stopped.Task.TaskArn) != holder.task.arn {
		t.Fatalf("StopTask did not accept exact verified task: output=%+v", stopped.Task)
	}
	if afterAccepted != nil {
		afterAccepted()
	}
	f.waitTaskStopped(t, ctx, holder.task.arn, 5*time.Minute)

	var successor verifiedHolder
	err = pollUntil(ctx, 2*time.Second, 8*time.Minute, func() (bool, error) {
		lease, readErr := f.readLease(ctx)
		if readErr != nil {
			return false, nil
		}
		if lease.owner == holder.lease.owner || lease.version <= holder.lease.version {
			return false, nil
		}
		task, mapErr := f.taskForLeaseEndpoint(ctx, lease.endpoint)
		if mapErr != nil || task.arn == holder.task.arn || task.lastStatus != "RUNNING" {
			return false, nil
		}
		if fullErr := exactTaskFull(ctx, lease.endpoint); fullErr != nil {
			return false, nil
		}
		successor = verifiedHolder{lease: lease, task: task}
		return true, nil
	})
	if err != nil {
		f.logDiagnostics(t, ctx)
		t.Fatalf("successor did not change owner+fencing version and reach ServiceLevelFull: %v", err)
	}
	duration := time.Since(detectedAt)
	if successor.lease.owner == holder.lease.owner || successor.lease.version <= holder.lease.version || successor.task.arn == holder.task.arn {
		t.Fatalf("invalid successor: prior=%+v successor=%+v", holder, successor)
	}
	path := classifyFailoverPath(successor.task.arn, warmSnapshot)
	t.Logf("verified successor owner=%s version=%d task=%s path=%s failure-to-Full=%s", successor.lease.owner, successor.lease.version, successor.task.arn, path, duration)

	sampleAt := time.Now().UTC()
	f.publishFailureToFull(t, ctx, duration, sampleAt)
	f.requireExactMetricSample(t, ctx, duration, sampleAt)
	if float64(duration.Milliseconds()) > f.objectiveMS {
		t.Fatalf("measured failure-to-Full %s exceeds declared objective %.0fms", duration, f.objectiveMS)
	}
	return duration, path
}

func classifyFailoverPath(successorARN string, warmSnapshot map[string]struct{}) string {
	if _, existedBeforeFailure := warmSnapshot[successorARN]; existedBeforeFailure {
		return "warm"
	}
	return "cold"
}

func (f haRuntimeFixture) snapshotRunningStandbys(t *testing.T, ctx context.Context, holderARN string) map[string]struct{} {
	t.Helper()
	tasks, err := f.runningTasks(ctx)
	if err != nil {
		t.Fatalf("snapshot running standbys: %v", err)
	}
	standbys := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if task.arn != holderARN && task.lastStatus == "RUNNING" {
			standbys[task.arn] = struct{}{}
		}
	}
	if len(standbys) == 0 {
		t.Fatal("warm failover requires at least one running pre-failure standby task")
	}
	return standbys
}

func (f haRuntimeFixture) waitTaskStopped(t *testing.T, ctx context.Context, taskARN string, timeout time.Duration) {
	t.Helper()
	err := pollUntil(ctx, 2*time.Second, timeout, func() (bool, error) {
		out, err := f.clients.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(f.clusterARN), Tasks: []string{taskARN},
		})
		if err != nil || len(out.Tasks) == 0 {
			return false, nil
		}
		return aws.ToString(out.Tasks[0].LastStatus) == "STOPPED", nil
	})
	if err != nil {
		t.Fatalf("exact leaseholder task %s did not reach STOPPED: %v", taskARN, err)
	}
}

func (f haRuntimeFixture) readLease(ctx context.Context) (haLeaseSnapshot, error) {
	out, err := f.clients.dynamodb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(f.leaseTable), ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: "LEASE#" + f.leaseID}},
	})
	if err != nil {
		return haLeaseSnapshot{}, fmt.Errorf("GetItem lease: %w", err)
	}
	owner, ok := out.Item["owner"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return haLeaseSnapshot{}, fmt.Errorf("lease row has no string owner")
	}
	versionValue, ok := out.Item["version"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return haLeaseSnapshot{}, fmt.Errorf("lease row has no numeric version")
	}
	version, err := strconv.ParseInt(versionValue.Value, 10, 64)
	if err != nil {
		return haLeaseSnapshot{}, fmt.Errorf("parse lease version: %w", err)
	}
	endpoint := ""
	if endpoints, ok := out.Item["endpoints"].(*ddbtypes.AttributeValueMemberM); ok {
		if httpEndpoint, ok := endpoints.Value["http"].(*ddbtypes.AttributeValueMemberS); ok {
			endpoint = httpEndpoint.Value
		}
	}
	return haLeaseSnapshot{owner: owner.Value, version: version, endpoint: endpoint}, nil
}

func (f haRuntimeFixture) taskForLeaseEndpoint(ctx context.Context, endpoint string) (haTask, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return haTask{}, fmt.Errorf("parse lease endpoint %q: %w", endpoint, err)
	}
	endpointIPs, err := resolveEndpointIPs(ctx, parsed.Hostname())
	if err != nil {
		return haTask{}, err
	}
	tasks, err := f.runningTasks(ctx)
	if err != nil {
		return haTask{}, err
	}
	var matches []haTask
	for _, task := range tasks {
		if endpointIPs[task.privateIP] {
			matches = append(matches, task)
		}
	}
	if len(matches) != 1 {
		return haTask{}, fmt.Errorf("lease endpoint %q maps to %d running ECS tasks, want exactly one: %+v", endpoint, len(matches), matches)
	}
	return matches[0], nil
}

func resolveEndpointIPs(ctx context.Context, host string) (map[string]bool, error) {
	if parsed := net.ParseIP(host); parsed != nil {
		return map[string]bool{parsed.String(): true}, nil
	}
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		// Fargate private DNS commonly has ip-10-0-1-23 as its first label.
		label := strings.Split(host, ".")[0]
		if strings.HasPrefix(label, "ip-") {
			parts := strings.Split(strings.TrimPrefix(label, "ip-"), "-")
			if len(parts) == 4 {
				candidate := strings.Join(parts, ".")
				if net.ParseIP(candidate) != nil {
					return map[string]bool{candidate: true}, nil
				}
			}
		}
		return nil, fmt.Errorf("resolve private lease endpoint host %q: %w", host, err)
	}
	out := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		if parsed := net.ParseIP(address); parsed != nil {
			out[parsed.String()] = true
		}
	}
	return out, nil
}

func (f haRuntimeFixture) runningTasks(ctx context.Context) ([]haTask, error) {
	listed, err := f.clients.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(f.clusterARN), DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("ListTasks: %w", err)
	}
	if len(listed.TaskArns) == 0 {
		return nil, nil
	}
	described, err := f.clients.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(f.clusterARN), Tasks: listed.TaskArns,
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTasks: %w", err)
	}
	out := make([]haTask, 0, len(described.Tasks))
	for _, task := range described.Tasks {
		service := strings.TrimPrefix(aws.ToString(task.Group), "service:")
		privateIP := ""
		for _, attachment := range task.Attachments {
			for _, detail := range attachment.Details {
				if aws.ToString(detail.Name) == "privateIPv4Address" {
					privateIP = aws.ToString(detail.Value)
				}
			}
		}
		out = append(out, haTask{
			arn: aws.ToString(task.TaskArn), service: service,
			privateIP: privateIP, lastStatus: aws.ToString(task.LastStatus),
		})
	}
	return out, nil
}

func exactTaskFull(ctx context.Context, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	parsed.Scheme = "http"
	parsed.Host = net.JoinHostPort(parsed.Hostname(), "8081")
	parsed.Path = "/api/v1/monitor/ready"
	parsed.RawQuery = "level=full"
	status, body, err := httpGet(ctx, parsed.String())
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("exact-task Full probe status=%d body=%s", status, string(body))
	}
	return nil
}

func (f haRuntimeFixture) publishFailureToFull(t *testing.T, ctx context.Context, duration time.Duration, at time.Time) {
	t.Helper()
	value := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	_, err := f.clients.cloudwatch.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(f.namespace),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String(gobridgealarms.FailureToFullMetricName),
			Timestamp:  aws.Time(at), Value: aws.Float64(value),
			Unit: cwtypes.StandardUnitMilliseconds, StorageResolution: aws.Int32(1),
		}},
	})
	if err != nil {
		t.Fatalf("PutMetricData %s (test principal needs namespace-scoped permission): %v", gobridgealarms.FailureToFullMetricName, err)
	}
}

func (f haRuntimeFixture) requireExactMetricSample(t *testing.T, ctx context.Context, duration time.Duration, at time.Time) {
	t.Helper()
	want := float64(duration.Nanoseconds()) / float64(time.Millisecond)
	err := pollUntil(ctx, 5*time.Second, 3*time.Minute, func() (bool, error) {
		out, err := f.clients.cloudwatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace: aws.String(f.namespace), MetricName: aws.String(gobridgealarms.FailureToFullMetricName),
			StartTime: aws.Time(at.Add(-5 * time.Second)), EndTime: aws.Time(at.Add(10 * time.Second)),
			Period: aws.Int32(1), Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
			Unit: cwtypes.StandardUnitMilliseconds,
		})
		if err != nil {
			return false, fmt.Errorf("GetMetricStatistics %s: %w", gobridgealarms.FailureToFullMetricName, err)
		}
		for _, point := range out.Datapoints {
			if point.Maximum != nil && math.Abs(*point.Maximum-want) < 0.001 {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("CloudWatch did not return the exact externally emitted %s sample %.3fms; missing data must not produce a false release pass: %v", gobridgealarms.FailureToFullMetricName, want, err)
	}
}

func reportHAPercentiles(t *testing.T, path string, samples []time.Duration) {
	t.Helper()
	if len(samples) == 0 {
		t.Fatalf("%s failure-to-Full sample set is empty", path)
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(value int) time.Duration {
		rank := (value*len(sorted) + 99) / 100
		if rank < 1 {
			rank = 1
		}
		return sorted[rank-1]
	}
	t.Logf("HA failure-to-ServiceLevelFull path=%s samples=%d p50=%s p95=%s p99=%s max=%s",
		path, len(sorted), percentile(50), percentile(95), percentile(99), sorted[len(sorted)-1])
}

func (f haRuntimeFixture) logDiagnostics(t *testing.T, ctx context.Context) {
	t.Helper()
	lease, leaseErr := f.readLease(ctx)
	tasks, taskErr := f.runningTasks(ctx)
	services, serviceErr := f.clients.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(f.clusterARN), Services: []string{f.controlService, f.workerService},
	})
	t.Logf("HA diagnostics lease=%+v lease_err=%v tasks=%+v task_err=%v services=%+v service_err=%v",
		lease, leaseErr, tasks, taskErr, services.Services, serviceErr)
}

func TestClassifyFailoverPath_RequiresPreFailureWarmTask(t *testing.T) {
	warmSnapshot := map[string]struct{}{"arn:standby-a": {}, "arn:standby-b": {}}
	if got := classifyFailoverPath("arn:standby-a", warmSnapshot); got != "warm" {
		t.Fatalf("existing standby classified as %q, want warm", got)
	}
	if got := classifyFailoverPath("arn:replacement", warmSnapshot); got != "cold" {
		t.Fatalf("replacement task classified as %q, want cold", got)
	}
}
