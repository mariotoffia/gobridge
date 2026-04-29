package config

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════
// S4: Clustered MQTT Shared Subscription Validation
//
// Validates that MQTT receivers in clustered mode require either
// $share/ topic prefixes (MQTT v5 shared subscriptions) or an
// exclusive session to prevent N-fold message duplication.
//
// Decision tree:
//   clustered?
//   ├── no  → pass (no check needed)
//   └── yes
//       └── MQTT receiver?
//           ├── no  → pass (SQS etc. have native competing consumers)
//           └── yes
//               └── exclusive session?
//                   ├── yes → pass (lease ensures single subscriber)
//                   └── no
//                       └── all topics $share/?
//                           ├── yes → pass (broker distributes)
//                           └── no  → ERROR
// ═══════════════════════════════════════════════════════════════════

func clusteredMQTTConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-sess", Transport: "mqtt", SessionMode: "persistent"},
		},
		Receivers: []ports.ReceiverDef{
			{
				ID:        "rx-mqtt",
				SessionID: "mqtt-sess",
				Topics: []ports.SubscriptionDef{
					{Topic: "devices/temperature", QoS: 1},
				},
			},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue-url"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx-mqtt", Bindings: []string{"b1"}},
		},
	}
}

// TestValidate_ClusteredMQTTWithoutSharePrefix_Error validates that a
// clustered MQTT receiver with a bare topic and non-exclusive session
// is rejected to prevent N-fold message duplication.
func TestValidate_ClusteredMQTTWithoutSharePrefix_Error(t *testing.T) {
	cfg := clusteredMQTTConfig()

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered MQTT receiver requires $share/ topic prefix")
	assert.Contains(t, err.Error(), "devices/temperature")
}

// TestValidate_ClusteredMQTTWithSharePrefix_OK validates that a clustered
// MQTT receiver with a properly formatted $share/ topic passes validation.
func TestValidate_ClusteredMQTTWithSharePrefix_OK(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Topics[0].Topic = "$share/mygroup/devices/temperature"

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_ClusteredMQTTExclusiveSession_OK validates that an exclusive
// session bypasses the $share/ requirement because the lease mechanism
// ensures only one subscriber.
func TestValidate_ClusteredMQTTExclusiveSession_OK(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Sessions[0].SessionMode = "exclusive"

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_StandaloneMode_NoCheck validates that standalone mode skips
// the shared subscription check entirely since there is only one instance.
func TestValidate_StandaloneMode_NoCheck(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Bridge.DeploymentMode = "standalone"

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_ClusteredNonMQTT_NoCheck validates that non-MQTT receivers
// (e.g. SQS) skip the check because they have native competing consumers.
func TestValidate_ClusteredNonMQTT_NoCheck(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1", DeploymentMode: "clustered"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-sqs", Transport: "sqs", Topics: []ports.SubscriptionDef{
				{Topic: "my-queue"},
			}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "topic/a"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx-sqs", Bindings: []string{"b1"}},
		},
	}

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_SharedTopicMalformed_Error validates that $share/ topics
// with empty group or missing topic portion are rejected.
//
// ═══════════════════════════════════════════════════════════════════
// Cases:
//
//	"$share//devices/temp"  → empty group  → ERROR
//	"$share/mygroup"        → no topic     → ERROR
//	"$share/mygroup/"       → empty topic  → ERROR
//
// ═══════════════════════════════════════════════════════════════════
func TestValidate_SharedTopicMalformed_Error(t *testing.T) {
	cases := []struct {
		name  string
		topic string
	}{
		{"empty_group", "$share//devices/temp"},
		{"no_topic_segment", "$share/mygroup"},
		{"empty_topic_after_slash", "$share/mygroup/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := clusteredMQTTConfig()
			cfg.Receivers[0].Topics[0].Topic = tc.topic

			err := Validate(cfg)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "malformed $share/ topic")
		})
	}
}

