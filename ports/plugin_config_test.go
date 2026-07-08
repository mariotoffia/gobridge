package ports_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeConfig is a minimal PluginConfig used to assert decoder output
// is returned unchanged by Registry.Decode.
type fakeConfig struct {
	kind string
	val  string
}

func (f fakeConfig) Kind() string    { return f.kind }
func (f fakeConfig) Validate() error { return nil }

// fakeRaw is a stub RawConfig — Decode is unused in registry tests
// because registered decoders ignore raw and return a fixed value.
type fakeRaw struct{}

func (fakeRaw) Decode(any) error { return nil }

// Verifies Registry.Decode returns the registered decoder's output for a known kind.
func TestRegistry_Decode_ReturnsRegisteredDecoderOutput(t *testing.T) {
	r := ports.NewRegistry()
	want := fakeConfig{kind: "demo.kind", val: "hello"}

	require.NoError(t, r.Register("demo.kind", func(raw ports.RawConfig) (ports.PluginConfig, error) {
		return want, nil
	}))

	got, err := r.Decode("demo.kind", fakeRaw{})
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Verifies Register returns ErrDuplicateKind when the same kind is registered twice.
func TestRegistry_Register_ReturnsErrDuplicateKindOnDuplicate(t *testing.T) {
	r := ports.NewRegistry()
	dec := func(ports.RawConfig) (ports.PluginConfig, error) { return fakeConfig{}, nil }

	require.NoError(t, r.Register("dup.kind", dec))

	err := r.Register("dup.kind", dec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ports.ErrDuplicateKind)
	assert.Contains(t, err.Error(), `"dup.kind"`)
}

// Verifies Register returns ErrNilDecoder on a nil decoder — registering nothing is a programming error.
func TestRegistry_Register_ReturnsErrNilDecoderOnNil(t *testing.T) {
	r := ports.NewRegistry()
	err := r.Register("x", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ports.ErrNilDecoder)
	assert.Contains(t, err.Error(), `"x"`)
}

// TestPortsSentinels_CodeClass pins the (Code, Class) pairing of the two
// ports-level BridgeError sentinels so the contract layer stays code→class
// consistent with the domain (finding 3). ErrNilDecoder was migrated off the
// message-payload code onto the dedicated config code: a nil ConfigDecoder is a
// registration/config defect, not a rejected message payload. Keeping it on
// ErrCodeInvalidConfig preserves "INVALID_PAYLOAD is uniquely ErrorRejected"
// across domain + ports.
func TestPortsSentinels_CodeClass(t *testing.T) {
	cases := []struct {
		name     string
		sentinel *shared.BridgeError
		code     shared.ErrorCode
		class    shared.ErrorClass
	}{
		{"ErrDuplicateKind", ports.ErrDuplicateKind, shared.ErrCodeAlreadyExists, shared.ErrorPermanent},
		{"ErrNilDecoder", ports.ErrNilDecoder, shared.ErrCodeInvalidConfig, shared.ErrorPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sentinel.Code != tc.code {
				t.Fatalf("%s code = %q, want %q", tc.name, tc.sentinel.Code, tc.code)
			}
			if tc.sentinel.Class != tc.class {
				t.Fatalf("%s class = %q, want %q", tc.name, tc.sentinel.Class, tc.class)
			}
			if tc.sentinel.Code == shared.ErrCodeInvalidPayload {
				t.Fatalf("%s must not reuse INVALID_PAYLOAD (that code is uniquely ErrorRejected)", tc.name)
			}
		})
	}
}

// Verifies Decode returns a clear "unknown plugin kind" error when no decoder is registered.
func TestRegistry_Decode_UnknownKind(t *testing.T) {
	r := ports.NewRegistry()

	got, err := r.Decode("missing.kind", fakeRaw{})
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "unknown plugin kind")
	assert.Contains(t, err.Error(), `"missing.kind"`)
}

// Verifies decoder errors are surfaced unchanged so callers can wrap them with their own context.
func TestRegistry_Decode_PropagatesDecoderError(t *testing.T) {
	r := ports.NewRegistry()
	sentinel := errors.New("decode boom")

	require.NoError(t, r.Register("err.kind", func(ports.RawConfig) (ports.PluginConfig, error) {
		return nil, sentinel
	}))

	got, err := r.Decode("err.kind", fakeRaw{})
	assert.Nil(t, got)
	require.ErrorIs(t, err, sentinel)
}

// Verifies concurrent Register and Decode calls are race-safe.
// Run with -race (the project's default) to surface any data races.
func TestRegistry_ConcurrentRegisterAndDecode(t *testing.T) {
	r := ports.NewRegistry()

	const writers = 16
	const readers = 16
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	// Writers register disjoint kinds to avoid duplicate errors; the
	// goal is to exercise the mutex, not the dup-detection path.
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				kind := fmt.Sprintf("kind.%d.%d", w, i)
				if err := r.Register(kind, func(ports.RawConfig) (ports.PluginConfig, error) {
					return fakeConfig{kind: kind}, nil
				}); err != nil {
					t.Errorf("register: %v", err)
					return
				}
			}
		}(w)
	}

	// Readers race against writers calling Decode; they accept either
	// a successful lookup or an "unknown plugin kind" miss — only a
	// data race or a panic should fail the test.
	for rIdx := 0; rIdx < readers; rIdx++ {
		go func(rIdx int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				kind := fmt.Sprintf("kind.%d.%d", rIdx%writers, i)
				_, err := r.Decode(kind, fakeRaw{})
				if err != nil && !strings.Contains(err.Error(), "unknown plugin kind") {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}(rIdx)
	}

	wg.Wait()
}
