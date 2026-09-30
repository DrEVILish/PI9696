package main

import (
	"testing"
)

// Pins the slog port of the leveled logger: default Error-only, the four
// index thresholds map onto slog's scale and back, and the WebUI/OLED round
// trip (Idx -> set -> read) is stable.
func TestLogLevelThresholds(t *testing.T) {
	orig := currentLogLevel()
	t.Cleanup(func() { applyLogLevel(orig) })

	if orig != LogError {
		t.Fatalf("expected default level Error, got %d", orig)
	}
	for i, want := range []LogLevel{LogError, LogWarn, LogInfo, LogDebug} {
		applyLogLevel(want)
		if got := currentLogLevel(); got != want {
			t.Fatalf("idx %d (%s): currentLogLevel() = %d, want %d", i, logLevelNames[i], got, want)
		}
		if slogLevels[i] != slogLevel.Level() {
			t.Fatalf("idx %d: slog threshold = %v, want %v", i, slogLevel.Level(), slogLevels[i])
		}
	}
}

// Disabled levels must not pay for formatting: with the threshold at Error,
// Debug/Info/Warn args (which Sprintf would evaluate eagerly) must never be
// touched, while Error still formats.
type formatProbe struct{ called *bool }

func (p formatProbe) String() string { *p.called = true; return "x" }

func TestDisabledLogSkipsFormatting(t *testing.T) {
	orig := currentLogLevel()
	applyLogLevel(LogError)
	t.Cleanup(func() { applyLogLevel(orig) })
	called := false
	p := formatProbe{called: &called}
	logDebugf("msg %s", p)
	logInfof("msg %s", p)
	logWarnf("msg %s", p)
	if called {
		t.Fatal("disabled level evaluated format args")
	}
	logErrorf("msg %s", p)
	if !called {
		t.Fatal("enabled level skipped formatting")
	}
}
