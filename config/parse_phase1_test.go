package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ─── FIX-003 Phase 1 parser tests ──────────────────────────────────
//
// Cover the two-stage decode for every plugin attachment point in
// ports/blueprint.go. Stories per attachment point:
//   a. UNKNOWN kind → error mentions ID + kind
//   b. MISSING kind → error mentions ID and "missing type/transport"
//   c. VALIDATION FAILURE — fake decoder returns a config whose
//      Validate() errors; parser wraps it with ID + kind
//   d. SUCCESS — fake decoder produces a typed config; parser
//      places it in .Config and the originating raw in .Raw().
//
// SubscriptionDef inherits its kind from the parent ReceiverDef;
// BindingDef inherits from the referenced SenderDef. The "sender not
// found" error path is exercised separately for BindingDef.

// ── fakes ──────────────────────────────────────────────────────────

const phase1FakeKind = "__phase1test.fake"

type phase1FakeConfig struct {
	kind       string
	validateOK bool
	field      string
}

func (f phase1FakeConfig) Kind() string { return f.kind }
func (f phase1FakeConfig) Validate() error {
	if f.validateOK {
		return nil
	}
	return errors.New("phase1: validation failure")
}

// fakeRegistry returns a registry where phase1FakeKind decodes to a
// phase1FakeConfig populated from the raw payload's `field` key.
// The decoder's Validate result is steered by the raw payload's
// `validate_ok` boolean (default false).
func fakeRegistry() *ports.Registry {
	reg := ports.NewRegistry()
	if err := reg.Register(phase1FakeKind, func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var probe struct {
			Field      string `json:"field"`
			ValidateOK bool   `json:"validate_ok"`
		}
		if err := raw.Decode(&probe); err != nil {
			return nil, err
		}
		cfg := phase1FakeConfig{
			kind:       phase1FakeKind,
			validateOK: probe.ValidateOK,
			field:      probe.Field,
		}
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return cfg, nil
	}); err != nil {
		panic(err)
	}
	return reg
}

// ── helpers ────────────────────────────────────────────────────────

// parseYAML is a thin wrapper that hides the io.Reader plumbing for
// table-driven tests.
func parseYAML(t *testing.T, reg *ports.Registry, src string) (*ports.BridgeConfig, error) {
	t.Helper()
	return Parse(strings.NewReader(src), FormatYAML, reg)
}

const baseBridge = `bridge:
  id: phase1
`

// ── unknown / missing / validation matrix ──────────────────────────

func TestParse_Phase1_UnknownKind(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		needsKind string // user-facing kind in the error
		needsID   string
	}{
		{
			name: "store/lease",
			yaml: baseBridge + `stores:
  lease:
    type: nope.kind
`,
			needsKind: "nope.kind",
			needsID:   `"lease"`,
		},
		{
			name: "session",
			yaml: baseBridge + `sessions:
  - id: sess-1
    transport: nope.kind
`,
			needsKind: "nope.kind",
			needsID:   `"sess-1"`,
		},
		{
			name: "receiver",
			yaml: baseBridge + `receivers:
  - id: rcv-1
    transport: nope.kind
`,
			needsKind: "nope.kind",
			needsID:   `"rcv-1"`,
		},
		{
			name: "sender",
			yaml: baseBridge + `senders:
  - id: snd-1
    transport: nope.kind
`,
			needsKind: "nope.kind",
			needsID:   `"snd-1"`,
		},
	}

	reg := fakeRegistry() // does NOT register `nope.kind`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseYAML(t, reg, tc.yaml)
			require.Error(t, err)
			msg := err.Error()
			assert.Contains(t, msg, "unknown plugin kind")
			assert.Contains(t, msg, tc.needsKind)
			assert.Contains(t, msg, tc.needsID)
		})
	}
}

func TestParse_Phase1_MissingKind(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		needsID    string
		needsField string // "type" for stores, "transport" elsewhere
	}{
		{
			name: "store/outbox",
			yaml: baseBridge + `stores:
  outbox:
    options:
      foo: bar
`,
			needsID:    `"outbox"`,
			needsField: "missing type",
		},
		{
			name: "session",
			yaml: baseBridge + `sessions:
  - id: sess-2
`,
			needsID:    `"sess-2"`,
			needsField: "missing transport",
		},
		{
			name: "receiver",
			yaml: baseBridge + `receivers:
  - id: rcv-2
`,
			needsID:    `"rcv-2"`,
			needsField: "missing transport",
		},
		{
			name: "sender",
			yaml: baseBridge + `senders:
  - id: snd-2
`,
			needsID:    `"snd-2"`,
			needsField: "missing transport",
		},
	}

	reg := fakeRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseYAML(t, reg, tc.yaml)
			require.Error(t, err)
			msg := err.Error()
			assert.Contains(t, msg, tc.needsID)
			assert.Contains(t, msg, tc.needsField)
		})
	}
}

