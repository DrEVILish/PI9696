// Package xlog provides the unit's leveled logger (design decision, Round 2).
//
// Messages are emitted at one of four levels - Error, Warn, Info, Debug -
// filtered by a single process-wide threshold that defaults to Error-only so
// the device writes the minimum log volume by default. The threshold is
// user-changeable from the OLED Settings → Logging submenu and the WebUI
// settings modal, and main persists it across reboots.
//
// Output is a best-effort dual sink: the standard logger (which the systemd
// service feeds to journald) plus an optional on-device file appender opened
// via OpenFileSink (setup.sh creates /var/log/pi9696; logrotate rotates it),
// so crash/early-boot messages survive independent of journald's retention.
// If the file can't be opened (e.g. sim mode as a non-root dev user), logging
// silently degrades to journald/stdout only.
package xlog

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Level is a severity threshold; a message is emitted when its level is <=
// the configured threshold (Error is the most severe, Debug the least).
type Level int

const (
	Error Level = iota
	Warn
	Info
	Debug
)

// Names are the user-facing level names (OLED/WebUI).
var Names = []string{"Error", "Warn", "Info", "Debug"}

// tiers are the line prefixes used on each level.
var tiers = []string{"ERR", "WRN", "INF", "DBG"}

var (
	mu    sync.Mutex
	level = Error

	// fileSink is the optional on-device log file appender, opened once via
	// OpenFileSink. nil if it couldn't be opened.
	fileSink *os.File
)

// OpenFileSink best-effort opens a log file to append to. Failure (e.g. no
// permission under sim/dev mode) is not fatal - logging degrades to
// journald/stdout only.
func OpenFileSink(path string) {
	mu.Lock()
	defer mu.Unlock()
	if fileSink != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return
	}
	fileSink = f
}

// SetLevel sets the logging threshold. Out-of-range values are ignored.
func SetLevel(l Level) {
	mu.Lock()
	defer mu.Unlock()
	if l < Error || l > Debug {
		return
	}
	level = l
}

// GetLevel returns the current logging threshold.
func GetLevel() Level {
	mu.Lock()
	defer mu.Unlock()
	return level
}

// at emits a message at the given level if it passes the threshold, to both
// the standard logger (journald) and the optional file sink.
func at(l Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if l < Error || l > Debug || l > level {
		return
	}
	msg := fmt.Sprintf("%s %s", tiers[l], fmt.Sprintf(format, args...))
	log.Print(msg)
	if fileSink != nil {
		fmt.Fprintf(fileSink, "%s %s\n", time.Now().Format(time.RFC3339), msg)
	}
}

// Errorf logs at Error level.
func Errorf(format string, args ...any) { at(Error, format, args...) }

// Warnf logs at Warn level.
func Warnf(format string, args ...any) { at(Warn, format, args...) }

// Infof logs at Info level.
func Infof(format string, args ...any) { at(Info, format, args...) }

// Debugf logs at Debug level.
func Debugf(format string, args ...any) { at(Debug, format, args...) }
