package parser_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// sqsLikeConfig mirrors the shape of a typical adapter Config struct:
// scalar fields with json tags (the repo-wide convention), including
// a time.Duration that decodes from a textual literal like "30s".
type sqsLikeConfig struct {
	QueueURL          string        `json:"queueUrl,omitempty"`
	Region            string        `json:"region,omitempty"`
	VisibilityTimeout time.Duration `json:"visibility,omitempty"`
	MaxMessages       int           `json:"maxMessages,omitempty"`
}

// Verifies a representative map[string]any round-trips into a typed Go struct via RawConfig.Decode.
func TestRawConfig_Decode_RoundTrip(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{
		"queueUrl":    "https://sqs.eu-west-1.amazonaws.com/123/queue",
		"region":      "eu-west-1",
		"visibility":  "30s",
		"maxMessages": 5,
	})

	var got sqsLikeConfig
	require.NoError(t, raw.Decode(&got))

	assert.Equal(t, "https://sqs.eu-west-1.amazonaws.com/123/queue", got.QueueURL)
	assert.Equal(t, "eu-west-1", got.Region)
	assert.Equal(t, 30*time.Second, got.VisibilityTimeout)
	assert.Equal(t, 5, got.MaxMessages)
}

// Verifies Decode surfaces a useful error when a scalar is the wrong type
// (e.g. a string where the target expects an int).
func TestRawConfig_Decode_WrongScalarType(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{
		"queueUrl":    "q",
		"maxMessages": "not-a-number",
	})

	var got sqsLikeConfig
	err := raw.Decode(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode plugin options")
}

// Verifies Decode against a nil map leaves the target at its zero value
// (an absent `options:` block must not error — Validate() rejects it).
func TestRawConfig_Decode_NilMap(t *testing.T) {
	raw := parser.NewRawConfig(nil)

	var got sqsLikeConfig
	require.NoError(t, raw.Decode(&got))
	assert.Equal(t, sqsLikeConfig{}, got)
}

// Verifies Decode against an empty map is a no-op, matching nil-map behaviour.
func TestRawConfig_Decode_EmptyMap(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{})

	var got sqsLikeConfig
	require.NoError(t, raw.Decode(&got))
	assert.Equal(t, sqsLikeConfig{}, got)
}

// Verifies Decode rejects a nil target with a clear error rather than panicking.
func TestRawConfig_Decode_NilTarget(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{"queueUrl": "q"})

	err := raw.Decode(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target is nil")
}

