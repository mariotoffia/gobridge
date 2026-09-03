# API Gateway — complete CDK stack

The full stack combines the `GoBridgeSingle` facade, NLB, VPC Link,
REST API, usage plans, and custom domain:

```go
package main

import (
    "github.com/aws/aws-cdk-go/awscdk/v2"
    apigw "github.com/aws/aws-cdk-go/awscdk/v2/awsapigateway"
    "github.com/aws/aws-cdk-go/awscdk/v2/awscertificatemanager"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsroute53"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsroute53targets"
    "github.com/aws/constructs-go/constructs/v10"
    "github.com/aws/jsii-runtime-go"

    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func NewAPIGatewayStack(scope constructs.Construct, id string) awscdk.Stack {
    stack := awscdk.NewStack(scope, &id, &awscdk.StackProps{
        Env: &awscdk.Environment{
            Account: jsii.String("123456789012"),
            Region:  jsii.String("us-west-1"),
        },
    })

    // --- VPC ---

    vpc := awsec2.Vpc_FromLookup(stack, jsii.String("Vpc"), &awsec2.VpcLookupOptions{
        VpcId: jsii.String("vpc-0abc1234def56789a"),
    })

    // --- GoBridge single facade (auto-creates EFS, cluster, seeder) ---

    bridge := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"),
        &gobridgesingle.SingleProps{
            Vpc: vpc,
            Image: awsecs.ContainerImage_FromRegistry(
                jsii.String("123456789012.dkr.ecr.us-west-1.amazonaws.com/gobridge:latest"),
                nil,
            ),
            Bootstrap: infra.BootstrapConfig{
                BridgeID:         "gobridge-api",
                ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
                AdminAPIKeyParam: "/gobridge/admin-api-key",
            },
            BridgeConfig: gobridgecdk.BridgeYamlAsset("bridge.yaml"),
            CPU:          jsii.Number(1024),
            MemoryMiB:    jsii.Number(2048),
        },
    )

    // --- NLB for API Gateway VPC Link ---

    nlb := elbv2.NewNetworkLoadBalancer(stack, jsii.String("TransportNLB"),
        &elbv2.NetworkLoadBalancerProps{
            Vpc:              vpc,
            InternetFacing:   jsii.Bool(false),
            CrossZoneEnabled: jsii.Bool(true),
        },
    )

    nlbListener := nlb.AddListener(jsii.String("Transport"),
        &elbv2.BaseNetworkListenerProps{
            Port:     jsii.Number(8082),
            Protocol: elbv2.Protocol_TCP,
        },
    )

    // The facade exposes the control ECS service via ControlService(); a
    // BaseService is itself an INetworkLoadBalancerTarget.
    nlbListener.AddTargets(jsii.String("TransportTG"),
        &elbv2.AddNetworkTargetsProps{
            Port:    jsii.Number(8082),
            Targets: &[]elbv2.INetworkLoadBalancerTarget{bridge.ControlService().(awsecs.BaseService)},
            HealthCheck: &elbv2.HealthCheck{
                Port:     jsii.String("8081"),
                Protocol: elbv2.Protocol_HTTP,
                Path:     jsii.String("/api/v1/monitor/health"),
            },
        },
    )

    // VPC Link connecting API Gateway to the private NLB.
    vpcLink := apigw.NewVpcLink(stack, jsii.String("VpcLink"),
        &apigw.VpcLinkProps{
            Targets: &[]elbv2.INetworkLoadBalancer{nlb},
        },
    )

    // --- REST API ---

    api := apigw.NewRestApi(stack, jsii.String("TransportAPI"),
        &apigw.RestApiProps{
            RestApiName: jsii.String("gobridge-transport"),
            Deploy:      jsii.Bool(true),
            DeployOptions: &apigw.StageOptions{
                StageName: jsii.String("v1"),
            },
        },
    )

    // Proxy integration: forward all paths through VPC Link to NLB.
    integration := apigw.NewIntegration(&apigw.IntegrationProps{
        Type:                  apigw.IntegrationType_HTTP_PROXY,
        IntegrationHttpMethod: jsii.String("ANY"),
        Options: &apigw.IntegrationOptions{
            ConnectionType: apigw.ConnectionType_VPC_LINK,
            VpcLink:        vpcLink,
        },
        Uri: jsii.String(
            "http://" + *nlb.LoadBalancerDnsName() + ":8082/{proxy}",
        ),
    })

    // {proxy+} catches all sub-paths under the root.
    proxy := api.Root().AddProxy(&apigw.ProxyResourceOptions{
        DefaultIntegration: integration,
        AnyMethod:          jsii.Bool(true),
        DefaultMethodOptions: &apigw.MethodOptions{
            ApiKeyRequired: jsii.Bool(true),
        },
    })
    _ = proxy

    // --- Usage plan and API key ---

    plan := api.AddUsagePlan(jsii.String("PartnerPlan"),
        &apigw.UsagePlanProps{
            Name: jsii.String("partner-standard"),
            Throttle: &apigw.ThrottleSettings{
                RateLimit:  jsii.Number(50),
                BurstLimit: jsii.Number(100),
            },
            Quota: &apigw.QuotaSettings{
                Limit:  jsii.Number(10000),
                Period: apigw.Period_DAY,
            },
        },
    )
    plan.AddApiStage(&apigw.UsagePlanPerApiStage{
        Api:   api,
        Stage: api.DeploymentStage(),
    })

    partnerKey := api.AddApiKey(jsii.String("PartnerAlphaKey"),
        &apigw.ApiKeyOptions{ApiKeyName: jsii.String("partner-alpha")},
    )
    plan.AddApiKey(partnerKey)

    // --- Custom domain ---

    zone := awsroute53.HostedZone_FromLookup(stack, jsii.String("Zone"),
        &awsroute53.HostedZoneProviderProps{
            DomainName: jsii.String("example.com"),
        },
    )

    cert := awscertificatemanager.NewCertificate(stack, jsii.String("APICert"),
        &awscertificatemanager.CertificateProps{
            DomainName: jsii.String("api.example.com"),
            Validation: awscertificatemanager.CertificateValidation_FromDns(zone),
        },
    )

    domain := apigw.NewDomainName(stack, jsii.String("APIDomain"),
        &apigw.DomainNameProps{
            DomainName:   jsii.String("api.example.com"),
            Certificate:  cert,
            EndpointType: apigw.EndpointType_REGIONAL,
        },
    )
    domain.AddBasePathMapping(api, &apigw.BasePathMappingOptions{
        BasePath: jsii.String("v1"),
    })

    awsroute53.NewARecord(stack, jsii.String("APIAlias"),
        &awsroute53.ARecordProps{
            Zone:       zone,
            RecordName: jsii.String("api"),
            Target: awsroute53.RecordTarget_FromAlias(
                awsroute53targets.NewApiGatewayDomain(domain),
            ),
        },
    )

    return stack
}

func main() {
    app := awscdk.NewApp(nil)
    NewAPIGatewayStack(app, "GoBridgeAPIGateway")
    app.Synth(nil)
}
```

### Code Walkthrough

| Section | Lines | Purpose |
|---------|-------|---------|
| VPC lookup | `Vpc_FromLookup` | Import existing VPC by ID |
| GoBridge service | `NewGoBridgeSingle` | Fargate task with EFS, SSM, config seeder |
| NLB + target group | `NewNetworkLoadBalancer` | Internal NLB on port 8082, health check on 8081 |
| VPC Link | `NewVpcLink` | Connects API Gateway to the private NLB |
| REST API + proxy | `NewRestApi`, `AddProxy` | Catches all paths, requires API key |
| Usage plan | `AddUsagePlan` | 50 req/s rate, 100 burst, 10K/day quota |
| Custom domain | `NewDomainName`, `NewARecord` | ACM cert + Route 53 alias for `api.example.com` |

The facade maps the transport HTTP port (8082) on the container by default, so
the NLB target on 8082 with the health check on 8081 lines up out of the box.
The single facade runs one task and has no autoscaling; use the cluster facade
([Scenario 5](05-multi-bridge-cluster.md)) when you need multiple replicas.
