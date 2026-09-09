package gobridgesingle_test

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestSingle_InitialConfigNeedsNoExtraContainerOrS3Read(t *testing.T) {
	for _, source := range []string{infra.ConfigSourceFile, infra.ConfigSourceDynamoDB} {
		t.Run(source, func(t *testing.T) {
			app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
			stack := awscdk.NewStack(app, jsii.String("InitialConfig"), nil)
			gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
				Vpc:   awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
				Image: gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{Version: "v0.4.0"}),
				Bootstrap: infra.BootstrapConfig{
					BridgeID: "initial", AdminAPIKeyParam: "/test/admin", ConfigSource: source,
				},
				BridgeConfig: gobridgecdk.BridgeYamlInline(&ports.BridgeConfig{
					Bridge: ports.BridgeSettings{ID: "initial"},
				}),
			})
			tpl := assertions.Template_FromStack(stack, nil)
			tpl.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
				"ContainerDefinitions": []any{assertions.Match_ObjectLike(&map[string]any{"Name": "gobridge"})},
			})
			data, err := json.Marshal(tpl.ToJSON())
			require.NoError(t, err)
			require.NotContains(t, string(data), "s3:GetObject")
			require.NotContains(t, string(data), "ASSET_S3_URI")
			require.NotContains(t, string(data), "ITEM_S3_URI")
		})
	}
}

func TestSingle_EmbeddedQueueTagsGrantDiscoveryAndExactDataAccess(t *testing.T) {
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
	stack := awscdk.NewStack(app, jsii.String("TaggedInitial"), nil)
	queue := awssqs.NewQueue(stack, jsii.String("Queue"), nil)
	queues := registry.NewQueueRegistry()
	queues.AddQueue("orders", queue)
	require.NoError(t, queues.BindQueueTags("orders", map[string]string{"app": "orders"}, ""))
	cfg, err := bridgecfg.New("tags").
		WithSQSReceiver("in", queues.Ref("orders")).
		WithSQSSender("out", queues.Ref("orders")).
		WithRoute("in", "out").Build()
	require.NoError(t, err)
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:           awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
		Image:         gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{Version: "v0.4.0"}),
		Bootstrap:     infra.BootstrapConfig{BridgeID: "tags", AdminAPIKeyParam: "/test/admin"},
		BridgeConfig:  gobridgecdk.BridgeYamlInline(cfg),
		QueueRegistry: queues,
	})
	tpl := assertions.Template_FromStack(stack, nil)
	for _, action := range []string{"sqs:ListQueues", "sqs:ListQueueTags"} {
		tpl.HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
			"PolicyDocument": assertions.Match_ObjectLike(&map[string]any{
				"Statement": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
					"Action": action, "Effect": "Allow",
				})}),
			}),
		})
	}
	tpl.HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
		"PolicyDocument": assertions.Match_ObjectLike(&map[string]any{
			"Statement": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{
				"Action":   assertions.Match_ArrayWith(&[]any{"sqs:ReceiveMessage"}),
				"Resource": stack.Resolve(queue.QueueArn()),
			})}),
		}),
	})
}
