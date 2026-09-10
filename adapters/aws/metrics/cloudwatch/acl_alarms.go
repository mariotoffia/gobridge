package cloudwatch

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// AlarmSeverity indicates the severity of an alarm.
type AlarmSeverity string

const (
	SeverityWarning  AlarmSeverity = "WARNING"
	SeverityCritical AlarmSeverity = "CRITICAL"
)

// metricSQSVisibilityExtensions mirrors
// adapters/aws/transport/sqs.MetricSQSVisibilityExtensions. Kept as a
// local constant because this module must not import a sibling adapter
// (TESTS.md §3.1 / .go-arch-lint.yml).
const metricSQSVisibilityExtensions = "SQSVisibilityExtensions"

// The MQTT transport's operational counters, mirrored for the same reason: this
// module must not import a sibling adapter. The shipped CDK alarm bundle alarms
// on all three DIMENSIONLESS, and the adapter emits each one tagged with the
// session, so without a rollup copy those alarms can never match a series.
const (
	metricMQTTIngressPoisonDropped = "MQTTIngressPoisonDropped"
	metricMQTTSessionTakeover      = "MQTTSessionTakeover"
	metricMQTTQoSDowngraded        = "MQTTQoSDowngraded"
)

// AlarmDefinition describes a CloudWatch alarm that EnsureAlarms will create.
type AlarmDefinition struct {
	Name        string
	MetricName  string
	Namespace   string
	Threshold   float64
	Period      int32
	EvalPeriods int32
	Statistic   cwtypes.Statistic
	Comparison  cwtypes.ComparisonOperator
	Severity    AlarmSeverity
	SNSTopicARN string
	// TreatMissingData controls how the alarm evaluates periods with no
	// datapoints ("breaching", "notBreaching", "ignore", "missing").
	// Empty defaults to "notBreaching" — correct for event counters
	// whose absence is the healthy state, WRONG for continuously
	// emitted gauges whose absence means the emitter died.
	TreatMissingData string
}

// DefaultRollupMetrics returns the metric names targeted by
// DefaultAlarms plus the silent-loss counters. Most are emitted WITH
// dimensions by the runtime (DLQEntries → route_id/category, lease
// metrics → lease_id, SQSVisibilityExtensions → queue_url, OutboxDepth →
// partition, MessagesDropped/Expired/Filtered → route_id[/reason]), and a
// CloudWatch alarm without dimensions NEVER matches dimensioned data —
// so the exporter must double-publish a zero-dimension rollup copy of
// these metrics for the default alarms to fire:
//
//	exporter, err := cloudwatch.New(ctx, shared.MetricNamespace,
//	    cloudwatch.WithRollupMetrics(cloudwatch.DefaultRollupMetrics()...),
//	)
//
// CredentialRefreshFailures and DLQDepth are the dimensionless exceptions:
// each is emitted with NO runtime dimension, so on a fleet WITH instance
// tagging (WithInstanceTag) its base series carries only instance_id and a
// zero-dimension alarm would still miss it — hence both are rolled up too.
// Without instance tagging their base and rollup copies coincide (a
// harmless double count for CredentialRefreshFailures, which has no default
// alarm; DLQDepth is a gauge whose rollup takes the fleet Maximum).
func DefaultRollupMetrics() []string {
	return []string{
		shared.MetricOutboxDepth,
		shared.MetricLeaseExpiries,
		shared.MetricDLQEntries,
		shared.MetricLeaseAcquireFailures,
		shared.MetricLeaseTransfers,
		shared.MetricOutboxDrainLatency,
		shared.MetricOutboxDepthFailures,
		shared.MetricOutboxRecordFailures,
		shared.MetricOutboxDrainStalled,
		shared.MetricDLQWriteFailures,
		shared.MetricCredentialRefreshFailures,
		metricSQSVisibilityExtensions,
		// Silent-loss + backlog counters (H-OBS). These are emitted with a
		// route/partition dimension by the runtime, so a dimensionless fleet
		// alarm never matches their base series without a zero-dimension rollup
		// copy. DLQDepth and MessagesDropped/Expired have default alarms below;
		// MessagesFiltered is rolled up too (so operators CAN alarm on an
		// unexpected filter rate) but ships no default alarm because a filter
		// discard is an intentional policy outcome, not loss.
		shared.MetricDLQDepth,
		shared.MetricMessagesDropped,
		shared.MetricMessagesExpired,
		shared.MetricMessagesFiltered,
		// Coordinated cluster rollout convergence gauges. Each is emitted with NO
		// runtime dimension, so on a fleet WITH instance tagging its base series
		// carries only instance_id and a dimensionless fleet alarm would miss it.
		// They are exactly the series a fleet alarm must read: whether ANY member is
		// off the decided generation, cannot repair itself, or has stopped being
		// able to see the rollout row at all — questions no per-instance view
		// answers, and no single member can answer about the cohort.
		shared.MetricClusterRolloutDiverged,
		shared.MetricClusterRolloutTerminal,
		shared.MetricClusterRolloutObservationAge,
		// The MQTT operational counters the CDK bundle alarms on, and the session
		// reconcile failure beside them. Each is emitted per session_id, so the
		// dimensionless alarms the bundle provisions had nothing to match: they sat
		// at INSUFFICIENT_DATA rather than reporting an acked-and-dropped poison
		// packet, a client-id collision, a broker QoS cap, or a subscription
		// reconcile that never converges.
		metricMQTTIngressPoisonDropped,
		shared.MetricReconcileFailures,
		metricMQTTSessionTakeover,
		metricMQTTQoSDowngraded,
	}
}

