package validation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// pmsScheme is the canonical SSM-parameter URI scheme produced by
// adapters/aws/credentials/ssm. Phase 2 only walks credentials_uri
// values that start with this prefix; any other scheme is left to
// the resolver to reject at runtime.
const pmsScheme = "pms://"

// sqsTransport is the discriminator the SQS adapter registers under.
// Mirrors the bridgecfg constant; duplicated here so the validation
// package does not depend on bridgecfg (which itself depends on
// validation peers).
const sqsTransport = "sqs"

// Phase2Input bundles the inputs needed by the aggregated, synth-time
// Phase 2 validator. QueueRegistry and SsmParamRegistry are
// conditionally required: nil is allowed when the yaml does not use
// the corresponding adapter type. Phase 2 inspects the yaml first and
// emits a typed "registry prop is required" error rather than
// dereferencing a nil registry.
type Phase2Input struct {
	Cfg              *ports.BridgeConfig
	QueueRegistry    *registry.QueueRegistry
	SsmParamRegistry *registry.SsmParamRegistry
}

// RunPhase2 emits one CDK Annotation per actionable problem so a
// single synth pass surfaces them all. Messages include (a) what was
// found, (b) what was expected and (c) how to fix. Unresolved
// references are errors (they fail synth); a DynamoDB store that omits
// table_name is a warning — the config is valid, but the stack cannot
// grant a table it cannot name.
//
// Order of checks (deterministic, alphabetised within each bucket):
//
//  1. SQS queue-name resolution against QueueRegistry (error).
//  2. SSM parameter-URI resolution against SsmParamRegistry (error).
//  3. bridge.cluster.endpoints URL parse (error).
//  4. DynamoDB stores missing table_name (warning).
//
// RunPhase2 never panics on a nil registry — see Phase2Input.
func RunPhase2(scope constructs.Construct, in Phase2Input) {
	if scope == nil || in.Cfg == nil {
		return
	}

	emit := func(msg string) {
		awscdk.Annotations_Of(scope).AddError(jsii.String(msg))
	}
	warn := func(msg string) {
		awscdk.Annotations_Of(scope).AddWarning(jsii.String(msg))
	}

	checkSQS(in.Cfg, in.QueueRegistry, emit)
	checkSSM(in.Cfg, in.SsmParamRegistry, emit)
	checkEndpoints(in.Cfg, emit)
	checkDynamoStores(in.Cfg, warn)
}

// checkSQS extracts the logical queue names referenced by SQS
// receivers/senders, deduplicates them, and verifies each is present
// in the registry. When the registry is nil but SQS refs exist, a
// single "QueueRegistry prop is required" error is emitted naming
// every offending queue so the operator can wire up the registry in
// one go.
func checkSQS(cfg *ports.BridgeConfig, reg *registry.QueueRegistry, emit func(string)) {
	names := collectSQSQueueNames(cfg)
	if len(names) == 0 {
		return
	}
	if reg == nil {
		emit(fmt.Sprintf(
			"yaml references SQS queue(s) %s via the SQS transport, but no QueueRegistry was supplied. "+
				"Expected: a *registry.QueueRegistry passed via the construct's QueueRegistry prop. "+
				"Fix: construct registry.NewQueueRegistry(), call AddQueue(name, queue) for each of %s, and pass it as QueueRegistry on the construct props.",
			quoteList(names), quoteList(names),
		))
		return
	}
	for _, name := range names {
		if reg.Has(name) {
			continue
		}
		emit(fmt.Sprintf(
			"yaml references SQS queue %q but no such entry in QueueRegistry. "+
				"Expected: an AddQueue(%q, queue) call before the GoBridge construct is synthesised. "+
				"Fix: registry.AddQueue(%q, queue) on the QueueRegistry passed via the construct's QueueRegistry prop.",
			name, name, name,
		))
	}
}

// checkSSM extracts every pms:// credential URI referenced by the
// config, deduplicates them, and verifies each is present in the
// registry. When the registry is nil but pms:// URIs exist, a single
// "SsmParamRegistry prop is required" error is emitted naming every
// offending URI.
func checkSSM(cfg *ports.BridgeConfig, reg *registry.SsmParamRegistry, emit func(string)) {
	uris := collectSSMURIs(cfg)
	if len(uris) == 0 {
		return
	}
	if reg == nil {
		emit(fmt.Sprintf(
			"yaml references SSM parameter URI(s) %s via credentials_uri fields, but no SsmParamRegistry was supplied. "+
				"Expected: a *registry.SsmParamRegistry passed via the construct's SsmParamRegistry prop. "+
				"Fix: construct registry.NewSsmParamRegistry(), call AddParameter(path, param) for each of %s, and pass it as SsmParamRegistry on the construct props.",
			quoteList(uris), quoteList(uris),
		))
		return
	}
	for _, uri := range uris {
		key := pmsURIToRegistryKey(uri)
		if reg.Has(key) {
			continue
		}
		emit(fmt.Sprintf(
			"yaml references SSM parameter %q (registry key %q) but no such entry in SsmParamRegistry. "+
				"Expected: an AddParameter(%q, param) call before the GoBridge construct is synthesised. "+
				"Fix: registry.AddParameter(%q, param) on the SsmParamRegistry passed via the construct's SsmParamRegistry prop.",
			uri, key, key, key,
		))
	}
}

// checkEndpoints aggregates every malformed bridge.cluster.endpoints
// entry. Phase 1 stops at the first; Phase 2 reports them all so a
// single synth shows the operator every URL to fix.
func checkEndpoints(cfg *ports.BridgeConfig, emit func(string)) {
	for _, k := range sortedEndpointKeys(cfg) {
		if e := parseEndpoint(k, cfg.Bridge.Cluster.Endpoints[k]); e != nil {
			emit(formatEndpointError(e))
		}
	}
}

