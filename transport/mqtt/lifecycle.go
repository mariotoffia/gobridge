package mqtt

import (
	"context"
	"fmt"
	"sync"

	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// mqttLifecycleCoordinator manages atomic lifecycle changes for MQTTConnection.
type mqttLifecycleCoordinator struct {
	conn *MQTTConnection
	mu   sync.Mutex
}

func newMQTTLifecycleCoordinator(conn *MQTTConnection) *mqttLifecycleCoordinator {
	return &mqttLifecycleCoordinator{conn: conn}
}

// BeginTransaction starts an atomic change operation.
func (c *mqttLifecycleCoordinator) BeginTransaction(ctx context.Context) (types.LifecycleTransaction, error) {
	c.mu.Lock()
	return &mqttLifecycleTransaction{
		coordinator: c,
		ctx:         ctx,
		addSources:  make([]types.SourceConfig, 0),
		addTargets:  make([]types.TargetConfig, 0),
	}, nil
}

// mqttLifecycleTransaction represents an atomic set of changes.
type mqttLifecycleTransaction struct {
	coordinator   *mqttLifecycleCoordinator
	ctx           context.Context
	addSources    []types.SourceConfig
	removeSources []string
	updateSources []sourceUpdate
	addTargets    []types.TargetConfig
	removeTargets []string
	updateTargets []targetUpdate
	committed     bool
	rolledBack    bool
}

type sourceUpdate struct {
	id     string
	config types.SourceConfig
}

type targetUpdate struct {
	id     string
	config types.TargetConfig
}

// AddSource schedules a source to be added.
func (t *mqttLifecycleTransaction) AddSource(config types.SourceConfig) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.addSources = append(t.addSources, config)
	return nil
}

// RemoveSource schedules a source to be removed.
func (t *mqttLifecycleTransaction) RemoveSource(sourceID string) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.removeSources = append(t.removeSources, sourceID)
	return nil
}

// UpdateSource schedules a source to be updated (remove + add).
func (t *mqttLifecycleTransaction) UpdateSource(sourceID string, config types.SourceConfig) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.updateSources = append(t.updateSources, sourceUpdate{id: sourceID, config: config})
	return nil
}

// AddTarget schedules a target to be added.
func (t *mqttLifecycleTransaction) AddTarget(config types.TargetConfig) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.addTargets = append(t.addTargets, config)
	return nil
}

// RemoveTarget schedules a target to be removed.
func (t *mqttLifecycleTransaction) RemoveTarget(targetID string) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.removeTargets = append(t.removeTargets, targetID)
	return nil
}

// UpdateTarget schedules a target to be updated (remove + add).
func (t *mqttLifecycleTransaction) UpdateTarget(targetID string, config types.TargetConfig) error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.updateTargets = append(t.updateTargets, targetUpdate{id: targetID, config: config})
	return nil
}

