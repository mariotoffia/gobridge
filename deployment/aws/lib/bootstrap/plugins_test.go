package bootstrap

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// linkedOptionalKinds names the config kinds the optional families compiled
// into THIS build contribute. Each tagged family test file adds its own kinds
// in init, so the absence assertions below cover exactly the families this
// build left out — every build asserts something, whatever tags it carries.
//
// The map is written only from init in test files; nothing mutates it once
// the tests run.
var linkedOptionalKinds = map[string]bool{}

// optionalFamilyKinds is every kind an optional family can add. The profile
// base set (mqtt, sqs, native stores, http) is always linked and is covered
// by the alias assertions in the factory-registry wiring tests.
func optionalFamilyKinds() []string {
	return []string{
		"amqp091", "amqp.amqp091",
		"amqp10", "amqp.amqp10",
		"servicebus", "azure.servicebus",
	}
}

// TestPluginRegistry_DecodesNoUnselectedOptionalKind pins the compile-time
// contract from the config side: a kind whose family was not selected must
// stay undecodable, so a config naming it fails loudly at parse instead of
// booting a binary that cannot serve it.
func TestPluginRegistry_DecodesNoUnselectedOptionalKind(t *testing.T) {
	kinds := newDefaultPluginRegistry().Kinds()

	for _, kind := range optionalFamilyKinds() {
		if linkedOptionalKinds[kind] {
			continue
		}
		assert.NotContains(t, kinds, kind,
			"kind %q decodes although its family was not selected at build time", kind)
	}
}

// TestFactoryRegistry_WiresNoUnselectedOptionalTransport is the same contract
// from the factory side: an unselected family must contribute no transport
// factory, so the two seams cannot drift apart.
func TestFactoryRegistry_WiresNoUnselectedOptionalTransport(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	for _, kind := range optionalFamilyKinds() {
		if linkedOptionalKinds[kind] {
			continue
		}
		assert.NotContains(t, reg.transports, kind,
			"transport %q is wired although its family was not selected at build time", kind)
	}
}

// TestPluginKindsLog_NamesExactlyWhatThisBuildDecodes pins the one observable
// difference between an image built with a family tag and one built without.
// Without it an operator meeting "unknown plugin kind amqp091" cannot tell a
// missing family from a typo.
func TestPluginKindsLog_NamesExactlyWhatThisBuildDecodes(t *testing.T) {
	var buf bytes.Buffer

	logPluginKinds(slog.New(slog.NewJSONHandler(&buf, nil)), newDefaultPluginRegistry())

	// Decode the logged array rather than search the text: a qualified alias
	// such as "amqp.amqp091" contains its short kind, so a substring match
	// would let one discriminator stand in for the other.
	var line struct {
		Kinds []string `json:"kinds"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))

	for _, kind := range []string{"mqtt", "mqtt.paho", "sqs", "aws.sqs", "http", "memory", "sqlite", "dynamodb"} {
		assert.Contains(t, line.Kinds, kind, "the base set is always linked and must always be named")
	}
	for _, kind := range optionalFamilyKinds() {
		if linkedOptionalKinds[kind] {
			assert.Contains(t, line.Kinds, kind)
		} else {
			assert.NotContains(t, line.Kinds, kind)
		}
	}
}
