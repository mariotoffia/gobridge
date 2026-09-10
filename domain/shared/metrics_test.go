package shared_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMetricConstants_TransportAgnostic enforces Finding 12's invariant: the
// shared kernel (domain/shared/metrics.go) must contain ONLY generic,
// transport-agnostic metric names. A contributor must not be able to add a
// provider-flavored metric (e.g. MetricSQSPolls = "SQSPolls") to the shared
// kernel and keep lint+test green.
//
// MetricMQTTReconnects is the sole allow-listed exception: it carries the
// historical "MQTTReconnects" wire value for observability compatibility, and
// is emitted by the generic runtime session manager (runtime/session), not by
// the MQTT adapter. See metrics.go for the wire-compat rationale.
func TestMetricConstants_TransportAgnostic(t *testing.T) {
	tokens := []string{
		"SQS", "Azure", "ServiceBus", "AMQP", "Kafka", "Kinesis",
		"Dynamo", "SNS", "Paho", "RabbitMQ", "MQTT",
	}

	// allowed values exempt from the token scan (historical wire-compat).
	allowed := map[string]bool{
		shared.MetricMQTTReconnects: true,
	}

	values := []string{
		shared.MetricLeaseAcquireLatency,
		shared.MetricLeaseRenewLatency,
		shared.MetricLeaseAcquireFailures,
		shared.MetricLeaseExpiries,
		shared.MetricLeaseTransfers,
		shared.MetricOutboxPersistLatency,
		shared.MetricOutboxDrainLatency,
		shared.MetricOutboxDepth,
		shared.MetricOutboxClaimBatchSize,
		shared.MetricOutboxDepthFailures,
		shared.MetricOutboxClaimRecoveries,
		shared.MetricOutboxCompletions,
		shared.MetricOutboxExpiredBeforeSend,
		shared.MetricOutboxReplayCount,
		shared.MetricOutboxRecordFailures,
		shared.MetricOutboxDuplicateRisk,
		shared.MetricOutboxClaimConflicts,
		shared.MetricAckLatency,
		shared.MetricVisibilityExtensions,
		shared.MetricDeliveryE2ELatency,
		shared.MetricDLQEntries,
		shared.MetricDLQDepth,
		shared.MetricDLQWriteFailures,
		shared.MetricDeliveryPanics,
		shared.MetricMessagesReceived,
		shared.MetricMessagesSent,
		shared.MetricMessagesDropped,
		shared.MetricRouteErrors,
		shared.MetricReceiveCountUnparseable,
		shared.MetricProcessorPanics,
		shared.MetricProcessorTimeouts,
		shared.MetricMQTTReconnects,
		shared.MetricReconcileFailures,
		shared.MetricSessionRestarts,
	}

	for _, v := range values {
		if allowed[v] {
			continue
		}
		lower := strings.ToLower(v)
		for _, tok := range tokens {
			if strings.Contains(lower, strings.ToLower(tok)) {
				t.Errorf("shared metric %q contains transport/provider token %q; "+
					"the shared kernel must hold only generic, transport-agnostic metrics", v, tok)
			}
		}
	}
}

// TestMetricNamespace_NonEmpty validates that MetricNamespace is a non-empty string.
func TestMetricNamespace_NonEmpty(t *testing.T) {
	if shared.MetricNamespace == "" {
		t.Fatal("MetricNamespace must not be empty")
	}
}

// TestMetricConstants_NonEmpty validates that every Metric* name
// constant is non-empty and unique across the entire set.
//
// The constants are DISCOVERED from metrics.go's AST rather than
// mirrored into a literal slice here. A hand-kept list has a silent
// blind spot: adding `MetricFoo = "Foo"` and forgetting to extend the
// slice leaves the set unchecked and nothing fails. (That is not
// hypothetical — the literal this replaced had drifted to 34 of the 57
// declared constants, so 23 were never checked for collisions.)
//
// go test runs a package's binary with the working directory set to the
// package directory, so metrics.go is right here.
func TestMetricConstants_NonEmpty(t *testing.T) {
	// MetricNamespace is the CloudWatch namespace, not a metric name.
	exempt := map[string]struct{}{"MetricNamespace": {}}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "metrics.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse metrics.go: %v", err)
	}

	seen := make(map[string]string) // value -> declaring constant
	found := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "Metric") {
				continue
			}
			if _, skip := exempt[name]; skip {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", name, lit.Value, err)
			}
			found++

			if value == "" {
				t.Errorf("%s: metric constant must not be empty", name)
			}
			if prev, dup := seen[value]; dup {
				t.Errorf("%s and %s both declare the metric name %q; "+
					"colliding names merge into one series at the exporter", prev, name, value)
			}
			seen[value] = name
		}
	}

	// Guard against the discovery itself silently breaking (a renamed
	// file, a refactor to typed constants) and reporting a clean zero.
	if found < 30 {
		t.Fatalf("discovered only %d Metric* constants in metrics.go; "+
			"the AST scan is probably no longer matching the declarations", found)
	}
}

// TestTagKeyConstants_NonEmpty validates that all TagKey* constants are non-empty and unique.
func TestTagKeyConstants_NonEmpty(t *testing.T) {
	tagKeys := []string{
		shared.TagKeyLeaseID,
		shared.TagKeyRouteID,
		shared.TagKeySessionID,
		shared.TagKeyPartition,
		shared.TagKeyCategory,
		shared.TagKeyTransport,
		shared.TagKeyEntity,
		shared.TagKeyProcessor,
	}

	seen := make(map[string]bool, len(tagKeys))
	for _, k := range tagKeys {
		if k == "" {
			t.Fatal("tag key constant must not be empty")
		}
		if seen[k] {
			t.Fatalf("duplicate tag key constant: %q", k)
		}
		seen[k] = true
	}
}

// TestTag_Construction validates Tag struct creation with Key and Value fields.
func TestTag_Construction(t *testing.T) {
	tag := shared.Tag{Key: "route_id", Value: "r1"}
	if tag.Key != "route_id" {
		t.Fatalf("Tag.Key: got %q, want %q", tag.Key, "route_id")
	}
	if tag.Value != "r1" {
		t.Fatalf("Tag.Value: got %q, want %q", tag.Value, "r1")
	}
}
