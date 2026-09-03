# Production stack — complete CDK stack

The following stack assembles all production components:

```go
package main

import (
    "github.com/aws/aws-cdk-go/awscdk/v2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
    "github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatchactions"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
    "github.com/aws/aws-cdk-go/awscdk/v2/awskms"
    "github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
    "github.com/aws/aws-cdk-go/awscdk/v2/awssns"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsxray"
    "github.com/aws/constructs-go/constructs/v10"
    "github.com/aws/jsii-runtime-go"

    gobridgecluster "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func NewProductionStack(scope constructs.Construct, id string, props *awscdk.StackProps) awscdk.Stack {
    stack := awscdk.NewStack(scope, &id, props)

    // --- Networking (no NAT -- VPC endpoints instead) ---
    vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{
        MaxAzs: jsii.Number(2), NatGateways: jsii.Number(0),
    })
    for _, ep := range []struct {
        ID  string
        Svc awsec2.InterfaceVpcEndpointAwsService
    }{
        {"SSM", awsec2.InterfaceVpcEndpointAwsService_SSM()},
        {"SQS", awsec2.InterfaceVpcEndpointAwsService_SQS()},
        {"ECR", awsec2.InterfaceVpcEndpointAwsService_ECR()},
        {"ECRDocker", awsec2.InterfaceVpcEndpointAwsService_ECR_DOCKER()},
        {"CWLogs", awsec2.InterfaceVpcEndpointAwsService_CLOUDWATCH_LOGS()},
        {"CWMetrics", awsec2.InterfaceVpcEndpointAwsService_CLOUDWATCH()},
    } {
        vpc.AddInterfaceEndpoint(jsii.String(ep.ID),
            &awsec2.InterfaceVpcEndpointOptions{
                Service: ep.Svc, PrivateDnsEnabled: jsii.Bool(true),
            },
        )
    }
    vpc.AddGatewayEndpoint(jsii.String("S3"), &awsec2.GatewayVpcEndpointOptions{
        Service: awsec2.GatewayVpcEndpointAwsService_S3(),
    })

    // --- KMS for SSM SecureString ---
    kmsKey := awskms.NewKey(stack, jsii.String("Key"), &awskms.KeyProps{
        Description:       jsii.String("GoBridge SSM encryption"),
        EnableKeyRotation: jsii.Bool(true),
    })

    // --- GoBridge cluster (control + autoscaled workers, EFS, log retention built in) ---
    workers := float64(2)
    src := gobridgecdk.BridgeYamlAsset("bridge.yaml")
    bridge := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"),
        &gobridgecluster.ClusterProps{
            Vpc: vpc,
            Image: awsecs.ContainerImage_FromRegistry(
                jsii.String("123456789012.dkr.ecr.eu-west-1.amazonaws.com/gobridge:latest"), nil,
            ),
            Bootstrap: infra.BootstrapConfig{
                BridgeID: "gobridge-prod", ConfigFilePath: "/var/lib/gobridge/bridge.yaml",
                PollInterval: "5s", AdminAPIKeyParam: "/gobridge/prod/admin-api-key",
                MonitorAPIKeyParam: "/gobridge/prod/monitor-api-key",
                Topology: infra.TopologyFilesystemReplicated,
                // Publish runtime metrics to CloudWatch (grants PutMetricData
                // scoped to the namespace).
                MetricsExporter: "cloudwatch",
            },
            BridgeConfig:       src,
            CPU:                jsii.Number(512),
            MemoryMiB:          jsii.Number(1024),
            WorkerDesiredCount: &workers,
            AutoScaling: &gobridgecluster.AutoScalingProps{
                Min: 2, Max: 8, TargetCPU: 70,
            },
            LogRetention: awslogs.RetentionDays_ONE_MONTH,
        },
    )

    // --- IAM: KMS decrypt + X-Ray on BOTH task roles ---
    for _, td := range []awsecs.FargateTaskDefinition{
        bridge.ControlTaskDefinition(), bridge.WorkerTaskDefinition(),
    } {
        td.TaskRole().AddToPrincipalPolicy(
            awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
                Actions:   &[]*string{jsii.String("kms:Decrypt")},
                Resources: &[]*string{kmsKey.KeyArn()},
            }),
        )
        td.TaskRole().AddToPrincipalPolicy(
            awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
                Actions: &[]*string{
                    jsii.String("xray:PutTraceSegments"), jsii.String("xray:PutTelemetryRecords"),
                    jsii.String("xray:GetSamplingRules"), jsii.String("xray:GetSamplingTargets"),
                },
                Resources: &[]*string{jsii.String("*")},
            }),
        )
    }

    // --- X-Ray sampling rule ---
    awsxray.NewCfnSamplingRule(stack, jsii.String("Sampling"),
        &awsxray.CfnSamplingRuleProps{
            SamplingRule: &awsxray.CfnSamplingRule_SamplingRuleProperty{
                RuleName: jsii.String("GoBridge-Prod"), Priority: jsii.Number(100),
                FixedRate: jsii.Number(0.1), ReservoirSize: jsii.Number(5),
                ServiceName: jsii.String("gobridge"), ServiceType: jsii.String("*"),
                Host: jsii.String("*"), HttpMethod: jsii.String("*"),
                UrlPath: jsii.String("*"), ResourceArn: jsii.String("*"),
            },
        },
    )

    // --- Alarms + SNS ---
    topic := awssns.NewTopic(stack, jsii.String("Alerts"),
        &awssns.TopicProps{TopicName: jsii.String("gobridge-prod-alerts")},
    )
    action := awscloudwatchactions.NewSnsAction(topic)

    alarms := []awscloudwatch.Alarm{
        awscloudwatch.NewAlarm(stack, jsii.String("DLQ"), &awscloudwatch.AlarmProps{
            AlarmName: jsii.String("GoBridge-DLQ"),
            Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
                Namespace: jsii.String("GoBridge/Runtime"), MetricName: jsii.String("DLQEntries"),
                Statistic: jsii.String("Sum"), Period: awscdk.Duration_Minutes(jsii.Number(5)),
            }),
            Threshold: jsii.Number(0), EvaluationPeriods: jsii.Number(1),
            ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
            TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
        }),
        awscloudwatch.NewAlarm(stack, jsii.String("CPU"), &awscloudwatch.AlarmProps{
            AlarmName: jsii.String("GoBridge-HighCPU"),
            Metric: bridge.WorkerService().(awsecs.BaseService).MetricCpuUtilization(nil),
            Threshold: jsii.Number(80), EvaluationPeriods: jsii.Number(3),
            ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
        }),
    }
    for _, a := range alarms {
        a.AddAlarmAction(action)
        a.AddOkAction(action)
    }

    // --- Dashboard ---
    dash := awscloudwatch.NewDashboard(stack, jsii.String("Dash"),
        &awscloudwatch.DashboardProps{DashboardName: jsii.String("GoBridge-Production")},
    )
    dash.AddWidgets(
        awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
            Title: jsii.String("Throughput"), Width: jsii.Number(12),
            Left: &[]awscloudwatch.IMetric{
                awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
                    Namespace: jsii.String("GoBridge/Runtime"),
                    MetricName: jsii.String("MessagesReceived"), Statistic: jsii.String("Sum"),
                }),
                awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
                    Namespace: jsii.String("GoBridge/Runtime"),
                    MetricName: jsii.String("MessagesSent"), Statistic: jsii.String("Sum"),
                }),
            },
        }),
        awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
            Title: jsii.String("ECS Health"), Width: jsii.Number(12),
            Left: &[]awscloudwatch.IMetric{
                bridge.WorkerService().(awsecs.BaseService).MetricCpuUtilization(nil),
                bridge.WorkerService().(awsecs.BaseService).MetricMemoryUtilization(nil),
            },
        }),
    )

    return stack
}

func main() {
    app := awscdk.NewApp(nil)
    NewProductionStack(app, "GoBridgeProd", &awscdk.StackProps{
        Env: &awscdk.Environment{
            Account: jsii.String("123456789012"), Region: jsii.String("eu-west-1"),
        },
    })
    app.Synth(nil)
}
```

Deploy:

```bash
cdk deploy GoBridgeProd
```

---
