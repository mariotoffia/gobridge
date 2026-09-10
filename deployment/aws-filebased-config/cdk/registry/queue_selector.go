package registry

import (
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
)

type queueSelector struct {
	tags   map[string]string
	prefix string
}

// BindQueueTags binds an explicit runtime selector to an already registered
// queue. Owned queues receive these tags through CDK. For imported queues this
// call is the caller's assertion that the producer applies these tags; CDK
// cannot inspect or change tags on an imported IQueue. No AWS calls are made.
//
// A queue can be bound only once. Selectors must be literal synth-time values.
// With a generated physical name, the caller must ensure any prefix matches
// the deployed name; omit the prefix when that cannot be guaranteed.
func (r *QueueRegistry) BindQueueTags(name string, tags map[string]string, queueNamePrefix string) error {
	if r == nil || !r.Has(name) {
		return fmt.Errorf("registry: BindQueueTags requires AddQueue(%q, queue) first", name)
	}
	cfg := sqs.Config{QueueTags: tags, QueueNamePrefix: queueNamePrefix}
	if err := cfg.ValidateQueue(); err != nil {
		return fmt.Errorf("registry: BindQueueTags: %w", err)
	}
	if tags == nil {
		return fmt.Errorf("registry: BindQueueTags requires a non-empty queue_tags selector")
	}
	if _, exists := r.selectors[name]; exists {
		return fmt.Errorf("registry: queue %q already has a QueueTags binding", name)
	}
	for key, value := range tags {
		if *awscdk.Token_IsUnresolved(jsii.String(key)) || *awscdk.Token_IsUnresolved(jsii.String(value)) {
			return fmt.Errorf("registry: queue_tags must contain literal keys and values, not deployment tokens")
		}
	}
	if *awscdk.Token_IsUnresolved(jsii.String(queueNamePrefix)) {
		return fmt.Errorf("registry: queue_name_prefix must be literal, not a deployment token")
	}
	ref := r.Ref(name)
	for other, selector := range r.selectors {
		if *r.queues[other].QueueArn() == *ref.Queue().QueueArn() &&
			(!maps.Equal(selector.tags, tags) || selector.prefix != queueNamePrefix) {
			return fmt.Errorf("registry: queue %q already has a different QueueTags binding under alias %q", name, other)
		}
	}
	if physical := ref.PhysicalName(); physical != "" && !strings.HasPrefix(physical, queueNamePrefix) {
		return fmt.Errorf("registry: queue_name_prefix does not match queue %q physical name", name)
	}
	if r.selectors == nil {
		r.selectors = make(map[string]queueSelector)
	}
	r.selectors[name] = queueSelector{tags: maps.Clone(tags), prefix: queueNamePrefix}
	// An imported queue has no owned CfnQueue child. Never pretend that
	// tagging its construct mutates the producer's resource.
	if _, owned := ref.Queue().Node().DefaultChild().(awssqs.CfnQueue); owned {
		keys := make([]string, 0, len(tags))
		for key := range tags {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			awscdk.Tags_Of(ref.Queue()).Add(jsii.String(key), jsii.String(tags[key]), nil)
		}
	}
	return nil
}

