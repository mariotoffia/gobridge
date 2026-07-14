//go:build !race

package validation_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/aws-cdk-go/awscdk/v2/cxapi"
	"github.com/aws/jsii-runtime-go"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// newStack spins a throwaway App+Stack to host the construct scope
// Phase 2 attaches Annotations to. jsii.Close on Cleanup keeps the
// jsii subprocess from leaking across tests.
func newStack(t *testing.T) awscdk.Stack {
	t.Helper()
	app := awscdk.NewApp(nil)
	t.Cleanup(jsii.Close)
	return awscdk.NewStack(app, jsii.String("TestStack"), nil)
}

// errorMessages returns every Annotation error string emitted under
// stack, regardless of the construct that produced it. The assertions
// API is keyed by construct path + matcher; "*" matches every path.
func errorMessages(t *testing.T, stack awscdk.Stack) []string {
	t.Helper()
	a := assertions.Annotations_FromStack(stack)
	return annotationData(a.FindError(jsii.String("*"), assertions.Match_AnyValue()))
}

// warningMessages mirrors errorMessages for the warning bucket.
func warningMessages(t *testing.T, stack awscdk.Stack) []string {
	t.Helper()
	a := assertions.Annotations_FromStack(stack)
	return annotationData(a.FindWarning(jsii.String("*"), assertions.Match_AnyValue()))
}

// annotationData flattens the string payloads from a set of synthesis
// messages (FindError and FindWarning both return this shape).
func annotationData(msgs *[]*cxapi.SynthesisMessage) []string { //nolint:staticcheck // CDK assertions API (FindError/FindWarning) still returns SynthesisMessage; no Go replacement yet.
	if msgs == nil {
		return nil
	}
	out := make([]string, 0, len(*msgs))
	for _, m := range *msgs {
		if m == nil || m.Entry == nil { //nolint:staticcheck // CDK assertions API still surfaces SynthesisMessage; no replacement on Annotations.Find* yet.
			continue
		}
		switch v := m.Entry.Data.(type) { //nolint:staticcheck // see above
		case string:
			out = append(out, v)
		case *string:
			if v != nil {
				out = append(out, *v)
			}
		}
	}
	return out
}

// containsAny reports whether at least one msg in haystack contains
// every needle. Tests assert on substrings — Phase 2's exact wording
// is intentionally rich (what / expected / fix) and stable, but
// pinning every byte would be brittle.
func containsAll(t *testing.T, haystack []string, needles ...string) bool {
	t.Helper()
	for _, m := range haystack {
		ok := true
		for _, n := range needles {
			if !strings.Contains(m, n) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// sqsReceiverNamed builds a *ports.BridgeConfig with one SQS receiver
// referencing the given logical queue name.
func sqsReceiverNamed(name string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-1"},
	}
	def := ports.ReceiverDef{ID: "r1", Transport: "sqs"}
	def.SetDecoded(&sqs.Config{QueueName: name}, nil)
	cfg.Receivers = append(cfg.Receivers, def)
	return cfg
}

// sqsSenderNamed mirrors sqsReceiverNamed for the sender side.
func sqsSenderNamed(name string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-1"},
	}
	def := ports.SenderDef{ID: "s1", Transport: "sqs"}
	def.SetDecoded(&sqs.Config{QueueName: name}, nil)
	cfg.Senders = append(cfg.Senders, def)
	return cfg
}

// ssmReceiverWithCreds returns a config whose SQS receiver carries a
// credentials_uri; SQS Config implements ports.CredentialedConfig so
// it doubles as the carrier for SSM URI extraction tests.
func ssmReceiverWithCreds(uri string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-1"},
	}
	def := ports.ReceiverDef{ID: "r1", Transport: "sqs"}
	// QueueURL is set so the SQS branch is satisfied without a
	// QueueRegistry; the test isolates the SSM branch.
	def.SetDecoded(&sqs.Config{QueueURL: "https://sqs/x", CredentialsURIRef: uri}, nil)
	cfg.Receivers = append(cfg.Receivers, def)
	return cfg
}

func TestRunPhase2_NoRefsNoRegistries_ZeroErrors(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors, got %d: %v", len(got), got)
	}
}

