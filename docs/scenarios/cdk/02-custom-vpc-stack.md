# Custom VPC — complete CDK stack

## Overview

The full stack wires the imported VPC and ECS cluster into the
`gobridge.NewCluster` facade (control + worker tasks sharing one
EFS filesystem, chosen here because we want more than one replica), then binds
it to the shared ALB listener with `gobridge.NewALBAttachment` — the
attachment owns the target groups and listener rules, so you do not wire them by
hand.

The registry image must contain its own embedded initial document, use an
existing target, or wait for operator creation. `BridgeConfig` declares
validation and grants; CDK does not modify that image or overwrite config.
See [initial configuration](../../aws-deployment/config-initialization.md).

```go
package main

import (
    "github.com/aws/aws-cdk-go/awscdk/v2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
    "github.com/aws/constructs-go/constructs/v10"
    "github.com/aws/jsii-runtime-go"

    "github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridge"
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

    src := gobridge.ConfigFile("bridge.yaml")
    workers := float64(2)

    bridge := gobridge.NewCluster(stack, "Bridge",
        &gobridge.ClusterProps{
            Vpc:     vpc,
            Cluster: cluster, // reuse the imported ECS cluster
            Image: gobridge.ImageFromRegistry(
                "123456789012.dkr.ecr.eu-west-1.amazonaws.com/gobridge@sha256:<digest>"),
            // NewCluster forces topology filesystem_replicated.
            Bootstrap: gobridge.Bootstrap{
                BridgeID:         "gobridge-mqtt",
                ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
                AdminAPIKeyParam: "/gobridge/prod/admin-api-key",
            },
            BridgeConfig:       src,
            CPU:                jsii.Number(1024),
            MemoryMiB:          jsii.Number(2048),
            WorkerDesiredCount: &workers,
            // Autoscaling is opt-in and applies to the worker service only.
            AutoScaling: &gobridge.AutoScaling{
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

    gobridge.NewALBAttachment(stack, "Attach",
        &gobridge.ALBAttachmentProps{
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
security groups for EFS access, container port mappings, and
(when `AutoScaling` is set) worker CPU target-tracking. The attachment construct
creates the admin/monitor/transport target groups and listener rules against the
shared ALB. Exactly one of `Single` or `Cluster` is set on the attachment.