// TestValidate_MixedTopics_OneBareTopic_Error validates that when a receiver
// has a mix of $share/ and bare topics, the bare topic triggers an error
// while the valid $share/ topic does not.
func TestValidate_MixedTopics_OneBareTopic_Error(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Topics = []ports.SubscriptionDef{
		{Topic: "$share/mygroup/devices/temperature", QoS: 1},
		{Topic: "devices/humidity", QoS: 1},
	}

	err := Validate(cfg)

	require.Error(t, err)
	ve := err.(*ValidationError)
	require.Len(t, ve.Errors, 1)
	assert.Contains(t, ve.Errors[0], "topics[1]")
	assert.Contains(t, ve.Errors[0], `"devices/humidity"`)
}

// TestValidate_ClusteredMQTTTransportFromSession_Error validates that when
// the receiver has no explicit transport but inherits it from the session,
// the MQTT check still fires.
func TestValidate_ClusteredMQTTTransportFromSession_Error(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Transport = ""

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered MQTT receiver requires $share/ topic prefix")
}

// TestValidate_EmptyDeploymentMode_NoCheck validates that when deployment_mode
// is empty (default), the shared subscription check is skipped.
func TestValidate_EmptyDeploymentMode_NoCheck(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Bridge.DeploymentMode = ""

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_ClusteredMQTTMultiLevelSharedTopic_OK validates that $share/
// topics with multi-level topic portions are accepted.
func TestValidate_ClusteredMQTTMultiLevelSharedTopic_OK(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Topics[0].Topic = "$share/factory-a/devices/+/temperature"

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_ClusteredMQTTExplicitTransportNoSession_Error validates that
// a receiver with explicit transport "mqtt" and no session_id still has
// its topics checked (sessionMode is empty, not exclusive).
func TestValidate_ClusteredMQTTExplicitTransportNoSession_Error(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1", DeploymentMode: "clustered"},
		Receivers: []ports.ReceiverDef{
			{
				ID:        "rx-mqtt-nosess",
				Transport: "mqtt",
				Topics:    []ports.SubscriptionDef{{Topic: "devices/temp", QoS: 1}},
			},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue-url"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx-mqtt-nosess", Bindings: []string{"b1"}},
		},
	}

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered MQTT receiver requires $share/ topic prefix")
}

// TestValidate_ClusteredMQTTEmptyTopics_OK validates that a clustered MQTT
// receiver with no topics passes validation (no subscriptions = no duplication).
func TestValidate_ClusteredMQTTEmptyTopics_OK(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Topics = nil

	err := Validate(cfg)

	assert.NoError(t, err)
}

// TestValidate_ClusteredMultiReceiver_OnlyMQTTChecked validates that when
// multiple receivers exist, only the MQTT receiver is checked. An SQS
// receiver with bare topics must not trigger the shared subscription rule.
func TestValidate_ClusteredMultiReceiver_OnlyMQTTChecked(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{
		ID:        "rx-sqs",
		Transport: "sqs",
		Topics:    []ports.SubscriptionDef{{Topic: "my-queue"}},
	})
	cfg.Routes = append(cfg.Routes, ports.RouteDef{
		ID: "r2", ReceiverID: "rx-sqs", Bindings: []string{"b1"},
	})

	err := Validate(cfg)

	require.Error(t, err)
	ve := err.(*ValidationError)
	for _, e := range ve.Errors {
		assert.NotContains(t, e, "rx-sqs")
	}
	assert.Contains(t, err.Error(), "rx-mqtt")
}

// TestValidate_ClusteredMQTTEphemeralSession_Error validates that ephemeral
// session mode (like persistent) still requires $share/ topics.
func TestValidate_ClusteredMQTTEphemeralSession_Error(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Sessions[0].SessionMode = "ephemeral"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered MQTT receiver requires $share/ topic prefix")
}

// TestValidate_ClusteredMQTTShareExact_Malformed validates that "$share/"
// alone (no group, no topic) is rejected as malformed.
func TestValidate_ClusteredMQTTShareExact_Malformed(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Receivers[0].Topics[0].Topic = "$share/"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed $share/ topic")
}

// TestValidate_ClusteredMQTTCaseInsensitiveTransport_Error validates that
// transport matching is case-insensitive (e.g. "MQTT" is treated as mqtt).
func TestValidate_ClusteredMQTTCaseInsensitiveTransport_Error(t *testing.T) {
	cfg := clusteredMQTTConfig()
	cfg.Sessions[0].Transport = "MQTT"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered MQTT receiver requires $share/ topic prefix")
}