func TestRunPhase2_SQSReceiver_MissingFromRegistry_EmitsError(t *testing.T) {
	stack := newStack(t)
	cfg := sqsReceiverNamed("foo")
	reg := registry.NewQueueRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "SQS queue \"foo\"", "no such entry in QueueRegistry", "AddQueue(\"foo\"") {
		t.Fatalf("missing expected text in errors: %v", got)
	}
}

func TestRunPhase2_SQSSender_MissingFromRegistry_EmitsError(t *testing.T) {
	stack := newStack(t)
	cfg := sqsSenderNamed("orders-out")
	reg := registry.NewQueueRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "SQS queue \"orders-out\"", "no such entry in QueueRegistry") {
		t.Fatalf("missing expected text in errors: %v", got)
	}
}

func TestRunPhase2_SQSRef_NilQueueRegistry_EmitsRequiredPropError(t *testing.T) {
	stack := newStack(t)
	cfg := sqsReceiverNamed("foo")

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "no QueueRegistry was supplied", "QueueRegistry prop", "\"foo\"") {
		t.Fatalf("missing required-prop error text: %v", got)
	}
	// Nil-registry path emits exactly one aggregated error.
	if len(got) != 1 {
		t.Fatalf("expected exactly one error, got %d: %v", len(got), got)
	}
}

func TestRunPhase2_SQSExplicitURL_NotLookedUp(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}
	def := ports.ReceiverDef{ID: "r1", Transport: "sqs"}
	def.SetDecoded(&sqs.Config{QueueURL: "https://sqs.eu-west-1.amazonaws.com/123/q"}, nil)
	cfg.Receivers = append(cfg.Receivers, def)

	// Pass nil QueueRegistry: explicit-URL receivers must not trigger
	// the "registry required" branch.
	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors for explicit QueueURL, got: %v", got)
	}
}

func TestRunPhase2_MultipleMissingQueues_AllReportedSorted(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}
	for _, n := range []string{"zeta", "alpha", "alpha", "mike"} {
		def := ports.ReceiverDef{ID: "r-" + n, Transport: "sqs"}
		def.SetDecoded(&sqs.Config{QueueName: n}, nil)
		cfg.Receivers = append(cfg.Receivers, def)
	}
	reg := registry.NewQueueRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	got := errorMessages(t, stack)
	// Three unique misses → three errors (alpha deduplicated).
	if len(got) != 3 {
		t.Fatalf("expected 3 deduplicated errors, got %d: %v", len(got), got)
	}
	// Deterministic order: alpha, mike, zeta.
	wantOrder := []string{"\"alpha\"", "\"mike\"", "\"zeta\""}
	for i, want := range wantOrder {
		if !strings.Contains(got[i], want) {
			t.Fatalf("error %d = %q, want it to mention %s (full list: %v)", i, got[i], want, got)
		}
	}
}

func TestRunPhase2_AllQueuesRegistered_ZeroErrors(t *testing.T) {
	stack := newStack(t)
	cfg := sqsReceiverNamed("orders-in")
	reg := registry.NewQueueRegistry()
	q := awssqs.NewQueue(stack, jsii.String("Q1"), &awssqs.QueueProps{
		QueueName: jsii.String("orders-in"),
	})
	reg.AddQueue("orders-in", q)

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors, got: %v", got)
	}
}

func TestRunPhase2_SSMURI_NilSsmParamRegistry_EmitsRequiredPropError(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("pms://bridge/mqtt")

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "no SsmParamRegistry was supplied", "SsmParamRegistry prop", "\"pms://bridge/mqtt\"") {
		t.Fatalf("missing required-prop error text: %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one aggregated error, got %d: %v", len(got), got)
	}
}

func TestRunPhase2_SSMURI_MissingFromRegistry_EmitsError(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("pms://bridge/missing")
	reg := registry.NewSsmParamRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, SsmParamRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got,
		"\"pms://bridge/missing\"",
		"\"/bridge/missing\"",
		"no such entry in SsmParamRegistry",
		"AddParameter(\"/bridge/missing\"",
	) {
		t.Fatalf("missing expected text in errors: %v", got)
	}
}

func TestRunPhase2_SSMURI_Registered_ZeroErrors(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("pms://bridge/mqtt")
	reg := registry.NewSsmParamRegistry()
	p := awsssm.StringParameter_FromStringParameterName(stack, jsii.String("P1"), jsii.String("/bridge/mqtt"))
	reg.AddParameter("/bridge/mqtt", p)

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, SsmParamRegistry: reg})

	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors, got: %v", got)
	}
}