// Commit applies all scheduled changes atomically.
func (t *mqttLifecycleTransaction) Commit(ctx context.Context) (*types.LifecycleChangeResult, error) {
	if t.committed || t.rolledBack {
		return nil, fmt.Errorf("transaction already completed")
	}
	t.committed = true
	defer t.coordinator.mu.Unlock()

	result := &types.LifecycleChangeResult{
		AddedSources:   make([]types.Source, 0),
		RemovedSources: make([]string, 0),
		AddedTargets:   make([]types.Target, 0),
		RemovedTargets: make([]string, 0),
		Errors:         make([]error, 0),
	}

	conn := t.coordinator.conn

	// 1. Collect all topics to unsubscribe
	unsubscribeTopics := t.collectUnsubscribeTopics(conn)

	// 2. Collect all topics to subscribe
	subscribeOptions := t.collectSubscribeOptions()

	// 3. Perform unsubscribe atomically
	if len(unsubscribeTopics) > 0 {
		if err := t.performUnsubscribe(ctx, conn, unsubscribeTopics); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("unsubscribe failed: %w", err))
			// Continue with other operations
		}
	}

	// 4. Remove sources and close them
	for _, srcID := range t.removeSources {
		conn.mu.RLock()
		src, exists := conn.activeSources[srcID]
		conn.mu.RUnlock()

		if exists {
			if err := src.Close(); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to close source %s: %w", srcID, err))
			}
			conn.unregisterSource(srcID)
			result.RemovedSources = append(result.RemovedSources, srcID)
		}
	}

	// 5. Handle source updates (remove old, add new)
	for _, update := range t.updateSources {
		conn.mu.RLock()
		src, exists := conn.activeSources[update.id]
		conn.mu.RUnlock()

		if exists {
			if err := src.Close(); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to close source %s: %w", update.id, err))
			}
			conn.unregisterSource(update.id)
			result.RemovedSources = append(result.RemovedSources, update.id)
		}

		// Add the updated source
		t.addSources = append(t.addSources, update.config)
	}

	// 6. Perform subscribe atomically
	if len(subscribeOptions) > 0 {
		if err := t.performSubscribe(ctx, conn, subscribeOptions); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("subscribe failed: %w", err))
			// Continue with other operations
		}
	}

	// 7. Create new sources
	for _, config := range t.addSources {
		src, err := conn.CreateSource(ctx, config)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to create source: %w", err))
			continue
		}

		// Start the source
		if err := src.Start(ctx); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to start source: %w", err))
			_ = src.Close()
			continue
		}

		result.AddedSources = append(result.AddedSources, src)
	}

	// 8. Remove targets
	for _, tgtID := range t.removeTargets {
		conn.mu.RLock()
		tgt, exists := conn.activeTargets[tgtID]
		conn.mu.RUnlock()

		if exists {
			if err := tgt.Close(); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to close target %s: %w", tgtID, err))
			}
			conn.unregisterTarget(tgtID)
			result.RemovedTargets = append(result.RemovedTargets, tgtID)
		}
	}

	// 9. Handle target updates
	for _, update := range t.updateTargets {
		conn.mu.RLock()
		tgt, exists := conn.activeTargets[update.id]
		conn.mu.RUnlock()

		if exists {
			if err := tgt.Close(); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to close target %s: %w", update.id, err))
			}
			conn.unregisterTarget(update.id)
			result.RemovedTargets = append(result.RemovedTargets, update.id)
		}

		// Add the updated target
		t.addTargets = append(t.addTargets, update.config)
	}

	// 10. Create new targets
	for _, config := range t.addTargets {
		tgt, err := conn.CreateTarget(ctx, config)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to create target: %w", err))
			continue
		}
		result.AddedTargets = append(result.AddedTargets, tgt)
	}

	return result, nil
}

// Rollback cancels the transaction without applying changes.
func (t *mqttLifecycleTransaction) Rollback() error {
	if t.committed || t.rolledBack {
		return fmt.Errorf("transaction already completed")
	}
	t.rolledBack = true
	t.coordinator.mu.Unlock()
	return nil
}

// collectUnsubscribeTopics collects all topics that need to be unsubscribed.
func (t *mqttLifecycleTransaction) collectUnsubscribeTopics(conn *MQTTConnection) []string {
	var topics []string

	conn.mu.RLock()
	defer conn.mu.RUnlock()

	// Collect topics from sources being removed
	for _, srcID := range t.removeSources {
		if src, ok := conn.activeSources[srcID]; ok {
			if mqttSrc, ok := src.(*Source); ok {
				topics = append(topics, mqttSrc.getTopics()...)
			}
		}
	}

	// Collect topics from sources being updated
	for _, update := range t.updateSources {
		if src, ok := conn.activeSources[update.id]; ok {
			if mqttSrc, ok := src.(*Source); ok {
				topics = append(topics, mqttSrc.getTopics()...)
			}
		}
	}

	return topics
}

// collectSubscribeOptions collects all subscription options for new sources.
func (t *mqttLifecycleTransaction) collectSubscribeOptions() []paho.SubscribeOptions {
	var options []paho.SubscribeOptions

	for _, config := range t.addSources {
		mqttConfig, ok := config.(*SourceConfigImpl)
		if !ok {
			continue
		}

		qos := byte(mqttConfig.QoS)
		// Clamp to valid QoS range (0-2)
		if qos > 2 {
			qos = 1
		}

		for _, topic := range mqttConfig.Topics {
			options = append(options, paho.SubscribeOptions{
				Topic: topic,
				QoS:   qos,
			})
		}
	}

	return options
}

// performUnsubscribe unsubscribes from topics.
func (t *mqttLifecycleTransaction) performUnsubscribe(ctx context.Context, conn *MQTTConnection, topics []string) error {
	if conn.client == nil {
		return fmt.Errorf("client not connected")
	}

	_, err := conn.client.Unsubscribe(ctx, &paho.Unsubscribe{
		Topics: topics,
	})

	return err
}

// performSubscribe subscribes to topics.
func (t *mqttLifecycleTransaction) performSubscribe(ctx context.Context, conn *MQTTConnection, options []paho.SubscribeOptions) error {
	if conn.client == nil {
		return fmt.Errorf("client not connected")
	}

	_, err := conn.client.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: options,
	})

	return err
}

// Ensure interfaces are implemented
var _ types.LifecycleCoordinator = (*mqttLifecycleCoordinator)(nil)
var _ types.LifecycleTransaction = (*mqttLifecycleTransaction)(nil)
