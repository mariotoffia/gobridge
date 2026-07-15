package bootstrap

import (
	"fmt"
	"math"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type mqttMemoryAllocationResult struct {
	perSessionBudgetBytes uint64
}

// applyMQTTMemoryProfile reserves one quarter of container memory for all
// memory-profile-aware ingress sessions, divides it equally by session, and
// asks each transport config to derive or validate its receive concurrency.
// Sessions used only by senders consume no ingress allocation.
func applyMQTTMemoryProfile(cfg *ports.BridgeConfig, bootstrapCfg deployinfra.BootstrapConfig) error {
	if cfg == nil {
		return shared.ErrInvalidConfig.WithMessage("bootstrap: MQTT memory profile requires a bridge config")
	}

	profiles := make(map[string]ports.IngressMemoryProfileConfig, len(cfg.Sessions))
	for i := range cfg.Sessions {
		session := &cfg.Sessions[i]
		if profile, ok := session.Config.(ports.IngressMemoryProfileConfig); ok {
			profiles[session.ID] = profile
			continue
		}
		if _, memoryAware := session.Config.(ports.IngressMemoryConfig); memoryAware {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bootstrap: session %q ingress memory config must be mutable (use a pointer PluginConfig)",
				session.ID,
			))
		}
	}

	receiverSession := make(map[string]string, len(cfg.Receivers))
	for i := range cfg.Receivers {
		receiver := &cfg.Receivers[i]
		if _, ok := profiles[receiver.SessionID]; ok {
			receiverSession[receiver.ID] = receiver.SessionID
		}
	}
	routeConcurrency := make(map[string]uint64, len(profiles))
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		sessionID := receiverSession[route.ReceiverID]
		if sessionID == "" {
			continue
		}
		if route.Policy.MaxInFlight < 0 {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bootstrap: route %q policy.max_in_flight must not be negative", route.ID,
			))
		}
		maxInFlight := route.Policy.MaxInFlight
		if maxInFlight == 0 {
			maxInFlight = routing.DefaultMaxInFlight
		}
		current := routeConcurrency[sessionID]
		add := uint64(maxInFlight)
		if add > math.MaxUint64-current {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bootstrap: session %q route concurrency overflows MQTT memory profile", sessionID,
			))
		}
		routeConcurrency[sessionID] = current + add
	}
	if len(routeConcurrency) == 0 {
		// SQS/HTTP-only configurations and MQTT sender-only configurations do
		// not instantiate an MQTT ingress window.
		return nil
	}

	allocation, err := mqttMemoryAllocation(
		bootstrapCfg.ContainerMemoryBytes,
		bootstrapCfg.ReservedMemoryBytes,
		uint64(len(routeConcurrency)),
	)
	if err != nil {
		return err
	}
	for i := range cfg.Sessions {
		session := &cfg.Sessions[i]
		routeMaxInFlight := routeConcurrency[session.ID]
		if routeMaxInFlight == 0 {
			continue
		}
		profile := profiles[session.ID]
		if err := profile.ConfigureIngressMemory(
			allocation.perSessionBudgetBytes,
			routeMaxInFlight,
		); err != nil {
			return shared.ErrInvalidConfig.Wrap(err).WithMessage(fmt.Sprintf(
				"bootstrap: session %q MQTT memory profile is impossible", session.ID,
			))
		}
	}
	return nil
}

func mqttMemoryAllocation(
	containerMemoryBytes uint64,
	reservedMemoryBytes uint64,
	ingressSessions uint64,
) (mqttMemoryAllocationResult, error) {
	if containerMemoryBytes == 0 {
		return mqttMemoryAllocationResult{},
			shared.ErrInvalidConfig.WithMessage("bootstrap: container memory must be greater than zero")
	}
	if ingressSessions == 0 {
		return mqttMemoryAllocationResult{},
			shared.ErrInvalidConfig.WithMessage("bootstrap: MQTT memory profile requires at least one ingress session")
	}

	totalBudget := containerMemoryBytes / 4
	if totalBudget == 0 {
		return mqttMemoryAllocationResult{},
			shared.ErrInvalidConfig.WithMessage("bootstrap: container memory is too small to reserve 25% for MQTT ingress")
	}
	perSessionBudget := totalBudget / ingressSessions
	if perSessionBudget == 0 {
		return mqttMemoryAllocationResult{},
			shared.ErrInvalidConfig.WithMessage("bootstrap: MQTT ingress allocation is smaller than the ingress session count")
	}

	minimumHeadroom := containerMemoryBytes / 5
	if containerMemoryBytes%5 != 0 {
		minimumHeadroom++
	}
	allowedUsed := containerMemoryBytes - minimumHeadroom
	if totalBudget > math.MaxUint64-reservedMemoryBytes {
		return mqttMemoryAllocationResult{},
			shared.ErrInvalidConfig.WithMessage("bootstrap: configured and MQTT-reserved memory overflow integer addition")
	}
	used := reservedMemoryBytes + totalBudget
	if used > allowedUsed {
		return mqttMemoryAllocationResult{}, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"bootstrap: reserved memory %d plus MQTT ingress reservation %d leaves less than 20%% "+
				"headroom in container memory %d",
			reservedMemoryBytes, totalBudget, containerMemoryBytes,
		))
	}
	return mqttMemoryAllocationResult{
		perSessionBudgetBytes: perSessionBudget,
	}, nil
}
