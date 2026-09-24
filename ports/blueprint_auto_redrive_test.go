package ports_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The accessor is forgiving by design: the validator rejects a malformed or
// negative window on the load path, so a value reaching here that cannot be
// read falls back to the default rather than silently turning redrive off.
func TestStoreConfig_AutoRedriveWindowDuration(t *testing.T) {
	cases := []struct {
		name string
		sc   *ports.StoreConfig
		want time.Duration
	}{
		{"nil store takes the default", nil, ports.DefaultAutoRedriveWindow},
		{"omitted takes the default", &ports.StoreConfig{Type: "memory"}, 24 * time.Hour},
		{"explicit zero turns it off", &ports.StoreConfig{Type: "memory", AutoRedriveWindow: "0s"}, 0},
		{"a written window is used", &ports.StoreConfig{Type: "memory", AutoRedriveWindow: "90m"}, 90 * time.Minute},
		{"malformed takes the default", &ports.StoreConfig{Type: "memory", AutoRedriveWindow: "bogus"}, 24 * time.Hour},
		{"negative takes the default", &ports.StoreConfig{Type: "memory", AutoRedriveWindow: "-1h"}, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.sc.AutoRedriveWindowDuration())
		})
	}
}

func TestDefaultAutoRedriveWindow_IsADay(t *testing.T) {
	assert.Equal(t, 24*time.Hour, ports.DefaultAutoRedriveWindow)
}

func dlqConfig(window string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b"},
		Stores: ports.StoresConfig{DLQ: &ports.StoreConfig{Type: "memory", AutoRedriveWindow: window}},
	}
}

func TestContentNormalForm_AutoRedriveWindow(t *testing.T) {
	cases := []struct {
		name, written, want string
	}{
		// An omitted window stays omitted, so a document written before the
		// setting existed keeps the digest it always had.
		{"omitted stays omitted", "", ""},
		{"an explicit zero is kept, it turns redrive off", "0s", "0s"},
		{"a written window takes the canonical spelling", "1440m", "24h0m0s"},
		{"an unreadable value is kept as written", "soon", "soon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ports.ContentNormalForm(dlqConfig(tc.written))
			require.NotNil(t, got.Stores.DLQ)
			assert.Equal(t, tc.want, got.Stores.DLQ.AutoRedriveWindow)
		})
	}
}

func TestContentNormalForm_AutoRedriveWindowSpellingsAgree(t *testing.T) {
	a := ports.ContentNormalForm(dlqConfig("1440m"))
	b := ports.ContentNormalForm(dlqConfig("24h"))

	assert.Equal(t, a.Stores.DLQ.AutoRedriveWindow, b.Stores.DLQ.AutoRedriveWindow)
}

func TestContentNormalForm_AutoRedriveWindowDoesNotMutateInput(t *testing.T) {
	plugin := &sharedPlugin{broker: "unused"}
	input := dlqConfig("1440m")
	input.Stores.DLQ.SetDecoded(plugin, nil)
	dlq := input.Stores.DLQ

	got := ports.ContentNormalForm(input)

	assert.Same(t, dlq, input.Stores.DLQ, "the caller keeps its own store pointer")
	assert.Equal(t, "1440m", input.Stores.DLQ.AutoRedriveWindow, "the caller keeps its spelling")
	assert.NotSame(t, input.Stores.DLQ, got.Stores.DLQ, "the normal form rewrites its own copy")
	assert.Same(t, plugin, got.Stores.DLQ.Config, "plugin options are carried by identity")
	assert.Equal(t, "memory", got.Stores.DLQ.Type)
}

func TestContentNormalForm_NoDLQStoreStaysNil(t *testing.T) {
	got := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b"}})

	assert.Nil(t, got.Stores.DLQ)
}
