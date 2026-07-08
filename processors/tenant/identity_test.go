package tenant

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// capturingValidator records the tenant ID passed to Validate so a test can
// prove the resolved (possibly coerced) identity that reached enforcement.
type capturingValidator struct {
	seen string
	info ports.TenantInfo
}

func (c *capturingValidator) Validate(_ context.Context, tenantID string) (ports.TenantInfo, error) {
	c.seen = tenantID
	return c.info, nil
}

func envWithHeader(key string, value any) *messaging.Envelope {
	return messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Headers: map[string]any{key: value},
	})
}

// F4: a tenant header stamped as a typed integer (int/int64/uint32) by a
// transport — or rehydrated as an integral float64 by a JSON round-trip
// (DLQ/outbox save-load) — is coerced to its decimal string and flows through
// enforcement; it must NOT be silently dropped as "no tenant".
func TestProcess_TenantHeader_IntegerCoercion(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want string
	}{
		{"int", int(42), "42"},
		{"int64", int64(9007199254740993), "9007199254740993"},
		{"uint32", uint32(4294967295), "4294967295"},
		// float64 is how encoding/json rehydrates EVERY numeric header, so a
		// numeric tenant must survive a DLQ/outbox round-trip identically.
		{"float64 json-rehydrated int", float64(42), "42"},
		{"float64 at 2^53 boundary", float64(1 << 53), "9007199254740992"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &capturingValidator{info: ports.TenantInfo{ID: tc.want, Active: true}}
			p := mustNew(t, Config{}, WithValidator(v))
			env := envWithHeader(DefaultTenantHeader, tc.raw)

			called := false
			err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
				called = true
				return nil
			})
			require.NoError(t, err)
			assert.True(t, called, "next must be invoked for a coercible tenant")
			assert.Equal(t, tc.want, v.seen,
				"enforcement must see the decimal-coerced tenant identity")
		})
	}
}

// F4: a header that is PRESENT but not a coercible identity (bool/struct/bytes,
// or a fractional / non-finite / out-of-safe-range float64) is a MALFORMED
// identity and must be rejected with ErrInvalidPayload — never silently treated
// as untenanted (which would fail-open the message).
func TestProcess_TenantHeader_PresentButNonString_Rejected(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"bool", true},
		{"float64 fractional", 42.5},
		{"float64 NaN", math.NaN()},
		{"float64 +Inf", math.Inf(1)},
		{"float64 -Inf", math.Inf(-1)},
		{"float64 above 2^53", float64(1 << 54)},
		{"float64 below -2^53", -float64(1 << 54)},
		{"struct", struct{ X int }{X: 1}},
		{"bytes", []byte("acme")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A validator that would fail-open is irrelevant: the reject must
			// happen before validation regardless of RequireTenant.
			v := &stubValidator{info: ports.TenantInfo{ID: "x", Active: true}}
			p := mustNew(t, Config{}, WithValidator(v))
			env := envWithHeader(DefaultTenantHeader, tc.raw)

			called := false
			err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
				called = true
				return nil
			})
			require.Error(t, err, "present-but-non-string tenant must be rejected")
			assert.False(t, called, "next must NOT be invoked for a malformed identity")
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
			be, ok := shared.AsBridgeError(err)
			require.True(t, ok)
			assert.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
		})
	}
}

// F4: an ABSENT header keeps the existing fail-open (RequireTenant=false) and
// reject-when-required (RequireTenant=true) behavior — the type-trap fix must
// not change the absent-header contract.
func TestProcess_TenantHeader_Absent_KeepsExistingBehavior(t *testing.T) {
	t.Run("not required -> fail-open", func(t *testing.T) {
		p := mustNew(t, Config{RequireTenant: false})
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "test"})
		called := false
		err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
			called = true
			return nil
		})
		require.NoError(t, err)
		assert.True(t, called, "absent header + RequireTenant=false must fail-open")
	})

	t.Run("required -> reject", func(t *testing.T) {
		p := mustNew(t, Config{RequireTenant: true})
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "test"})
		err := p.Process(context.Background(), env, nextOK)
		require.Error(t, err)
		require.ErrorIs(t, err, shared.ErrInvalidPayload)
	})
}

// F4: an empty-string tenant header is treated the same as absent (fail-open
// when not required), not as a malformed identity.
func TestProcess_TenantHeader_EmptyString_TreatedAsAbsent(t *testing.T) {
	p := mustNew(t, Config{RequireTenant: false})
	env := envWithHeader(DefaultTenantHeader, "")
	called := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called)
}

// Directly exercises the resolver's three-way contract.
func TestResolveTenantID(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, err := resolveTenantID(map[string]any{}, "tenant")
		require.NoError(t, err)
		assert.Equal(t, "", got)
	})
	t.Run("nil headers", func(t *testing.T) {
		got, err := resolveTenantID(nil, "tenant")
		require.NoError(t, err)
		assert.Equal(t, "", got)
	})
	t.Run("string", func(t *testing.T) {
		got, err := resolveTenantID(map[string]any{"tenant": "acme"}, "tenant")
		require.NoError(t, err)
		assert.Equal(t, "acme", got)
	})
	t.Run("int coerced", func(t *testing.T) {
		got, err := resolveTenantID(map[string]any{"tenant": int(7)}, "tenant")
		require.NoError(t, err)
		assert.Equal(t, "7", got)
	})
	t.Run("integral float coerced", func(t *testing.T) {
		// A JSON round-trip rehydrates a numeric header as float64; an integral
		// value coerces to the same decimal string int64 would produce.
		got, err := resolveTenantID(map[string]any{"tenant": 7.0}, "tenant")
		require.NoError(t, err)
		assert.Equal(t, "7", got)
	})
	t.Run("fractional float rejected", func(t *testing.T) {
		_, err := resolveTenantID(map[string]any{"tenant": 7.5}, "tenant")
		require.Error(t, err)
		require.True(t, errors.Is(err, shared.ErrInvalidPayload))
	})
	t.Run("out-of-range float rejected", func(t *testing.T) {
		_, err := resolveTenantID(map[string]any{"tenant": float64(1 << 54)}, "tenant")
		require.Error(t, err)
		require.True(t, errors.Is(err, shared.ErrInvalidPayload))
	})
}