// checkDynamoStores warns for each DynamoDB-backed store (lease,
// outbox, DLQ, managed subscriptions) that omits table_name. Such a store is
// valid — the adapter falls back to its built-in default table — but the grant
// path in gobridgebase cannot import a table it cannot name, so the
// task role receives no IAM grant and the store hits AccessDenied at
// runtime. A warning (not an error) preserves the documented escape
// hatch of granting the default table externally.
func checkDynamoStores(cfg *ports.BridgeConfig, warn func(string)) {
	roles := []struct {
		name  string
		store *ports.StoreConfig
	}{
		{"lease", cfg.Stores.Lease},
		{"outbox", cfg.Stores.Outbox},
		{"dlq", cfg.Stores.DLQ},
		{"managed_subscriptions", cfg.Stores.ManagedSubscriptions},
	}
	for _, r := range roles {
		if r.store == nil || !strings.EqualFold(r.store.Type, awsstore.DynamoDBKind) {
			continue
		}
		if dynamoTableName(r.store) != "" {
			continue
		}
		warn(fmt.Sprintf(
			"dynamodb %s store omits table_name; the adapter falls back to its built-in default table, "+
				"which this stack cannot import or grant, so the task role hits DynamoDB AccessDenied at runtime. "+
				"Expected: an explicit table_name so the stack imports the table and attaches a GrantReadWriteData policy. "+
				"Fix: set table_name on the %s store, or grant the default table to the task role externally.",
			r.name, r.name,
		))
	}
}

// dynamoTableName reads the configured DynamoDB table name from a
// store config, preferring the typed DynamoDBConfig and falling back
// to a raw probe. Returns "" when unset. Mirrors the resolver in
// gobridgebase.grants (kept local so validation does not import the
// internal construct package).
func dynamoTableName(sc *ports.StoreConfig) string {
	if dc, ok := sc.Config.(*awsstore.DynamoDBConfig); ok {
		return dc.TableName
	}
	if raw := sc.Raw(); raw != nil {
		var probe struct {
			TableName string `yaml:"table_name" json:"table_name" mapstructure:"table_name"`
		}
		_ = raw.Decode(&probe)
		return probe.TableName
	}
	return ""
}

// collectSQSQueueNames walks Receivers and Senders, deduplicates the
// non-empty QueueName values from SQS-typed configs, and returns them
// in sorted order. Defs that supply QueueURL directly are skipped:
// an explicit URL bypasses the registry by design.
func collectSQSQueueNames(cfg *ports.BridgeConfig) []string {
	seen := map[string]struct{}{}
	add := func(name string) {
		if name == "" {
			return
		}
		seen[name] = struct{}{}
	}
	for i := range cfg.Receivers {
		r := &cfg.Receivers[i]
		if r.Transport != sqsTransport {
			continue
		}
		if c, ok := r.Config.(*sqs.Config); ok && c != nil && c.QueueURL == "" {
			add(c.QueueName)
		}
	}
	for i := range cfg.Senders {
		s := &cfg.Senders[i]
		if s.Transport != sqsTransport {
			continue
		}
		if c, ok := s.Config.(*sqs.Config); ok && c != nil && c.QueueURL == "" {
			add(c.QueueName)
		}
	}
	return sortedKeys(seen)
}

// collectSSMURIs walks every plugin payload that may carry a
// credentials_uri (sessions, receivers, senders, bindings, stores)
// and returns the deduplicated, sorted list of pms:// URIs found.
// Non-pms schemes are ignored — only the SSM registry is in scope.
func collectSSMURIs(cfg *ports.BridgeConfig) []string {
	seen := map[string]struct{}{}
	add := func(pc ports.PluginConfig) {
		uri := credentialsURI(pc)
		if uri == "" || !strings.HasPrefix(uri, pmsScheme) {
			return
		}
		seen[uri] = struct{}{}
	}
	for i := range cfg.Sessions {
		add(cfg.Sessions[i].Config)
	}
	for i := range cfg.Receivers {
		add(cfg.Receivers[i].Config)
	}
	for i := range cfg.Senders {
		add(cfg.Senders[i].Config)
	}
	for i := range cfg.Bindings {
		add(cfg.Bindings[i].Config)
	}
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions} {
		if sc != nil {
			add(sc.Config)
		}
	}
	return sortedKeys(seen)
}

// credentialsURI extracts CredentialsURI() when pc satisfies
// ports.CredentialedConfig, otherwise the empty string. A nil pc is
// treated as "no URI"; the type assertion handles the typed-nil case
// because PluginConfig is an interface.
func credentialsURI(pc ports.PluginConfig) string {
	if pc == nil {
		return ""
	}
	cc, ok := pc.(ports.CredentialedConfig)
	if !ok {
		return ""
	}
	return cc.CredentialsURI()
}

// pmsURIToRegistryKey converts a pms://host/path URI into the
// "/host/path" form used as the SsmParamRegistry key. Mirrors the
// reverse mapping in bridgecfg.paramRefToPMS.
func pmsURIToRegistryKey(uri string) string {
	rest := strings.TrimPrefix(uri, pmsScheme)
	if rest == "" {
		return "/"
	}
	if strings.HasPrefix(rest, "/") {
		// pms:///abs/path → already absolute; strip the empty host.
		return rest
	}
	return "/" + rest
}

// sortedKeys returns the keys of a string-set in deterministic order.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quoteList renders names as `["a", "b", "c"]` for inclusion in
// human-readable Annotation messages.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
