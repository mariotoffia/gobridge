package transport

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// TestNewFactory_NilClockOptionKeepsDefault is the regression test for
// CLOCK_FINDINGS Minor #3: ensures that `NewFactory(WithClock(nil))`
// preserves the `clock.System` default rather than clobbering it.
func TestNewFactory_NilClockOptionKeepsDefault(t *testing.T) {
	t.Parallel()

	f := NewFactory(WithClock(nil))

	if f.clock == nil {
		t.Fatal("NewFactory(WithClock(nil)): factory clock is nil; expected clock.System default")
	}
	if f.clock != clock.System {
		t.Fatalf("NewFactory(WithClock(nil)): factory clock = %T, want clock.System", f.clock)
	}
}

// TestNewFactory_DefaultsToSystemClock validates the no-option path.
func TestNewFactory_DefaultsToSystemClock(t *testing.T) {
	t.Parallel()

	f := NewFactory()

	if f.clock == nil {
		t.Fatal("NewFactory(): factory clock is nil; expected clock.System default")
	}
	if f.clock != clock.System {
		t.Fatalf("NewFactory(): factory clock = %T, want clock.System", f.clock)
	}
}
