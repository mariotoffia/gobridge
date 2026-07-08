package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// c15FakeProcessor is a no-op processor for registration tests.
type c15FakeProcessor struct{ name string }

func (p *c15FakeProcessor) Name() string { return p.name }
func (p *c15FakeProcessor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	return next(ctx, env)
}

// F12: registering two processors under the same name must fail Build with an
// error naming the duplicate, instead of silently overwriting (last-wins) and
// dropping the losing processor from every route that references the name.
func TestBuilder_RegisterProcessor_DuplicateNameFailsBuild(t *testing.T) {
	ctx := context.Background()

	b := buildWith(directHoldConfig(), "sqs").
		RegisterProcessor("dup", &c15FakeProcessor{name: "first"}).
		RegisterProcessor("dup", &c15FakeProcessor{name: "second"})

	_, err := b.Build(ctx)
	require.Error(t, err, "duplicate processor name must fail Build")
	assert.Contains(t, err.Error(), "dup", "error must name the duplicate processor")
	assert.Contains(t, strings.ToLower(err.Error()), "already registered")
}

// F12: distinct processor names build fine (guard does not false-positive).
func TestBuilder_RegisterProcessor_DistinctNamesBuild(t *testing.T) {
	ctx := context.Background()

	b := buildWith(directHoldConfig(), "sqs").
		RegisterProcessor("a", &c15FakeProcessor{name: "a"}).
		RegisterProcessor("b", &c15FakeProcessor{name: "b"})

	rt, err := b.Build(ctx)
	require.NoError(t, err)
	require.NotNil(t, rt)
}

// F12: the same guard applies to transport-factory registration (the guard is
// symmetric across all three Register* surfaces).
func TestBuilder_RegisterTransportFactory_DuplicateNameFailsBuild(t *testing.T) {
	ctx := context.Background()

	// buildWith already registers "sqs"; register it again to collide.
	b := buildWith(directHoldConfig(), "sqs").
		RegisterTransportFactory("sqs", &fakeTransportFactory{})

	_, err := b.Build(ctx)
	require.Error(t, err, "duplicate transport factory name must fail Build")
	assert.Contains(t, err.Error(), "sqs")
	assert.Contains(t, strings.ToLower(err.Error()), "already registered")
}

// F12: the Supervisor is the surface the CLI registers plugins on, so a
// duplicate name there must ALSO fail the build — the deduped supervisor maps
// would otherwise drop the collision before any Builder guard could see it.
// White-box via newBuilder().Build(): buildRuntime uses exactly that path, so
// this exercises the real propagation from Supervisor.regErrs into the Builder.
func TestSupervisor_RegisterProcessor_DuplicateNameFailsBuild(t *testing.T) {
	s := NewSupervisor()
	// Register the transport + store the config references so the ONLY build
	// error is the duplicate processor, not a missing dependency.
	s.RegisterTransport("sqs", &fakeTransportFactory{})
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	s.RegisterProcessor("dup", &c15FakeProcessor{name: "first"})
	s.RegisterProcessor("dup", &c15FakeProcessor{name: "second"})

	_, err := s.newBuilder(directHoldConfig()).Build(context.Background())
	require.Error(t, err, "duplicate processor on the Supervisor must fail the build")
	assert.Contains(t, err.Error(), "dup", "error must name the duplicate processor")
	assert.Contains(t, strings.ToLower(err.Error()), "already registered")
}

// F12: distinct names on the Supervisor still build (guard does not
// false-positive across rebuilds).
func TestSupervisor_RegisterProcessor_DistinctNamesBuild(t *testing.T) {
	s := NewSupervisor()
	s.RegisterTransport("sqs", &fakeTransportFactory{})
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	s.RegisterProcessor("a", &c15FakeProcessor{name: "a"})
	s.RegisterProcessor("b", &c15FakeProcessor{name: "b"})

	rt, err := s.newBuilder(directHoldConfig()).Build(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rt)
}
