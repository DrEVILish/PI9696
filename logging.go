// Leveled logging for the unit (design decision, Round 2) on stdlib log/slog.
//
// Messages are emitted at one of four levels - Error, Warn, Info, Debug -
// filtered by a single process-wide threshold that defaults to Error-only so
// the device writes the minimum log volume by default. The threshold is
// user-changeable from the OLED Settings → Logging submenu and the WebUI
// settings modal, and main persists it across reboots (LogLevelIdx, 0=Error).
//
// Output is a best-effort dual sink: stderr (which the systemd service feeds
// to journald) plus an optional on-device file appender opened by
// openLogFileSink (created at install time under /var/log/pi9696; logrotate rotates it),
// so crash/early-boot messages survive independent of journald's retention.
// If the file can't be opened (e.g. sim mode as a non-root dev user), logging
// silently degrades to stderr/journald only.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// LogLevel is a severity threshold as a 0..3 index (Error most severe) - the
// index doubles as the OLED/WebUI list position and the persisted LogLevelIdx.
type LogLevel int

const (
	LogError LogLevel = iota
	LogWarn
	LogInfo
	LogDebug
)

// logLevelNames are the user-facing level names (OLED/WebUI).
var logLevelNames = []string{"Error", "Warn", "Info", "Debug"}

// slogLevel is the live slog threshold; slog's dynamic-level var.
var slogLevel slog.LevelVar

// slogLevels maps a LogLevel index onto slog's severity scale.
var slogLevels = [4]slog.Level{slog.LevelError, slog.LevelWarn, slog.LevelInfo, slog.LevelDebug}

func init() {
	setupLogs("")
}

// logFileSink is the open on-device log handle, if any. setupLogs replaces
// the handler on every call - without closing the previous file that leaked
// an fd per re-setup.
var logFileSink *os.File

// setupLogs installs the default slog logger (threshold default Error) over
// stderr plus, when path is non-empty and openable, the on-device log file.
// Replaces any previously installed handler, so it's safe to call once at
// startup after deciding on the file path.
func setupLogs(path string) {
	slogLevel.Set(slog.LevelError)
	w := io.Writer(os.Stderr)
	if logFileSink != nil {
		logFileSink.Close()
		logFileSink = nil
	}
	if path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640); err == nil {
			logFileSink = f
			w = io.MultiWriter(os.Stderr, f)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: &slogLevel})))
}

// openLogFileSink best-effort adds the app's on-device log file as a second
// sink. Failure (e.g. no permission under sim/dev mode) is not fatal -
// logging degrades to stderr/journald only.
func openLogFileSink() {
	setupLogs("/var/log/pi9696/app.log")
}

// currentLogLevel returns the live threshold as a LogLevel index.
func currentLogLevel() LogLevel {
	switch slogLevel.Level() {
	case slog.LevelWarn:
		return LogWarn
	case slog.LevelInfo:
		return LogInfo
	case slog.LevelDebug:
		return LogDebug
	default:
		return LogError
	}
}

// setLogLevel sets the logging threshold and persists it, so a raised level
// survives a reboot. Must be called from under mutex like every other setter
// (rendered values change on the next frame).
func setLogLevel(l LogLevel) {
	slogLevel.Set(slogLevels[l])
	settingChanged()
}

// applyLogLevel sets the logging threshold without persisting - used only
// when loading the persisted config at startup, where a spurious write-back
// (and the directory it implies) is unwanted.
func applyLogLevel(l LogLevel) {
	slogLevel.Set(slogLevels[l])
}

func logDebugf(format string, args ...any) { slog.Debug(fmt.Sprintf(format, args...)) }
func logInfof(format string, args ...any)  { slog.Info(fmt.Sprintf(format, args...)) }
func logWarnf(format string, args ...any)  { slog.Warn(fmt.Sprintf(format, args...)) }
func logErrorf(format string, args ...any) { slog.Error(fmt.Sprintf(format, args...)) }
