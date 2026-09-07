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
