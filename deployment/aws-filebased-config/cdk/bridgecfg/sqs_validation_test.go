package bridgecfg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestValidateSQSConfigBindingIdentity(t *testing.T) {
	for _, tt := range []struct {
		name      string
		sender    sqs.Config
		binding   sqs.Config
		wantError bool
	}{
		{"same name", sqs.Config{QueueName: "queue-a"}, sqs.Config{QueueName: "queue-a"}, false},
		{"different name", sqs.Config{QueueName: "queue-a"}, sqs.Config{QueueName: "queue-b"}, true},
		{"same URL", sqs.Config{QueueURL: "https://sqs.local/queue-a"}, sqs.Config{QueueURL: "https://sqs.local/queue-a"}, false},
		{"different URL", sqs.Config{QueueURL: "https://sqs.local/queue-a"}, sqs.Config{QueueURL: "https://sqs.local/queue-b"}, true},
		{"URL precedence", sqs.Config{QueueURL: "https://sqs.local/queue-a", QueueName: "queue-a"}, sqs.Config{QueueName: "queue-a"}, true},
		{"same scope", sqs.Config{QueueName: "queue-a", Region: "eu-west-1"}, sqs.Config{QueueName: "queue-a", Region: "eu-west-1"}, false},
		{"partial scope", sqs.Config{QueueName: "queue-a", Region: "eu-west-1"}, sqs.Config{DelaySeconds: 3}, false},
		{"different profile", sqs.Config{QueueName: "queue-a"}, sqs.Config{Profile: "other"}, true},
		{"different endpoint", sqs.Config{QueueName: "queue-a"}, sqs.Config{Endpoint: "https://another.example"}, true},
		{"different prefix", sqs.Config{QueueTags: map[string]string{"queue": "a"}, QueueNamePrefix: "queue-a"}, sqs.Config{QueueTags: map[string]string{"queue": "a"}, QueueNamePrefix: "queue-b"}, true},
		{"different tags", sqs.Config{QueueTags: map[string]string{"queue": "a"}}, sqs.Config{QueueTags: map[string]string{"queue": "b"}}, true},
		{"invalid selector", sqs.Config{QueueName: "queue-a"}, sqs.Config{QueueTags: map[string]string{}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ports.BridgeConfig{
				Senders:  []ports.SenderDef{{ID: "out", Transport: "aws.sqs", Config: tt.sender}},
				Bindings: []ports.BindingDef{{ID: "bind", SenderID: "out", Address: sqs.QueueAddress, Config: tt.binding}},
			}
			err := bridgecfg.ValidateSQSConfig(cfg)
			if (err != nil) != tt.wantError {
				t.Fatalf("ValidateSQSConfig = %v, wantError=%v", err, tt.wantError)
			}
			if err != nil && (!errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "binding")) {
				t.Fatalf("missing binding-scoped invalid configuration: %v", err)
			}
		})
	}
}
