// Logging re-exports for package main: the unit's leveled logger lives in
// xlog so the hardware package can share it, and main keeps this thin
// adapter so the rest of main/remote can log without importing xlog directly
// (and so a level change funnels through settingChanged -> persistConfig).

package main

import "pi9696/xlog"

// LogLevel aliases xlog.Level so existing main code keeps its type name.
type LogLevel = xlog.Level

// Level constants, matching xlog.
const (
	LogError = xlog.Error
	LogWarn  = xlog.Warn
	LogInfo  = xlog.Info
	LogDebug = xlog.Debug
)

// logLevelNames are the user-facing level names (OLED/WebUI).
var logLevelNames = xlog.Names

// openLogFileSink best-effort opens the app's on-device log file.
func openLogFileSink() {
	xlog.OpenFileSink("/var/log/pi9696/app.log")
}

// setLogLevel sets the logging threshold and persists it, so a raised level
// survives a reboot. Must be called from under mutex like every other setter
// (rendered values change on the next frame).
func setLogLevel(l LogLevel) {
	xlog.SetLevel(l)
	settingChanged()
}

// applyLogLevel sets the logging threshold without persisting - used only
// when loading the persisted config at startup, where a spurious write-back
// (and the directory it implies) is unwanted.
func applyLogLevel(l LogLevel) {
	xlog.SetLevel(l)
}

func logDebugf(format string, args ...any) { xlog.Debugf(format, args...) }
func logInfof(format string, args ...any)  { xlog.Infof(format, args...) }
func logWarnf(format string, args ...any)  { xlog.Warnf(format, args...) }
func logErrorf(format string, args ...any) { xlog.Errorf(format, args...) }
