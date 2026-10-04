package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
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

// The standard log package carries lifecycle lines and every log.Fatalf
// reason. slog.SetDefault bridges it at Info, which the Error-only default
// drops, so those messages vanished; setupLogs must route std log to the
// sinks regardless of the slog threshold.
func TestStdLogWrittenAtDefaultLevel(t *testing.T) {
	orig := currentLogLevel()
	path := filepath.Join(t.TempDir(), "app.log")
	setupLogs(path)
	t.Cleanup(func() {
		setupLogs("")
		applyLogLevel(orig)
	})
	if currentLogLevel() != LogError {
		t.Fatalf("setupLogs should leave the Error-only default, got %d", currentLogLevel())
	}
	log.Println("std log lifecycle line")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "std log lifecycle line") {
		t.Fatalf("std log output dropped at the default level; log file: %q", data)
	}
}

// The app never reopens app.log, so the shipped logrotate config must
// rotate it in place (copytruncate) and bound it; a rename-based rotation
// would leave the app writing to the rotated file forever.
func TestLogrotateConfigCopyTruncates(t *testing.T) {
	data, err := os.ReadFile("deploy/pi9696.logrotate")
	if err != nil {
		t.Fatalf("logrotate config missing: %v", err)
	}
	conf := string(data)
	for _, want := range []string{"/var/log/pi9696/app.log", "copytruncate", "rotate ", "maxsize "} {
		if !strings.Contains(conf, want) {
			t.Errorf("deploy/pi9696.logrotate lacks %q", want)
		}
	}
}
