package bootstrap

import (
	"context"
	"log/slog"
	"testing"
	"time"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func testBootstrapCfg() deployinfra.BootstrapConfig {
	return deployinfra.BootstrapConfig{
		BridgeID:         "bridge-x",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}
}

func TestApplyLogLevel_UpdatesLevelVar(t *testing.T) {
	lv := new(slog.LevelVar) // starts at Info (0)
	app := NewApp(testBootstrapCfg(),
		WithLogLevelVar(lv),
		WithLogger(slog.New(slog.NewJSONHandler(discard{}, &slog.HandlerOptions{Level: lv}))),
	)

	// Raise to debug.
	app.applyLogLevel(&ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "debug"}})
	if lv.Level() != slog.LevelDebug {
		t.Fatalf("after debug: level = %v, want Debug", lv.Level())
	}

	// Lower to error.
	app.applyLogLevel(&ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "error"}})
	if lv.Level() != slog.LevelError {
		t.Fatalf("after error: level = %v, want Error", lv.Level())
	}

	// Unknown / empty leaves the level unchanged (does not silently reset).
	app.applyLogLevel(&ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "bogus"}})
	if lv.Level() != slog.LevelError {
		t.Fatalf("after bogus: level = %v, want Error (unchanged)", lv.Level())
	}
	app.applyLogLevel(&ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: ""}})
	if lv.Level() != slog.LevelError {
		t.Fatalf("after empty: level = %v, want Error (unchanged)", lv.Level())
	}
}

func TestApplyLogLevel_NoLevelVar_NoPanic(t *testing.T) {
	app := NewApp(testBootstrapCfg()) // no WithLogLevelVar
	// Must be a no-op, not a nil-deref.
	app.applyLogLevel(&ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "debug"}})
}

func TestWatchTerminal_SignalsOnTerminalRuntime(t *testing.T) {
	app := NewApp(testBootstrapCfg(), WithTerminalPollInterval(5*time.Millisecond))
	app.terminalProbe = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.watchTerminal(ctx)

	// The backstop must signal terminalCh so App.Run exits non-zero.
	wait.RequireReceive(t, app.terminalCh, time.Second)
}

func TestWatchTerminal_StopsOnContextCancel(t *testing.T) {
	app := NewApp(testBootstrapCfg(), WithTerminalPollInterval(5*time.Millisecond))
	app.terminalProbe = func() bool { return false } // never terminal

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.watchTerminal(ctx)
		close(done)
	}()

	cancel()
	// Cancelling the context must return the goroutine (no leak, no signal).
	wait.RequireClosed(t, done, time.Second)
	select {
	case <-app.terminalCh:
		t.Fatal("terminalCh signalled despite non-terminal runtime")
	default:
	}
}

// discard is an io.Writer that drops all writes (test logger sink).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
