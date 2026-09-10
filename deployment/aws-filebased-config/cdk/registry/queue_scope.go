package registry

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
)

func checkQueueScope(cfg sqs.Config, ref QueueRef, scopes []constructs.Construct) error {
	region := cfg.Region
	account := ""
	if len(scopes) == 1 && scopes[0] != nil {
		stack := awscdk.Stack_Of(scopes[0])
		// ECS supplies the task region to the SDK environment. Credential
		// selection does not erase that known region; cfg.Region overrides it.
		if region == "" {
			region = *stack.Region()
		}
		// This is the CDK task/default credential contract. Custom profile,
		// credential URI, or endpoint identities cannot be inspected at synth.
		if cfg.Profile == "" && cfg.CredentialsURIRef == "" && cfg.Endpoint == "" {
			account = *stack.Account()
		}
	}
	env := ref.Queue().Env()
	if env == nil {
		return nil
	}
	if env.Region != nil && knownScopeValue(region) && knownScopeValue(*env.Region) && region != *env.Region {
		return fmt.Errorf("SQS queue reference does not match the effective runtime region; register a queue in the configured region")
	}
	// Neither ListQueues nor this adapter's name lookup supplies an owner
	// account override. Direct URLs, unlike discovery, can be cross-account.
	if cfg.QueueURL == "" && env.Account != nil && knownScopeValue(account) &&
		knownScopeValue(*env.Account) && account != *env.Account {
		return fmt.Errorf("SQS queue reference does not match the task/default credential account; queue discovery cannot cross accounts")
	}
	return nil
}

func knownScopeValue(value string) bool {
	return value != "" && !*awscdk.Token_IsUnresolved(value)
}