// DefaultAlarms returns the standard alarm set for a gobridge deployment:
// one alarm per operationally significant metric, as described in
// docs/aws-deployment/monitoring.md.
// snsTopicARN is optional; when non-empty, alarm actions are set to publish
// to the given SNS topic.
//
// The alarms carry no dimensions, so they only match the
// zero-dimension rollup series produced by
// WithRollupMetrics(DefaultRollupMetrics()...) — configure that option
// on the exporter or these alarms will never leave INSUFFICIENT_DATA
// silently. To alarm per dimension value instead (e.g. one
// alarm per route_id), create per-dimension alarms via your deployment
// tooling; AlarmDefinition deliberately models the fleet-rollup shape.
//
// TreatMissingData: the OutboxDepth alarms use "breaching" because the
// runtime emits OutboxDepth continuously while an outbox is configured
// — silence there means the drainer (or the bridge) is dead, which is
// exactly what the alarm exists to catch. Do not install the
// OutboxDepth alarms on deployments without an outbox. The event
// counters (LeaseExpiries, DLQEntries, LeaseAcquireFailures,
// SQSVisibilityExtensions) use "notBreaching": no events is the
// healthy state.
func DefaultAlarms(namespace, snsTopicARN string) []AlarmDefinition {
	if namespace == "" {
		namespace = shared.MetricNamespace
	}
	return []AlarmDefinition{
		{
			Name:             "GoBridge-OutboxDepth-Warning",
			MetricName:       shared.MetricOutboxDepth,
			Namespace:        namespace,
			Threshold:        1000,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticMaximum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "breaching",
		},
		{
			Name:             "GoBridge-OutboxDepth-Critical",
			MetricName:       shared.MetricOutboxDepth,
			Namespace:        namespace,
			Threshold:        10000,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticMaximum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityCritical,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "breaching",
		},
		{
			Name:             "GoBridge-LeaseExpiries-Warning",
			MetricName:       shared.MetricLeaseExpiries,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-DLQEntries-Warning",
			MetricName:       shared.MetricDLQEntries,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-LeaseAcquireFailures-Critical",
			MetricName:       shared.MetricLeaseAcquireFailures,
			Namespace:        namespace,
			Threshold:        3,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityCritical,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-SQSVisibilityExtensions-Warning",
			MetricName:       metricSQSVisibilityExtensions,
			Namespace:        namespace,
			Threshold:        100,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		// DLQ depth: any outstanding dead-letter entry is worth attention. A
		// gauge (Maximum), so notBreaching — absence means no DLQ store / no
		// sampler, not a healthy zero (H-OBS DLQ-1).
		{
			Name:             "GoBridge-DLQDepth-Warning",
			MetricName:       shared.MetricDLQDepth,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticMaximum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		// Terminal silent loss: a message settled WITHOUT a DLQ record and
		// WITHOUT a successful send. Any drop is critical — this is the single
		// signal for lost messages. Counter (Sum) > 0 over one period; absence
		// is the healthy state (notBreaching).
		{
			Name:             "GoBridge-MessagesDropped-Critical",
			MetricName:       shared.MetricMessagesDropped,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityCritical,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		// TTL loss: messages expiring before delivery. A trickle can be normal
		// for short-TTL traffic, so alarm on SUSTAINED expiry — Sum > 0 in each
		// of three consecutive 5-minute windows — rather than a single expiry.
		{
			Name:             "GoBridge-MessagesExpired-Warning",
			MetricName:       shared.MetricMessagesExpired,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      3,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
	}
}

// EnsureAlarms creates or updates CloudWatch alarms matching the given
// definitions. Existing alarms with the same name are updated in place.
//
// Deployment wiring note: nothing in the runtime calls EnsureAlarms —
// it is a provisioning-time API. Call it from your bootstrap/CDK-glue
// alongside exporter construction, with the SAME namespace the
// exporter publishes to and WithRollupMetrics(DefaultRollupMetrics()...)
// configured on the exporter.
func EnsureAlarms(ctx context.Context, client cloudWatchAPI, alarms []AlarmDefinition) error {
	for _, a := range alarms {
		treatMissing := a.TreatMissingData
		if treatMissing == "" {
			treatMissing = "notBreaching"
		}
		input := &cloudwatch.PutMetricAlarmInput{
			AlarmName:          aws.String(a.Name),
			MetricName:         aws.String(a.MetricName),
			Namespace:          aws.String(a.Namespace),
			Threshold:          aws.Float64(a.Threshold),
			Period:             aws.Int32(a.Period),
			EvaluationPeriods:  aws.Int32(a.EvalPeriods),
			Statistic:          a.Statistic,
			ComparisonOperator: a.Comparison,
			TreatMissingData:   aws.String(treatMissing),
			AlarmDescription:   aws.String(fmt.Sprintf("[%s] %s alarm for %s", a.Severity, a.MetricName, a.Namespace)),
		}

		if a.SNSTopicARN != "" {
			input.AlarmActions = []string{a.SNSTopicARN}
			input.OKActions = []string{a.SNSTopicARN}
		}

		if _, err := client.PutMetricAlarm(ctx, input); err != nil {
			return fmt.Errorf("cloudwatch: put metric alarm: %w", err)
		}
	}
	return nil
}