// ResolveQueue maps a runtime queue reference to one exact registered IQueue.
// Tag selectors match all required tags against explicit BindQueueTags
// declarations, not inferred tags. Ambiguous mappings fail rather than granting
// an arbitrary queue. A direct URL without a registration remains allowed for
// callers that manage its IAM themselves, as before. The optional scope is the
// consuming task's construct, not the queue's producer; it supplies default
// region/account checks. Explicit cfg.Region always takes precedence.
//
// An explicit profile/credential URI/custom endpoint makes the credential
// account unknown to CDK. Unresolved stack/queue environments are also unknown.
// In those cases registration is the caller's assertion that the queue belongs
// to the actual runtime discovery scope; CDK never guesses a credential account.
func (r *QueueRegistry) ResolveQueue(cfg sqs.Config, runtimeScope ...constructs.Construct) (QueueRef, error) {
	if err := cfg.ValidateQueue(); err != nil {
		return QueueRef{}, err
	}
	if len(runtimeScope) > 1 {
		return QueueRef{}, fmt.Errorf("registry: ResolveQueue accepts at most one runtime scope")
	}
	if r == nil {
		if cfg.QueueURL != "" {
			return QueueRef{}, nil
		}
		if cfg.QueueTags != nil {
			return QueueRef{}, fmt.Errorf("SQS queue_tags requires the QueueRegistry prop; no QueueRegistry was supplied. Call AddQueue and BindQueueTags for the intended queue")
		}
		return QueueRef{}, fmt.Errorf("SQS reference requires the QueueRegistry prop; no QueueRegistry was supplied")
	}
	names := r.Names()
	sort.Strings(names)
	var match QueueRef
	var scopeErr error
	for _, name := range names {
		ref := r.Ref(name)
		if !queueReferenceMatches(cfg, ref) {
			continue
		}
		if err := checkQueueScope(cfg, ref, runtimeScope); err != nil {
			scopeErr = err
			continue
		}
		if match.IsResolved() && *match.Queue().QueueArn() != *ref.Queue().QueueArn() {
			return QueueRef{}, fmt.Errorf("SQS queue reference has an ambiguous QueueRegistry mapping; use a unique queue_tags selector or physical name")
		}
		match = ref
	}
	if match.IsResolved() {
		return match, nil
	}
	if scopeErr != nil {
		return QueueRef{}, scopeErr
	}
	if cfg.QueueURL != "" {
		return QueueRef{}, nil
	}
	if cfg.QueueTags != nil {
		return QueueRef{}, fmt.Errorf("SQS queue_tags has no matching QueueRegistry entry; call AddQueue and BindQueueTags for the intended queue")
	}
	if r.Has(cfg.QueueName) {
		return QueueRef{}, fmt.Errorf("SQS queue %q is a registry alias, not its physical QueueName; use the physical name or BindQueueTags", cfg.QueueName)
	}
	return QueueRef{}, fmt.Errorf("SQS queue %q has no such entry in QueueRegistry; call AddQueue(%q, queue) with that physical QueueName", cfg.QueueName, cfg.QueueName)
}

func queueReferenceMatches(cfg sqs.Config, ref QueueRef) bool {
	if cfg.QueueTags != nil {
		if ref.tags == nil {
			return false
		}
		for key, value := range cfg.QueueTags {
			if actual, ok := ref.tags[key]; !ok || actual != value {
				return false
			}
		}
		if physical := ref.PhysicalName(); physical != "" {
			return strings.HasPrefix(physical, cfg.QueueNamePrefix)
		}
		// Unknown physical names rely on the explicit binding contract.
		// A narrower requested prefix cannot be proven by that contract.
		return strings.HasPrefix(ref.prefix, cfg.QueueNamePrefix)
	}
	if cfg.QueueURL != "" {
		return queueURLMatches(cfg.QueueURL, ref)
	}
	return cfg.QueueName == ref.PhysicalName() ||
		(ref.PhysicalName() == "" && cfg.QueueName == *ref.Queue().QueueName())
}

func queueURLMatches(value string, ref QueueRef) bool {
	if queueURL := ref.Queue().QueueUrl(); queueURL != nil && value == *queueURL {
		return true
	}
	// Imported IQueue URLs may still contain a URLSuffix token even when
	// the ARN and physical name are literal. Match a literal AWS URL using
	// all of its account, region and name, never only its trailing name.
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" {
		return false
	}
	env := ref.Queue().Env()
	if env == nil || env.Account == nil || env.Region == nil {
		return false
	}
	if parsed.Path != "/"+*env.Account+"/"+ref.PhysicalName() || ref.PhysicalName() == "" {
		return false
	}
	host := parsed.Hostname()
	return host == "sqs."+*env.Region+".amazonaws.com" ||
		host == "sqs."+*env.Region+".amazonaws.com.cn" ||
		host == "sqs."+*env.Region+".api.aws"
}