// Verifies Decode rejects unknown / misspelled keys at decode time
// rather than silently dropping them. This guards against typos in
// plugin option blocks (e.g. "timeoutt" instead of "timeout").
func TestRawConfig_Decode_UnknownKey_Rejected(t *testing.T) {
	type strictTarget struct {
		Timeout time.Duration `json:"timeout,omitempty"`
	}

	raw := parser.NewRawConfig(map[string]any{
		"timeout":  "30s",
		"timeoutt": "5s",
	})

	var got strictTarget
	err := raw.Decode(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode plugin options")
	assert.Contains(t, err.Error(), "timeoutt")
	assert.Equal(t, strictTarget{}, got)
}

// otelLikeConfig mirrors the shape of the existing OTel adapter
// configs: every field uses a `json:"..."` tag with camelCase keys,
// the dominant repo-wide convention. RawConfig.Decode must honour
// this tag (yaml.v3 alone does not) so plugin option blocks like
// `serviceName: foo` populate the correct field.
type otelLikeConfig struct {
	Endpoint      string            `json:"endpoint,omitempty"`
	ServiceName   string            `json:"serviceName,omitempty"`
	FlushInterval time.Duration     `json:"flushInterval,omitempty"`
	Insecure      bool              `json:"insecure,omitempty"`
	DefaultTags   []string          `json:"defaultTags,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Verifies camelCase keys decode into json-tagged fields, including
// time.Duration parsed from a string literal and slice/map composites.
func TestRawConfig_Decode_JSONTaggedStruct(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{
		"endpoint":      "https://otel.example.com:4317",
		"serviceName":   "checkout",
		"flushInterval": "15s",
		"insecure":      true,
		"defaultTags":   []any{"env=prod", "tier=edge"},
		"headers": map[string]any{
			"x-tenant": "acme",
		},
	})

	var got otelLikeConfig
	require.NoError(t, raw.Decode(&got))

	assert.Equal(t, "https://otel.example.com:4317", got.Endpoint)
	assert.Equal(t, "checkout", got.ServiceName)
	assert.Equal(t, 15*time.Second, got.FlushInterval)
	assert.True(t, got.Insecure)
	assert.Equal(t, []string{"env=prod", "tier=edge"}, got.DefaultTags)
	assert.Equal(t, map[string]string{"x-tenant": "acme"}, got.Headers)
}

// Verifies a camelCase typo (flushIntervall) on a json-tagged struct is
// rejected with target zeroed — the json tag path enforces strictness
// just like the canonical key path.
func TestRawConfig_Decode_JSONTaggedStruct_UnknownKey(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{
		"serviceName":    "checkout",
		"flushIntervall": "15s",
	})

	var got otelLikeConfig
	err := raw.Decode(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode plugin options")
	assert.Contains(t, err.Error(), "flushIntervall")
	assert.Equal(t, otelLikeConfig{}, got)
}

// Verifies that float-shaped numeric inputs are rejected when they
// would silently lose precision (fractional -> int) or be misread
// (bare float -> time.Duration), while still allowing exact integral
// floats and string-form durations through.
func TestRawConfig_DecodeRejectsFractionalAndBareFloatDuration(t *testing.T) {
	type decodeHookCfg struct {
		MaxMessages   int           `json:"maxMessages"`
		Count32       int32         `json:"count32"`
		CountU64      uint64        `json:"countU64"`
		FlushInterval time.Duration `json:"flushInterval"`
	}
	type row struct {
		name      string
		data      map[string]any
		wantErr   bool
		errSubstr string
		check     func(t *testing.T, got decodeHookCfg)
	}
	rows := []row{
		{name: "float_with_fraction_into_int_rejected", data: map[string]any{"maxMessages": 5.9}, wantErr: true, errSubstr: "fractional"},
		{name: "integral_float_into_int_accepted", data: map[string]any{"maxMessages": 5.0}, wantErr: false, check: func(t *testing.T, g decodeHookCfg) {
			if g.MaxMessages != 5 {
				t.Fatalf("got %d", g.MaxMessages)
			}
		}},
		{name: "plain_int_into_int_regression", data: map[string]any{"maxMessages": 5}, wantErr: false, check: func(t *testing.T, g decodeHookCfg) {
			if g.MaxMessages != 5 {
				t.Fatalf("got %d", g.MaxMessages)
			}
		}},
		{name: "float_with_fraction_into_int32_rejected", data: map[string]any{"count32": 5.9}, wantErr: true, errSubstr: "fractional"},
		{name: "float_with_fraction_into_uint64_rejected", data: map[string]any{"countU64": 5.9}, wantErr: true, errSubstr: "fractional"},
		{name: "fractional_float_into_duration_rejected", data: map[string]any{"flushInterval": 1.5}, wantErr: true, errSubstr: "bare number"},
		{name: "bare_int_float_into_duration_rejected_unconditionally", data: map[string]any{"flushInterval": float64(30)}, wantErr: true, errSubstr: "bare number"},
		{name: "string_30s_into_duration_regression", data: map[string]any{"flushInterval": "30s"}, wantErr: false, check: func(t *testing.T, g decodeHookCfg) {
			if g.FlushInterval != 30*time.Second {
				t.Fatalf("got %v", g.FlushInterval)
			}
		}},
		{name: "string_0s_into_duration_edge", data: map[string]any{"flushInterval": "0s"}, wantErr: false, check: func(t *testing.T, g decodeHookCfg) {
			if g.FlushInterval != 0 {
				t.Fatalf("got %v", g.FlushInterval)
			}
		}},
	}
	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got decodeHookCfg
			err := parser.NewRawConfig(tc.data).Decode(&got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; decoded=%+v", got)
				}
				if !strings.Contains(strings.ToLower(err.Error()), tc.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, got)
		})
	}
}

// secretLikeConfig mirrors a plugin-config carrying a redaction-safe
// secret field plus a nested connection sub-config. It exercises the
// mapstructure TextUnmarshaller hook that lets a scalar string decode
// into a shared.Secret — mapstructure does not honour
// encoding.TextUnmarshaler natively.
type secretLikeConfig struct {
	Host       string        `json:"host,omitempty"`
	APIKey     shared.Secret `json:"apiKey,omitempty"`
	Connection secretConnSub `json:"connection,omitempty"`
}

type secretConnSub struct {
	ConnectionString shared.Secret `json:"connectionString,omitempty"`
}

// Verifies a scalar string decodes into shared.Secret fields — both
// direct and nested — through the production mapstructure path. This is
// the regression behind "expected a map or struct, got string" when
// plugin-config secret fields moved from string to shared.Secret.
func TestRawConfig_Decode_SecretField(t *testing.T) {
	raw := parser.NewRawConfig(map[string]any{
		"host":   "broker:1883",
		"apiKey": "s3cr3t-key",
		"connection": map[string]any{
			"connectionString": "Endpoint=sb://x",
		},
	})

	var got secretLikeConfig
	require.NoError(t, raw.Decode(&got))

	assert.Equal(t, "broker:1883", got.Host)
	assert.Equal(t, "s3cr3t-key", got.APIKey.Reveal())
	assert.Equal(t, "Endpoint=sb://x", got.Connection.ConnectionString.Reveal())

	// An absent secret stays zero — no spurious decode error.
	var empty secretLikeConfig
	require.NoError(t, parser.NewRawConfig(map[string]any{"host": "h"}).Decode(&empty))
	assert.True(t, empty.APIKey.IsZero())
}
