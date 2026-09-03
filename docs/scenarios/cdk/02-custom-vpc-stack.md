# Custom VPC — complete CDK stack

The full stack wires the imported VPC and ECS cluster into the
`gobridgecluster.NewGoBridgeCluster` facade (control + worker tasks sharing one
EFS filesystem, chosen here because we want more than one replica), then binds
it to the shared ALB listener with the `gobridgealbattachment` construct — the
attachment owns the target groups and listener rules, so you do not wire them by
hand.

```go
package main

import (
    "github.com/aws/aws-cdk-go/awscdk/v2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
    "github.com/aws/constructs-go/constructs/v10"
    "github.com/aws/jsii-runtime-go"

    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func NewCustomVpcStack(scope constructs.Construct, id string) awscdk.Stack {
    stack := awscdk.NewStack(scope, &id, &awscdk.StackProps{
        Env: &awscdk.Environment{
            Account: jsii.String("123456789012"),
            Region:  jsii.String("eu-west-1"),
        },
    })

    // --- Import existing infrastructure ---

    vpc := awsec2.Vpc_FromLookup(stack, jsii.String("Vpc"), &awsec2.VpcLookupOptions{
        VpcId: jsii.String("vpc-0abc1234def56789a"),
    })

    cluster := awsecs.Cluster_FromClusterAttributes(stack, jsii.String("Cluster"),
        &awsecs.ClusterAttributes{
            ClusterName:    jsii.String("platform-ecs"),
            Vpc:            vpc,
            SecurityGroups: &[]awsec2.ISecurityGroup{},
        },
    )

    // --- GoBridge cluster facade (control + workers, shared EFS) ---

    src := gobridgecdk.BridgeYamlAsset("bridge.yaml")
    workers := float64(2)

    bridge := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"),
        &gobridgecluster.ClusterProps{
            Vpc:     vpc,
            Cluster: cluster, // reuse the imported ECS cluster
            Image: awsecs.ContainerImage_FromRegistry(
                jsii.String("123456789012.dkr.ecr.eu-west-1.amazonaws.com/gobridge:latest"),
                nil,
            ),
            Bootstrap: infra.BootstrapConfig{
                BridgeID:         "gobridge-mqtt",
                ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
                AdminAPIKeyParam: "/gobridge/prod/admin-api-key",
                Topology:         infra.TopologyFilesystemReplicated,
            },
            BridgeConfig:       src,
            CPU:                jsii.Number(1024),
            MemoryMiB:          jsii.Number(2048),
            WorkerDesiredCount: &workers,
            // Autoscaling is opt-in and applies to the worker service only.
            AutoScaling: &gobridgecluster.AutoScalingProps{
                Min:       2,
                Max:       6,
                TargetCPU: 65,
            },
        },
    )

    // --- Bind to the shared ALB listener ---

    listener := elbv2.ApplicationListener_FromLookup(stack, jsii.String("Listener"),
        &elbv2.ApplicationListenerLookupOptions{
            LoadBalancerTags: &map[string]string{"purpose": "internal-services"},
            ListenerPort:     jsii.Number(443),
        },
    )

    gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attach"),
        &gobridgealbattachment.AttachmentProps{
            Cluster:      bridge,
            Listener:     listener,
            Vpc:          vpc,
            BridgeConfig: src,
        },
    )

    return stack
}

func main() {
    app := awscdk.NewApp(nil)
    NewCustomVpcStack(app, "GoBridgeCustomVpc")
    app.Synth(nil)
}
```

The cluster facade handles task definitions, EFS volume mounts, IAM policies,
security groups for EFS access, container port mappings, the config seeder, and
(when `AutoScaling` is set) worker CPU target-tracking. The attachment construct
creates the admin/monitor/transport target groups and listener rules against the
shared ALB. Exactly one of `Single` or `Cluster` is set on the attachment.
