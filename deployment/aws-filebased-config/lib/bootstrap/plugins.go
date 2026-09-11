package bootstrap

import (
	"errors"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

// The profile binary always links the AWS, MQTT, native-store and HTTP
// families — that is what the AWS deployment profile means. The families
// below are additive: each is selected with its own build tag
// (gobridge_amqp091, gobridge_amqp10, gobridge_azure, or gobridge_all) and
// contributes nothing to a binary built without it. Tag names, the tagged
// file plus inverse stub convention, and the kinds each family adds are the
// repo-wide ones in PLUGIN.md.
//
// The two aggregates here are the only place that enumerates the optional
// families. Both seams they feed — the decoder registry and the transport
// factory map — must list a family, or a config could decode a kind this
// binary cannot actually run.

// registerOptionalDecoders installs the PluginConfig decoders of every
// optional family selected at build time.
func registerOptionalDecoders(reg *ports.Registry) error {
	return errors.Join(
		registerAMQP091Decoders(reg),
		registerAMQP10Decoders(reg),
		registerAzureDecoders(reg),
	)
}

// wireOptionalTransports adds the transport factories of every optional
// family selected at build time to the factory map, before the map is
// registered on the builder.
func wireOptionalTransports(
	transports map[string]ports.TransportFactory,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
) {
	wireAMQP091Transports(transports, logger, metrics)
	wireAMQP10Transports(transports, logger, metrics)
	wireAzureTransports(transports, logger, metrics)
}

// logPluginKinds records the config kinds this binary can decode. An image
// built with a family tag is otherwise indistinguishable from one built
// without: the compiled families are a property of the binary, invisible to
// every runtime setting. Naming the kinds once at startup is what lets an
// operator meeting "unknown plugin kind" tell a missing family apart from a
// typo. Registry.Kinds is already sorted.
func logPluginKinds(logger *slog.Logger, reg *ports.Registry) {
	if logger == nil || reg == nil {
		return
	}
	logger.Info("bootstrap: decodable plugin kinds", "kinds", reg.Kinds())
}
