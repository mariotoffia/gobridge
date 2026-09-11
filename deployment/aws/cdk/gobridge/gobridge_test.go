//go:build !race

package gobridge_test

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridge"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

// ordersBridge is an SQS route whose receiver reads its credentials from SSM,
// so a deployment needs both a queue grant and a secret grant.
func ordersBridge(t *testing.T) (awscdk.Stack, *gobridge.SingleProps) {
	t.Helper()
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
	stack := awscdk.NewStack(app, jsii.String("OnePackage"), nil)
	orders := awssqs.NewQueue(stack, jsii.String("Orders"), &awssqs.QueueProps{QueueName: jsii.String("orders")})
	refs := registry.NewQueueRegistry()
	refs.AddQueue("orders", orders)
	cfg, err := bridgecfg.New("orders").
		WithSQSReceiver("in", refs.Ref("orders")).
		WithSQSSender("out", refs.Ref("orders")).
		WithRoute("in", "out").
		Build()
	require.NoError(t, err)
	receiver, ok := cfg.Receivers[0].Config.(*sqs.Config)
	require.True(t, ok)
	receiver.CredentialsURIRef = "pms://bridge/sqs"

	return stack, &gobridge.SingleProps{
		Vpc:          awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
		Image:        gobridge.ImageFromGoBuild(gobridge.GoBuild{Version: "v0.4.0"}),
		Bootstrap:    gobridge.Bootstrap{BridgeID: "orders", AdminAPIKeyParam: "/orders/admin"},
		BridgeConfig: gobridge.ConfigInline(cfg),
		Queues:       map[string]awssqs.IQueue{"orders": orders},
	}
}

// Category: unit (TESTS.md §1).
func TestNewSingle_GrantsTheListedQueuesAndSecrets(t *testing.T) {
	stack, props := ordersBridge(t)
	props.Secrets = map[string]awsssm.IParameter{
		"/bridge/sqs": awsssm.StringParameter_FromSecureStringParameterAttributes(stack, jsii.String("SQSCreds"),
			&awsssm.SecureStringParameterAttributes{ParameterName: jsii.String("/bridge/sqs")}),
	}

	gobridge.NewSingle(stack, "Bridge", props)

	assertions.Annotations_FromStack(stack).HasNoError(jsii.String("*"), assertions.Match_AnyValue())
	data, err := json.Marshal(assertions.Template_FromStack(stack, nil).ToJSON())
	require.NoError(t, err)
	require.Contains(t, string(data), "sqs:ReceiveMessage")
	require.Contains(t, string(data), "ssm:GetParameter")
}

// Category: unit (TESTS.md §1).
func TestNewSingle_ASecretMissingFromSecretsFailsSynth(t *testing.T) {
	stack, props := ordersBridge(t)

	gobridge.NewSingle(stack, "Bridge", props)

	assertions.Annotations_FromStack(stack).HasError(jsii.String("*"),
		assertions.Match_StringLikeRegexp(jsii.String(`"/bridge/sqs".*Secrets`)))
}
