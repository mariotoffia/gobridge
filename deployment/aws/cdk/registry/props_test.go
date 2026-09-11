//go:build !race

package registry_test

import (
	"fmt"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

// Category: unit (TESTS.md §1).
func TestFromProps_EmptyPropsGiveNilRegistries(t *testing.T) {
	queues, secrets, err := registry.FromProps(nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, queues)
	require.Nil(t, secrets)
}

// Category: unit (TESTS.md §1).
func TestFromProps_RegistersEveryQueueSelectorAndSecret(t *testing.T) {
	stack := newStack(t)
	orders := awssqs.NewQueue(stack, jsii.String("Orders"), &awssqs.QueueProps{QueueName: jsii.String("orders")})
	audit := awssqs.NewQueue(stack, jsii.String("Audit"), &awssqs.QueueProps{QueueName: jsii.String("audit")})
	mqtt := awsssm.StringParameter_FromSecureStringParameterAttributes(stack, jsii.String("Mqtt"),
		&awsssm.SecureStringParameterAttributes{ParameterName: jsii.String("/bridge/mqtt")})

	queues, secrets, err := registry.FromProps(
		map[string]awssqs.IQueue{"orders": orders, "audit": audit},
		map[string]registry.QueueTags{"orders": {Tags: map[string]string{"app": "orders"}}},
		map[string]awsssm.IParameter{"pms://bridge/mqtt": mqtt},
	)

	require.NoError(t, err)
	require.ElementsMatch(t, []string{"audit", "orders"}, queues.Names())
	require.Equal(t, orders, queues.Ref("orders").Queue())
	require.Equal(t, map[string]string{"app": "orders"}, queues.Ref("orders").QueueTags())
	require.Empty(t, queues.Ref("audit").QueueTags())
	require.True(t, secrets.Has("/bridge/mqtt"), "a pms:// key registers its canonical path")
}

// Category: unit (TESTS.md §1).
func TestFromProps_QueueTagsWithoutItsQueueIsAnError(t *testing.T) {
	_, _, err := registry.FromProps(nil,
		map[string]registry.QueueTags{"orders": {Tags: map[string]string{"app": "orders"}}}, nil)
	require.Error(t, err)
}

// Category: unit (TESTS.md §1).
func TestFromProps_QueueTagsThatCannotBindIsAnError(t *testing.T) {
	stack := newStack(t)
	orders := awssqs.NewQueue(stack, jsii.String("Orders"), nil)
	_, _, err := registry.FromProps(
		map[string]awssqs.IQueue{"orders": orders},
		map[string]registry.QueueTags{"orders": {}},
		nil,
	)
	require.Error(t, err, "an empty tag selector must be refused")
}

func benchmarkFromProps(b *testing.B, count int) {
	stack := newStack(&testing.T{})
	queues := make(map[string]awssqs.IQueue, count)
	secrets := make(map[string]awsssm.IParameter, count)
	for i := range count {
		name := fmt.Sprintf("queue-%03d", i)
		queues[name] = awssqs.NewQueue(stack, jsii.String(name), &awssqs.QueueProps{QueueName: jsii.String(name)})
		path := fmt.Sprintf("/bridge/secret-%03d", i)
		secrets[path] = awsssm.StringParameter_FromSecureStringParameterAttributes(stack, jsii.String(path),
			&awsssm.SecureStringParameterAttributes{ParameterName: jsii.String(path)})
	}
	for b.Loop() {
		if _, _, err := registry.FromProps(queues, nil, secrets); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFromProps_OneQueueOneSecret(b *testing.B) { benchmarkFromProps(b, 1) }

// A large bridge fans many routes over many queues and credentials.
func BenchmarkFromProps_HundredQueuesAndSecrets(b *testing.B) { benchmarkFromProps(b, 100) }