func TestParse_Phase1_ValidationFailure(t *testing.T) {
	// The fake decoder returns an error when validate_ok is false.
	// Cover one representative attachment point per kind family:
	// store (own kind), session (own kind), receiver (parent of
	// subscription), sender (parent of binding).
	cases := []struct {
		name string
		yaml string
		id   string
	}{
		{
			name: "store",
			yaml: baseBridge + `stores:
  lease:
    type: ` + phase1FakeKind + `
    options:
      validate_ok: false
`,
			id: `"lease"`,
		},
		{
			name: "session",
			yaml: baseBridge + `sessions:
  - id: sess-v
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: false
`,
			id: `"sess-v"`,
		},
		{
			name: "receiver",
			yaml: baseBridge + `receivers:
  - id: rcv-v
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: false
`,
			id: `"rcv-v"`,
		},
		{
			name: "sender",
			yaml: baseBridge + `senders:
  - id: snd-v
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: false
`,
			id: `"snd-v"`,
		},
	}

	reg := fakeRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseYAML(t, reg, tc.yaml)
			require.Error(t, err)
			msg := err.Error()
			assert.Contains(t, msg, tc.id)
			assert.Contains(t, msg, phase1FakeKind)
			assert.Contains(t, msg, "validation failure")
		})
	}
}

// ── success path ───────────────────────────────────────────────────

// TestParse_Phase1_Success_AllAttachmentPoints exercises a full
// blueprint that uses the fake kind for every attachment point
// (store, session, receiver+subscription, sender+binding). Asserts
// that .Config is populated with the typed PluginConfig and
// .Raw() carries the originating raw payload.
func TestParse_Phase1_Success_AllAttachmentPoints(t *testing.T) {
	yamlSrc := baseBridge + `stores:
  lease:
    type: ` + phase1FakeKind + `
    options:
      validate_ok: true
      field: lease-store
sessions:
  - id: sess-ok
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: true
      field: sess-cfg
receivers:
  - id: rcv-ok
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: true
      field: rcv-cfg
    topics:
      - topic: a/b
        options:
          validate_ok: true
          field: sub-cfg
senders:
  - id: snd-ok
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: true
      field: snd-cfg
bindings:
  - id: bnd-ok
    sender_id: snd-ok
    address: out://x
    options:
      validate_ok: true
      field: bnd-cfg
`

	cfg, err := parseYAML(t, fakeRegistry(), yamlSrc)
	require.NoError(t, err)

	// Store
	require.NotNil(t, cfg.Stores.Lease)
	require.NotNil(t, cfg.Stores.Lease.Config)
	assert.Equal(t, phase1FakeKind, cfg.Stores.Lease.Config.Kind())
	assert.NotNil(t, cfg.Stores.Lease.Raw())
	assert.Equal(t, "lease-store", cfg.Stores.Lease.Config.(phase1FakeConfig).field)

	// Session
	require.Len(t, cfg.Sessions, 1)
	require.NotNil(t, cfg.Sessions[0].Config)
	assert.Equal(t, phase1FakeKind, cfg.Sessions[0].Config.Kind())
	assert.NotNil(t, cfg.Sessions[0].Raw())

	// Receiver + inherited Subscription kind
	require.Len(t, cfg.Receivers, 1)
	require.NotNil(t, cfg.Receivers[0].Config)
	assert.Equal(t, phase1FakeKind, cfg.Receivers[0].Config.Kind())
	assert.NotNil(t, cfg.Receivers[0].Raw())
	require.Len(t, cfg.Receivers[0].Topics, 1)
	require.NotNil(t, cfg.Receivers[0].Topics[0].Config,
		"subscription Config must be populated with the parent receiver's inherited kind")
	assert.Equal(t, phase1FakeKind, cfg.Receivers[0].Topics[0].Config.Kind())
	assert.NotNil(t, cfg.Receivers[0].Topics[0].Raw())

	// Sender
	require.Len(t, cfg.Senders, 1)
	require.NotNil(t, cfg.Senders[0].Config)
	assert.Equal(t, phase1FakeKind, cfg.Senders[0].Config.Kind())

	// Binding inherits sender's kind
	require.Len(t, cfg.Bindings, 1)
	require.NotNil(t, cfg.Bindings[0].Config,
		"binding Config must be populated with the referenced sender's inherited kind")
	assert.Equal(t, phase1FakeKind, cfg.Bindings[0].Config.Kind())
	assert.NotNil(t, cfg.Bindings[0].Raw())
}

// TestParse_Phase1_Binding_SenderNotFound covers the binding-specific
// "kind family" error path: the binding references a sender ID that
// does not exist anywhere in the blueprint.
func TestParse_Phase1_Binding_SenderNotFound(t *testing.T) {
	yamlSrc := baseBridge + `senders:
  - id: snd-known
    transport: ` + phase1FakeKind + `
    options:
      validate_ok: true
bindings:
  - id: b1
    sender_id: snd-missing
    address: out://x
`
	_, err := parseYAML(t, fakeRegistry(), yamlSrc)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `binding "b1"`)
	assert.Contains(t, msg, `sender "snd-missing" not found`)
}

// TestParse_Phase1_EmptyOptions_StillConsultsRegistry verifies the
// brief's contract that an absent `options:` block does not skip the
// registry: a registered decoder still runs against an empty
// RawConfig and may error or succeed accordingly.
func TestParse_Phase1_EmptyOptions_StillConsultsRegistry(t *testing.T) {
	// validate_ok defaults to false ⇒ the fake decoder errors.
	yamlSrc := baseBridge + `sessions:
  - id: sess-empty
    transport: ` + phase1FakeKind + `
`
	_, err := parseYAML(t, fakeRegistry(), yamlSrc)
	require.Error(t, err, "empty options block must still flow through the registered decoder")
	assert.Contains(t, err.Error(), `"sess-empty"`)
	assert.Contains(t, err.Error(), "validation failure")
}
