package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestTraceEnabled_NilLogger(t *testing.T) {
	if TraceEnabled(nil) {
		t.Error("TraceEnabled(nil) should be false")
	}
}

func TestTraceEnabled_WarnLevel(t *testing.T) {
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
	if TraceEnabled(l) {
		t.Error("TraceEnabled should be false at Warn level")
	}
	if DebugEnabled(l) {
		t.Error("DebugEnabled should be false at Warn level")
	}
}

func TestTraceEnabled_DebugLevel(t *testing.T) {
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	if TraceEnabled(l) {
		t.Error("TraceEnabled should be false at Debug level (trace is lower)")
	}
	if !DebugEnabled(l) {
		t.Error("DebugEnabled should be true at Debug level")
	}
}

func TestTraceEnabled_TraceLevel(t *testing.T) {
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{
		Level: LevelTrace,
	}))
	if !TraceEnabled(l) {
		t.Error("TraceEnabled should be true at Trace level")
	}
	if !DebugEnabled(l) {
		t.Error("DebugEnabled should be true at Trace level")
	}
}

func TestTrace_WritesOutput(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level:       LevelTrace,
		ReplaceAttr: ReplaceLevel,
	}))
	Trace(l, "hello", "key", "val")
	if !strings.Contains(buf.String(), "TRACE") {
		t.Errorf("expected TRACE in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("expected 'hello' in output, got: %s", buf.String())
	}
}

func TestTrace_NilLogger_NoPanic(t *testing.T) {
	Trace(nil, "should not panic")
}

func TestDebug_NilLogger_NoPanic(t *testing.T) {
	Debug(nil, "should not panic")
}

func TestReplaceLevel_NonLevelKey(t *testing.T) {
	a := slog.String("msg", "hello")
	result := ReplaceLevel(nil, a)
	if result.Value.String() != "hello" {
		t.Errorf("non-level attr should be unchanged")
	}
}
