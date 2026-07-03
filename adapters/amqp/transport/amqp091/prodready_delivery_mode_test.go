// ═══════════════════════════════════════════════
// Production-readiness remediation tests: publish persistence (F1).
//
// Before this fix every publish went out with the unset delivery mode
// (0 = transient): a confirmed message only existed in broker memory,
// so a broker restart silently lost every in-queue bridged message on
// a durable classic queue — after the bridge had already acked the
// source. These tests pin:
//
//   - the persistent-by-default contract of envelopeToPublishing,
//   - the sender.delivery_mode knob (config, options map, validation),
//   - the per-message "amqp091.delivery-mode" header override accepting
//     the int/float/string shapes YAML/JSON headers actually produce.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestDeliveryModeFromHeader_Coercion pins the accepted per-message
// override shapes: typed uint8 (loop-back), int/int64/float64 (YAML and
// JSON decoded headers), and the symbolic strings. Anything else is
// rejected so the configured default applies.
func TestDeliveryModeFromHeader_Coercion(t *testing.T) {
	tests := []struct {
		name   string
		value  any
		want   uint8
		wantOK bool
	}{
		{"uint8-persistent", uint8(2), amqp.Persistent, true},
		{"uint8-transient", uint8(1), amqp.Transient, true},
		{"int-persistent", int(2), amqp.Persistent, true},
		{"int64-transient", int64(1), amqp.Transient, true},
		{"int32-persistent", int32(2), amqp.Persistent, true},
		{"uint64-persistent", uint64(2), amqp.Persistent, true},
		{"float64-persistent", float64(2), amqp.Persistent, true},
		{"float64-transient", float64(1), amqp.Transient, true},
		{"float32-persistent", float32(2), amqp.Persistent, true},
		{"string-numeric-persistent", "2", amqp.Persistent, true},
		{"string-numeric-transient", "1", amqp.Transient, true},
		{"string-word-persistent", "persistent", amqp.Persistent, true},
		{"string-word-transient", "transient", amqp.Transient, true},
		{"string-word-upper", "PERSISTENT", amqp.Persistent, true},
		{"string-word-padded", " persistent ", amqp.Persistent, true},
		{"nil", nil, 0, false},
		{"invalid-int", int(3), 0, false},
		{"invalid-zero", int(0), 0, false},
		{"invalid-negative", int(-1), 0, false},
		{"invalid-fractional-float", float64(1.5), 0, false},
		{"invalid-string", "durable", 0, false},
		{"invalid-bool", true, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := deliveryModeFromHeader(tt.value)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestEnvelopeToPublishing_DeliveryMode_DefaultsPersistent pins the
// F1 core contract: with no knob and no header override, published
// messages are PERSISTENT so they survive a destination-broker restart
// on a durable queue.
func TestEnvelopeToPublishing_DeliveryMode_DefaultsPersistent(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("x")})

	cfg := SenderConfig{}
	cfg.applyDefaults()
	require.Equal(t, DeliveryModePersistent, cfg.DeliveryMode,
		"applyDefaults must default delivery_mode to persistent")

	pub := envelopeToPublishing(env, cfg, nil)
	require.Equal(t, amqp.Persistent, pub.DeliveryMode)
}

// TestEnvelopeToPublishing_DeliveryMode_TransientKnob pins the opt-in
// transient knob.
func TestEnvelopeToPublishing_DeliveryMode_TransientKnob(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m2", Payload: []byte("x")})
	cfg := SenderConfig{DeliveryMode: DeliveryModeTransient}
	cfg.applyDefaults()

	pub := envelopeToPublishing(env, cfg, nil)
	require.Equal(t, amqp.Transient, pub.DeliveryMode)
}

// TestEnvelopeToPublishing_DeliveryMode_HeaderOverridesKnob pins the
// per-message override precedence, including the YAML/JSON int shape
// that the old uint8-only type assertion silently dropped.
func TestEnvelopeToPublishing_DeliveryMode_HeaderOverridesKnob(t *testing.T) {
	tests := []struct {
		name   string
		header any
		knob   string
		want   uint8
	}{
		{"typed-uint8-transient-over-persistent-default", uint8(1), "", amqp.Transient},
		{"yaml-int-transient-over-persistent-default", int(1), "", amqp.Transient},
		{"json-float-persistent-over-transient-knob", float64(2), DeliveryModeTransient, amqp.Persistent},
		{"string-persistent-over-transient-knob", "persistent", DeliveryModeTransient, amqp.Persistent},
		{"invalid-header-falls-back-to-default", "bogus", "", amqp.Persistent},
		{"invalid-header-falls-back-to-transient-knob", int(9), DeliveryModeTransient, amqp.Transient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "m3",
				Payload: []byte("x"),
				Headers: map[string]any{HeaderDeliveryMode: tt.header},
			})
			cfg := SenderConfig{DeliveryMode: tt.knob}
			cfg.applyDefaults()

			pub := envelopeToPublishing(env, cfg, nil)
			require.Equal(t, tt.want, pub.DeliveryMode)
		})
	}
}

// TestConfigValidate_DeliveryMode pins the knob's validation surface:
// empty and the two canonical values pass, anything else fails.
func TestConfigValidate_DeliveryMode(t *testing.T) {
	base := func(mode string) Config {
		return Config{
			Session: SessionOptions{BrokerURL: "amqp://localhost:5672/"},
			Sender:  SenderParams{Exchange: "ex", DeliveryMode: mode},
		}
	}

	require.NoError(t, base("").Validate())
	require.NoError(t, base(DeliveryModePersistent).Validate())
	require.NoError(t, base(DeliveryModeTransient).Validate())

	err := base("durable").Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "delivery_mode")
}

// TestSenderConfigFromOptions_DeliveryMode pins the options-map path.
func TestSenderConfigFromOptions_DeliveryMode(t *testing.T) {
	cfg := SenderConfigFromOptions(map[string]any{"delivery_mode": "transient"})
	require.Equal(t, DeliveryModeTransient, cfg.DeliveryMode)

	cfg = SenderConfigFromOptions(nil)
	cfg.applyDefaults()
	require.Equal(t, DeliveryModePersistent, cfg.DeliveryMode)
}
