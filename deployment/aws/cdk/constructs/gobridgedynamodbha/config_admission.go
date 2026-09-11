package gobridgedynamodbha

// Synth-time admission for the DynamoDB-coordinated HA profile: everything the
// shared config document must already say before this construct will provision a
// cohort for it. It runs against the SAME materialized document the task
// definitions are built from, so nothing can pass here and differ at boot.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// inspectedHAConfig is what admission learned from the document: the
// deployment-owned table identities, the one failover objective every managed
// route agrees on, and the attested managed-subscription baselines.
type inspectedHAConfig struct {
	tables                       dataTableNames
	failoverObjective            time.Duration
	managedSubscriptionBaselines []managedSubscriptionBaseline
}

func inspectHAConfig(
	cfg *ports.BridgeConfig,
	declaredBaselines map[string][]string,
	slots *MemberSlots,
) (inspectedHAConfig, error) {
	if cfg == nil {
		return inspectedHAConfig{}, fmt.Errorf("bridge config is nil")
	}
	if err := config.Validate(cfg); err != nil {
		return inspectedHAConfig{}, err
	}
	if cfg.Bridge.DeploymentMode != "clustered" {
		return inspectedHAConfig{}, fmt.Errorf("bridge.deployment_mode must be clustered")
	}
	if cfg.Bridge.Cluster != nil && len(cfg.Bridge.Cluster.Endpoints) > 0 {
		return inspectedHAConfig{}, fmt.Errorf("bridge.cluster.endpoints must be omitted so every task advertises its ECS-resolved endpoint")
	}
	// Exactly one of these runs. Without attested static slots the facade deploys
	// interchangeable autoscaled workers and refuses a coordinated cohort outright;
	// with them it admits the cohort only when the roster and the provisioned slots
	// name the same set.
	if slots == nil {
		if err := rejectCoordinatedRollout(cfg); err != nil {
			return inspectedHAConfig{}, err
		}
	} else if err := validateMemberSlots(cfg, slots); err != nil {
		return inspectedHAConfig{}, err
	}

	lease, err := requiredDynamoDBStore("lease", cfg.Stores.Lease)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	outbox, err := requiredDynamoDBStore("outbox", cfg.Stores.Outbox)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	history, err := requiredDynamoDBStore("managed_subscriptions", cfg.Stores.ManagedSubscriptions)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	names := dataTableNames{lease: lease, outbox: outbox, managedSubscriptions: history}
	if lease == outbox || lease == history || outbox == history {
		return inspectedHAConfig{}, fmt.Errorf("lease, outbox, and managed_subscriptions must use three distinct tables")
	}
	if slots != nil {
		rollout, err := rolloutTableNameFor(cfg.Bridge.ID)
		if err != nil {
			return inspectedHAConfig{}, err
		}
		if rollout == lease || rollout == outbox || rollout == history {
			return inspectedHAConfig{}, fmt.Errorf("the derived rollout coordination table %q collides with a "+
				"configured store table; rename the store or the bridge id", rollout)
		}
		names.rollout = rollout
	}

	exclusive := map[string]string{}
	for i := range cfg.Sessions {
		session := &cfg.Sessions[i]
		if session.SessionMode != string(connectivity.SessionExclusive) {
			continue
		}
		mqtt, ok := session.Config.(*paho.Config)
		if !ok || mqtt == nil || !paho.IsKind(session.Transport) {
			return inspectedHAConfig{}, fmt.Errorf("exclusive session %q must use the MQTT Paho config", session.ID)
		}
		if err := mqtt.ValidateEffectiveSession(connectivity.SessionExclusive); err != nil {
			return inspectedHAConfig{}, fmt.Errorf("exclusive session %q is not an effective stable MQTT session: %w", session.ID, err)
		}
		storageIdentity, err := mqtt.DurableSessionIdentity(connectivity.SessionExclusive)
		if err != nil {
			return inspectedHAConfig{}, fmt.Errorf(
				"exclusive session %q durable identity: %w",
				session.ID,
				err,
			)
		}
		exclusive[session.ID] = storageIdentity
	}
	if len(exclusive) == 0 {
		return inspectedHAConfig{}, fmt.Errorf("at least one Exclusive MQTT session is required")
	}
	managedSubscriptionBaselines, err := validateManagedSubscriptionBaselines(
		exclusive,
		declaredBaselines,
	)
	if err != nil {
		return inspectedHAConfig{}, err
	}

	var objective time.Duration
	managedRoutes := 0
	coveredSessions := map[string]struct{}{}
	receiverSessions := make(map[string]string, len(cfg.Receivers))
	for i := range cfg.Receivers {
		receiverSessions[cfg.Receivers[i].ID] = cfg.Receivers[i].SessionID
	}
	bindingSessions := make(map[string]string, len(cfg.Bindings))
	for i := range cfg.Bindings {
		bindingSessions[cfg.Bindings[i].ID] = cfg.Bindings[i].SessionID
	}
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		referencedExclusive := ""
		if sessionID := receiverSessions[route.ReceiverID]; sessionID != "" {
			if _, ok := exclusive[sessionID]; ok {
				referencedExclusive = sessionID
			}
		}
		for _, bindingID := range route.Bindings {
			sessionID := bindingSessions[bindingID]
			if _, ok := exclusive[sessionID]; !ok {
				continue
			}
			if referencedExclusive != "" && referencedExclusive != sessionID {
				return inspectedHAConfig{}, fmt.Errorf("route %q references multiple Exclusive sessions", route.ID)
			}
			referencedExclusive = sessionID
		}
		if route.Session == nil {
			if referencedExclusive != "" {
				return inspectedHAConfig{}, fmt.Errorf("route %q references Exclusive session %q but has no explicit route.session failover budget", route.ID, referencedExclusive)
			}
			continue
		}
		if _, ok := exclusive[route.Session.SessionID]; !ok {
			return inspectedHAConfig{}, fmt.Errorf("route %q session %q is not an Exclusive MQTT session", route.ID, route.Session.SessionID)
		}
		if referencedExclusive != "" && referencedExclusive != route.Session.SessionID {
			return inspectedHAConfig{}, fmt.Errorf("route %q route.session %q diverges from referenced Exclusive session %q", route.ID, route.Session.SessionID, referencedExclusive)
		}
		coveredSessions[route.Session.SessionID] = struct{}{}
		managedRoutes++
		if route.DeliveryMode != "shared_outbox" {
			return inspectedHAConfig{}, fmt.Errorf("route %q must use delivery_mode: shared_outbox", route.ID)
		}
		if route.Policy.AckAfter != "outbox_persist" {
			return inspectedHAConfig{}, fmt.Errorf("route %q must use policy.ack_after: outbox_persist", route.ID)
		}
		if route.Session.FailoverSLO == "" || route.Session.StartupAllowance == "" {
			return inspectedHAConfig{}, fmt.Errorf("route %q requires explicit failover_slo and startup_allowance", route.ID)
		}
		parsed, parseErr := time.ParseDuration(route.Session.FailoverSLO)
		if parseErr != nil || parsed <= 0 {
			return inspectedHAConfig{}, fmt.Errorf("route %q has invalid failover_slo %q", route.ID, route.Session.FailoverSLO)
		}
		if objective == 0 {
			objective = parsed
		} else if parsed != objective {
			return inspectedHAConfig{}, fmt.Errorf("all coordinated routes must declare the same profile failover_slo")
		}
	}
	if managedRoutes == 0 {
		return inspectedHAConfig{}, fmt.Errorf("at least one lease-managed shared_outbox route is required")
	}
	for sessionID := range exclusive {
		if _, ok := coveredSessions[sessionID]; !ok {
			return inspectedHAConfig{}, fmt.Errorf("exclusive MQTT session %q is not covered by a lease-managed route.session", sessionID)
		}
	}

	if err := validateTask9Admission(cfg); err != nil {
		return inspectedHAConfig{}, err
	}
	return inspectedHAConfig{
		tables:                       names,
		failoverObjective:            objective,
		managedSubscriptionBaselines: managedSubscriptionBaselines,
	}, nil
}