func TestRunPhase2_SSMURI_NonPMSScheme_Ignored(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("vault://kv/secret/db")

	// Non-pms scheme: SSM branch should not fire even with nil registry.
	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors for non-pms URI, got: %v", got)
	}
}

func TestRunPhase2_MalformedClusterEndpoint_EmitsError(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID: "bridge-1",
			Cluster: &ports.ClusterConfig{
				Endpoints: map[string]string{
					"node-a": "not a url",
					"node-b": "https://valid.internal:8443",
				},
			},
		},
	}

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "bridge.cluster.endpoints[\"node-a\"]", "not a url") {
		t.Fatalf("missing endpoint error: %v", got)
	}
	// The valid entry must not produce an error.
	for _, m := range got {
		if strings.Contains(m, "node-b") {
			t.Fatalf("unexpected error for valid endpoint: %q", m)
		}
	}
}

func TestRunPhase2_NilScopeOrCfg_NoOp(t *testing.T) {
	// Should not panic. Nothing to assert beyond absence of panic.
	validation.RunPhase2(nil, validation.Phase2Input{})
	stack := newStack(t)
	validation.RunPhase2(stack, validation.Phase2Input{Cfg: nil})
}

// dynamoStore builds a config with one DynamoDB-backed store in the
// given role ("lease"/"outbox"/"dlq"/"managed_subscriptions") carrying
// tableName (empty to omit it).
func dynamoStore(role, tableName string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}
	sc := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	sc.SetDecoded(&awsstore.DynamoDBConfig{TableName: tableName}, nil)
	switch role {
	case "lease":
		cfg.Stores.Lease = sc
	case "outbox":
		cfg.Stores.Outbox = sc
	case "dlq":
		cfg.Stores.DLQ = sc
	case "managed_subscriptions":
		cfg.Stores.ManagedSubscriptions = sc
	}
	return cfg
}

func TestRunPhase2_DynamoStoreNoTableName_EmitsWarningNotError(t *testing.T) {
	stack := newStack(t)
	cfg := dynamoStore("lease", "")

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	// Omitting table_name is a valid (if risky) choice: warn, never
	// fail synth.
	if got := errorMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero errors (missing table_name is a warning), got %d: %v", len(got), got)
	}
	got := warningMessages(t, stack)
	if !containsAll(t, got, "dynamodb lease store", "table_name", "AccessDenied") {
		t.Fatalf("missing dynamodb table_name warning: %v", got)
	}
}

func TestRunPhase2_ManagedSubscriptionDynamoStoreNoTableName_EmitsWarning(t *testing.T) {
	stack := newStack(t)
	cfg := dynamoStore("managed_subscriptions", "")

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	got := warningMessages(t, stack)
	if !containsAll(t, got, "dynamodb managed_subscriptions store", "table_name", "AccessDenied") {
		t.Fatalf("missing managed-subscription table_name warning: %v", got)
	}
}

func TestRunPhase2_ManagedSubscriptionStoreSSMURI_IsCollected(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}
	sc := &ports.StoreConfig{Type: "credentialed-test-store"}
	sc.SetDecoded(&sqs.Config{CredentialsURIRef: "pms://bridge/managed-store"}, nil)
	cfg.Stores.ManagedSubscriptions = sc

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	got := errorMessages(t, stack)
	if !containsAll(t, got, "no SsmParamRegistry was supplied", "pms://bridge/managed-store") {
		t.Fatalf("missing managed-subscription SSM URI error: %v", got)
	}
}

func TestRunPhase2_DynamoStoreWithTableName_NoWarning(t *testing.T) {
	stack := newStack(t)
	cfg := dynamoStore("outbox", "gobridge-outbox")

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	if got := warningMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero warnings when table_name is set, got %d: %v", len(got), got)
	}
}

func TestRunPhase2_NonDynamoStore_NoWarning(t *testing.T) {
	stack := newStack(t)
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-1"}}
	cfg.Stores.Lease = &ports.StoreConfig{Type: "sqlite"}

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg})

	if got := warningMessages(t, stack); len(got) != 0 {
		t.Fatalf("expected zero warnings for a non-dynamodb store, got %d: %v", len(got), got)
	}
}