func validateManagedSubscriptionBaselines(
	required map[string]string,
	declared map[string][]string,
) ([]managedSubscriptionBaseline, error) {
	for sessionID := range declared {
		if _, ok := required[sessionID]; !ok {
			return nil, fmt.Errorf(
				"managed subscription baseline references unknown or unmanaged session %q",
				sessionID,
			)
		}
	}

	sessionIDs := make([]string, 0, len(required))
	for sessionID := range required {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	baselines := make([]managedSubscriptionBaseline, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		filters, ok := declared[sessionID]
		if !ok {
			return nil, fmt.Errorf(
				"managed subscription baseline for Exclusive MQTT session %q is required",
				sessionID,
			)
		}
		seen := make(map[string]struct{}, len(filters))
		validatedFilters := make([]string, 0, len(filters))
		for _, filter := range filters {
			if filter == "" {
				return nil, fmt.Errorf(
					"managed subscription baseline for session %q contains an empty filter",
					sessionID,
				)
			}
			if err := paho.ValidateMQTTTopicFilter(filter); err != nil {
				return nil, fmt.Errorf(
					"managed subscription baseline for session %q contains invalid filter %q: %w",
					sessionID,
					filter,
					err,
				)
			}
			if _, duplicate := seen[filter]; duplicate {
				continue
			}
			seen[filter] = struct{}{}
			validatedFilters = append(validatedFilters, filter)
		}
		sort.Strings(validatedFilters)
		baselines = append(baselines, managedSubscriptionBaseline{
			sessionID:       sessionID,
			storageIdentity: required[sessionID],
			filters:         validatedFilters,
		})
	}
	return baselines, nil
}

func requiredDynamoDBStore(role string, store *ports.StoreConfig) (string, error) {
	if store == nil || !strings.EqualFold(store.Type, awsstore.DynamoDBKind) {
		return "", fmt.Errorf("stores.%s must use type: dynamodb", role)
	}
	cfg, ok := store.Config.(*awsstore.DynamoDBConfig)
	if !ok || cfg == nil {
		return "", fmt.Errorf("stores.%s has an incompatible DynamoDB plugin config", role)
	}
	name, err := awsstore.ResolveDynamoDBTableName(role, cfg.TableName)
	if err != nil {
		return "", err
	}
	if unresolved := awscdk.Token_IsUnresolved(name); unresolved != nil && *unresolved {
		return "", fmt.Errorf("stores.%s table_name must be a resolved physical table_name; deploy-time tokens cannot be embedded safely in the immutable config asset", role)
	}
	return name, nil
}

// validateTask9Admission reuses the runtime composition preflight rather than
// duplicating its checked budget formula. A nil SDK client keeps this synth-time
// pass source-safe: store construction and schema preflight perform no calls.
func validateTask9Admission(cfg *ports.BridgeConfig) error {
	mqttFactory := paho.NewFactory(nil, nil)
	builder := bridge.NewBuilder(cfg, bridge.WithBlueprintValidator(config.Validate)).
		RegisterTransportFactory("mqtt", mqttFactory).
		RegisterTransportFactory("mqtt.paho", mqttFactory).
		RegisterStoreFactory(awsstore.DynamoDBKind, awsstore.NewDynamoDBStoreFactory(nil))
	plan, err := builder.Plan(context.Background())
	if err != nil {
		return fmt.Errorf("task 9 failover admission rejected the profile: %w", err)
	}
	plan.Close()
	return nil
}
