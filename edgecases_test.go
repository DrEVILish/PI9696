package main

// Edge cases that had no test, found from a coverage profile of the suite:
// each test pins one boundary or error branch so a regression in it fails
// here instead of on the unit.

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A WiFi QR payload must backslash-escape \ ; , : " in both fields, or a
// phone's camera parses the wrong SSID/password (or nothing at all).
func TestWifiQRContentEscapesSpecialCharacters(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	origSSID, origPass := wifiSSID, wifiPassword
	defer func() { wifiSSID, wifiPassword = origSSID, origPass }()

	wifiSSID, wifiPassword = `Stage;A,B:C`, `p\a"ss:word`
	want := `WIFI:T:WPA;S:Stage\;A\,B\:C;P:p\\a\"ss\:word;;`
	if got := wifiQRContent(); got != want {
		t.Fatalf("QR payload\n got %s\nwant %s", got, want)
	}
	wifiSSID, wifiPassword = "PI9696", "plainpass"
	if got := wifiQRContent(); got != "WIFI:T:WPA;S:PI9696;P:plainpass;;" {
		t.Fatalf("plain fields altered: %s", got)
	}
}

// hostapd's config is line-based: a newline in the SSID or passphrase would
// inject directives. sanitizeHostapd is the last guard before the file.
func TestSanitizeHostapdStripsInjection(t *testing.T) {
	for in, want := range map[string]string{
		"PI9696":                     "PI9696",
		"evil\nctrl_interface=/tmp":  "evilctrl_interface=/tmp",
		"a\r\nb":                     "ab",
		`quo"te\back`:                "quoteback",
		"spaces and-dashes_are fine": "spaces and-dashes_are fine",
	} {
		if got := sanitizeHostapd(in); got != want {
			t.Errorf("sanitizeHostapd(%q) = %q, want %q", in, got, want)
		}
	}
}

// encoding/json refuses +-Inf and NaN, so one silent block (astats -inf)
// would blank every meter payload; both sanitizers must map them to the
// silence sentinel and pass finite values through.
func TestMeterDBSanitizersRejectNonFinite(t *testing.T) {
	for _, f := range []func(float64) float64{sanitizeMeterDB, jsonSafeDB} {
		for _, v := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
			if got := f(v); got != meterSilence {
				t.Errorf("non-finite %v -> %v, want %v", v, got, meterSilence)
			}
		}
		for _, v := range []float64{0, -6.02, -99.9, 3} {
			if got := f(v); got != v {
				t.Errorf("finite %v altered to %v", v, got)
			}
		}
	}
}

// The encoder can drive the channel count past either end; it must clamp
// to 1..MaxChannelCount, and shrinking must pull the VU browse page back
// inside the new page count.
func TestAdjustChannelCountClampsAndFixesBrowsePage(t *testing.T) {
	initTestHardware(t) // restores channelCount
	mutex.Lock()
	defer mutex.Unlock()
	origPage := idleBrowsePage
	defer func() { idleBrowsePage = origPage }()

	channelCount = 1
	adjustChannelCount(-5)
	if channelCount != 1 {
		t.Fatalf("below 1: channelCount = %d", channelCount)
	}
	channelCount = MaxChannelCount
	adjustChannelCount(+3)
	if channelCount != MaxChannelCount {
		t.Fatalf("above max: channelCount = %d", channelCount)
	}
	idleBrowsePage = idleVUPageCount() + 1 // last page at 128 channels
	channelCount = 2
	adjustChannelCount(0)
	if total := idleVUPageCount() + 2; idleBrowsePage >= total {
		t.Fatalf("browse page %d left past the last page (%d pages)", idleBrowsePage, total)
	}
}

// Tag presets wrap in both directions, including steps larger than the list.
func TestAdjustRecordTagWraps(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	orig := tagPresetIdx
	defer func() { tagPresetIdx = orig }()
	n := len(tagPresets)
	tagPresetIdx = 0
	adjustRecordTag(-1)
	if tagPresetIdx != n-1 {
		t.Fatalf("0 - 1 = %d, want %d", tagPresetIdx, n-1)
	}
	adjustRecordTag(+1)
	if tagPresetIdx != 0 {
		t.Fatalf("wrap forward = %d, want 0", tagPresetIdx)
	}
	adjustRecordTag(-(2*n + 1))
	if tagPresetIdx != n-1 {
		t.Fatalf("large negative step = %d, want %d", tagPresetIdx, n-1)
	}
}

// Status texts for the OLED and dashboard: every Inferno state, the "None"
// tag, the "Default" prefix, and the meter readout at silence.
func TestStatusTextsCoverEveryState(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	origState, origTag, origPrefix := infernoState, tagPresetIdx, filePrefix
	origPeak, origRMS := meterPeakDB, meterRMSDB
	defer func() {
		infernoState, tagPresetIdx, filePrefix = origState, origTag, origPrefix
		meterPeakDB, meterRMSDB = origPeak, origRMS
	}()
	for st, want := range map[InfernoState]string{
		InfernoRunning: "Running", InfernoStarting: "Starting...", InfernoFailed: "Failed", InfernoStopped: "Stopped",
	} {
		infernoState = st
		if got := getInfernoStatusText(); got != want {
			t.Errorf("state %v: %q, want %q", st, got, want)
		}
	}
	for i, tag := range tagPresets {
		tagPresetIdx = i
		want := tag
		if tag == "" {
			want = "None"
		}
		if got := tagStatusText(); got != want {
			t.Errorf("tag %d: %q, want %q", i, got, want)
		}
	}
	filePrefix = ""
	if prefixStatusText() != "Default" || effectiveFilePrefix() != defaultFilePrefix {
		t.Errorf("empty prefix: status %q, effective %q", prefixStatusText(), effectiveFilePrefix())
	}
	filePrefix = "Live"
	if prefixStatusText() != "Live" || effectiveFilePrefix() != "Live" {
		t.Errorf("set prefix: status %q, effective %q", prefixStatusText(), effectiveFilePrefix())
	}
	meterPeakDB, meterRMSDB = meterSilence, meterSilence
	if got := formatMeter(); got != "Peak: -- RMS: --" {
		t.Errorf("silent meter: %q", got)
	}
	meterPeakDB, meterRMSDB = -6.04, -18.26
	if got := formatMeter(); got != "Peak: -6.0dB  RMS: -18.3dB" {
		t.Errorf("meter: %q", got)
	}
}

// Device names: 1-32 bytes of letters, digits, space, _ and -. The length
// bound is exact, and anything a hostname or path could choke on is refused.
func TestIsValidDeviceNameBounds(t *testing.T) {
	for name, want := range map[string]bool{
		"":                      false,
		"PI9696":                true,
		"Stage Left_Rec-2":      true,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"name/../etc":           false,
		"tab\tname":             false,
		"café":                  false, // non-ASCII
		"semi;colon":            false,
		strings.Repeat("é", 10): false,
	} {
		if got := isValidDeviceName(name); got != want {
			t.Errorf("isValidDeviceName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Deployment overrides read from the environment.
func TestEnvOverrides(t *testing.T) {
	t.Setenv("PI9696_INFERNO_BIN", "")
	if got := infernoBinary(); got != "inferno/target/release/inferno2pipe" {
		t.Errorf("default inferno binary = %q", got)
	}
	t.Setenv("PI9696_INFERNO_BIN", "/opt/custom/inferno2pipe")
	if got := infernoBinary(); got != "/opt/custom/inferno2pipe" {
		t.Errorf("PI9696_INFERNO_BIN ignored: %q", got)
	}
	t.Setenv("PI9696_STATIME_OBS", "")
	if got := statimeObservationPathFromEnv(); got != "/run/statime/observation.sock" {
		t.Errorf("default statime socket = %q", got)
	}
	t.Setenv("PI9696_STATIME_OBS", "/tmp/obs.sock")
	if got := statimeObservationPathFromEnv(); got != "/tmp/obs.sock" {
		t.Errorf("PI9696_STATIME_OBS ignored: %q", got)
	}
}

func postFormTo(h http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Every index-based WebUI setting must ignore an out-of-range or
// non-numeric value instead of storing it: a stored bad index would panic
// the next render that indexes the option slice.
func TestSettingsHandlersRejectOutOfRange(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	origVU, origHold, origRate, origTag := vuRangeIdx, peakHoldIdx, sampleRateIdx, tagPresetIdx
	origMode, origBright, origLog := transportMode, oledBrightnessPct, currentLogLevel()
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		vuRangeIdx, peakHoldIdx, sampleRateIdx, tagPresetIdx = origVU, origHold, origRate, origTag
		transportMode, oledBrightnessPct = origMode, origBright
		applyLogLevel(origLog)
		mutex.Unlock()
	})
	snapshot := func() []any {
		mutex.Lock()
		defer mutex.Unlock()
		return []any{vuRangeIdx, peakHoldIdx, sampleRateIdx, tagPresetIdx, transportMode, oledBrightnessPct, currentLogLevel()}
	}
	before := snapshot()
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		key  string
		bad  []string
	}{
		{"vu-range", handleAPISettingsVURange, "idx", []string{"-1", "99", "abc", ""}},
		{"peak-hold", handleAPISettingsPeakHold, "idx", []string{"-1", "99", "x"}},
		{"sample-rate", handleAPISettingsSampleRate, "idx", []string{"-1", "99", "48000"}},
		{"tag", handleAPISettingsTag, "idx", []string{"-1", "99", "Show"}},
		{"transport-mode", handleAPISettingsTransportMode, "idx", []string{"-1", "2", "text"}},
		{"brightness", handleAPISettingsBrightness, "pct", []string{"-1", "101", "50%"}},
		{"log-level", handleAPISettingsLogLevel, "idx", []string{"-1", "99", "debug"}},
	} {
		for _, v := range c.bad {
			rec := postFormTo(c.h, url.Values{c.key: {v}})
			if rec.Code >= 500 {
				t.Errorf("%s=%q: status %d", c.name, v, rec.Code)
			}
		}
	}
	after := snapshot()
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("setting %d changed by invalid input: %v -> %v", i, before[i], after[i])
		}
	}
	// And the boundary values themselves are accepted.
	postFormTo(handleAPISettingsVURange, url.Values{"idx": {"0"}})
	postFormTo(handleAPISettingsBrightness, url.Values{"pct": {"100"}})
	postFormTo(handleAPISettingsTransportMode, url.Values{"idx": {"1"}})
	mutex.Lock()
	vu, br, mode := vuRangeIdx, oledBrightnessPct, transportMode
	mutex.Unlock()
	if vu != 0 || br != 100 || mode != "text" {
		t.Errorf("boundary values refused: vu=%d brightness=%d mode=%q", vu, br, mode)
	}
}

// The free-text prefix: invalid input is a 400 that leaves the prefix
// alone, whitespace-only resets to the default, and surrounding spaces are
// trimmed off a valid value.
func TestPrefixHandlerEdges(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	orig := filePrefix
	filePrefix = "Keep"
	mutex.Unlock()
	t.Cleanup(func() { mutex.Lock(); filePrefix = orig; mutex.Unlock() })

	for _, bad := range []string{"has_underscore", "slash/x", strings.Repeat("a", 33), "dot.dot"} {
		rec := postFormTo(handleAPISettingsPrefix, url.Values{"prefix": {bad}})
		mutex.Lock()
		got := filePrefix
		mutex.Unlock()
		if rec.Code != http.StatusBadRequest || got != "Keep" {
			t.Errorf("prefix %q: status %d, prefix now %q", bad, rec.Code, got)
		}
	}
	postFormTo(handleAPISettingsPrefix, url.Values{"prefix": {"  Live Set  "}})
	mutex.Lock()
	trimmed := filePrefix
	mutex.Unlock()
	if trimmed != "Live Set" {
		t.Errorf("valid prefix stored as %q", trimmed)
	}
	postFormTo(handleAPISettingsPrefix, url.Values{"prefix": {"   "}})
	mutex.Lock()
	reset := filePrefix
	mutex.Unlock()
	if reset != "" {
		t.Errorf("whitespace prefix did not reset to default: %q", reset)
	}
}

// Downloads: the take being written is never served (a half-finalized WAV),
// traversal and unknown names are 404, and a finished take is served.
func TestDownloadEdges(t *testing.T) {
	initTestHardware(t)
	os.MkdirAll(RecordPath, 0755)
	name := "dl_20990101_120000_ch2_48kHz.wav"
	path := filepath.Join(RecordPath, name)
	os.WriteFile(path, []byte("RIFFdata"), 0644)
	t.Cleanup(func() { os.Remove(path) })
	mutex.Lock()
	origRec, origFile := isRecording, recordingFile
	mutex.Unlock()
	t.Cleanup(func() { mutex.Lock(); isRecording, recordingFile = origRec, origFile; mutex.Unlock() })

	get := func(rel string) int {
		req := httptest.NewRequest("GET", "/download/x", nil)
		req.SetPathValue("filepath", rel)
		rec := httptest.NewRecorder()
		handleDownload(rec, req)
		return rec.Code
	}
	mutex.Lock()
	isRecording, recordingFile = true, path
	mutex.Unlock()
	if code := get(name); code != http.StatusNotFound {
		t.Errorf("in-progress take served: %d", code)
	}
	mutex.Lock()
	isRecording = false
	mutex.Unlock()
	if code := get(name); code != http.StatusOK {
		t.Errorf("finished take: %d, want 200", code)
	}
	for _, rel := range []string{"../" + name, "..", "", "nope_20990101_120000_ch2_48kHz.wav", "/etc/passwd"} {
		if code := get(rel); code != http.StatusNotFound {
			t.Errorf("download %q: %d, want 404", rel, code)
		}
	}
}

// A locked-out address gets a clean slate once the minute has passed, and
// stale never-locked entries are swept rather than kept forever.
func TestLoginLimiterLockoutExpiresAndSweeps(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < loginMaxAttempts; i++ {
		l.allowed("203.0.113.5")
		l.recordFailure("203.0.113.5")
	}
	if l.allowed("203.0.113.5") {
		t.Fatal("not locked after max failures")
	}
	l.mu.Lock()
	l.lockedAt["203.0.113.5"] = time.Now().Add(-61 * time.Second)
	l.seenAt["198.51.100.1"] = time.Now().Add(-2 * time.Minute) // stale prober
	l.failures["198.51.100.1"] = 3
	l.mu.Unlock()
	if !l.allowed("203.0.113.5") {
		t.Fatal("still locked after the lockout expired")
	}
	l.mu.Lock()
	failures := l.failures["203.0.113.5"]
	_, staleSeen := l.seenAt["198.51.100.1"]
	_, staleFail := l.failures["198.51.100.1"]
	l.mu.Unlock()
	if failures != 1 {
		t.Errorf("failures after expiry = %d, want 1 (this attempt only)", failures)
	}
	if staleSeen || staleFail {
		t.Error("stale entry for an idle address was not swept")
	}
}

// /proc/stat parsing: per-core lines only (not the aggregate "cpu" line),
// total = sum of all fields, idle = idle + iowait; short lines skipped.
func TestParseCPUStat(t *testing.T) {
	data := "cpu  10 0 10 80 0 0 0 0 0 0\n" +
		"cpu0 1 2 3 4 5 6 7 8 9 10\n" +
		"cpu1 100 0 0 900\n" + // 4 values, no iowait: too short, skipped
		"cpu3 100 0 0 800 100\n" + // 5 values (older kernels): accepted
		"cpu2 0 0 0 50 50 0 0 0 0 0\n" +
		"intr 12345\n" +
		"ctxt 99\n"
	got := parseCPUStat(data)
	want := []cpuStat{{55, 4 + 5}, {1000, 900}, {100, 100}}
	if len(got) != len(want) {
		t.Fatalf("parsed %d cores (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("core %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := parseCPUStat(""); len(s) != 0 {
		t.Errorf("empty input parsed as %v", s)
	}
}

// Peak hold: a held peak stays inside the hold window, then falls 2 dB per
// tick but never below the instant level; "no hold" falls at once; a
// channel-count change re-seeds the held values; no channels clears them.
func TestDecayPeakHoldEdges(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	origPeak, origHeld, origSet, origIdx := meterChannelPeak, meterChannelPeakHeld, peakHeldSetAt, peakHoldIdx
	defer func() {
		meterChannelPeak, meterChannelPeakHeld, peakHeldSetAt, peakHoldIdx = origPeak, origHeld, origSet, origIdx
	}()

	peakHoldIdx = 2 // 1 s
	meterChannelPeak = []float64{-10}
	meterChannelPeakHeld = nil
	decayPeakHold() // seeds
	meterChannelPeak = []float64{-40}
	decayPeakHold()
	if meterChannelPeakHeld[0] != -10 {
		t.Fatalf("inside hold window: held %v, want -10", meterChannelPeakHeld[0])
	}
	peakHeldSetAt[0] = time.Now().Add(-2 * time.Second)
	decayPeakHold()
	if meterChannelPeakHeld[0] != -12 {
		t.Fatalf("after hold window: held %v, want -12", meterChannelPeakHeld[0])
	}
	meterChannelPeakHeld[0] = -39
	decayPeakHold()
	if meterChannelPeakHeld[0] != -40 {
		t.Fatalf("decay went below the instant level: %v", meterChannelPeakHeld[0])
	}

	peakHoldIdx = 0 // no hold: falls on the next tick
	meterChannelPeak = []float64{-5}
	meterChannelPeakHeld, peakHeldSetAt = []float64{-5}, []time.Time{time.Now()}
	meterChannelPeak = []float64{-50}
	decayPeakHold()
	if meterChannelPeakHeld[0] != -7 {
		t.Fatalf("no-hold decay: %v, want -7", meterChannelPeakHeld[0])
	}

	meterChannelPeak = []float64{-3, -4, -5}
	decayPeakHold()
	if len(meterChannelPeakHeld) != 3 || meterChannelPeakHeld[2] != -5 {
		t.Fatalf("channel change not re-seeded: %v", meterChannelPeakHeld)
	}
	meterChannelPeak = nil
	decayPeakHold()
	if meterChannelPeakHeld != nil || peakHeldSetAt != nil {
		t.Fatal("no channels: held values not cleared")
	}
}

// Every clock-sync state the dashboard can show.
func TestClockSyncTextBranches(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	orig := clockSync
	defer func() { clockSync = orig }()
	now := time.Now()

	clockSync = clockSyncStatus{}
	if got := clockSyncTextLocked(now); got != "Checking" {
		t.Errorf("never checked: %q", got)
	}
	clockSync = clockSyncStatus{checked: now}
	if got := clockSyncTextLocked(now); got != "Not synced (statime unreachable)" {
		t.Errorf("unreachable: %q", got)
	}
	clockSync = clockSyncStatus{checked: now, portState: "Slave", offset: 8 * time.Microsecond, goodStreak: clockSyncStableSamples}
	if got := clockSyncTextLocked(now); got != "Synced (PTP slave, 8µs)" {
		t.Errorf("synced: %q", got)
	}
	clockSync.goodStreak = 1
	if got := clockSyncTextLocked(now); got != "Locking (PTP slave, 8µs)" {
		t.Errorf("locking: %q", got)
	}
	clockSync = clockSyncStatus{checked: now, portState: "Listening"}
	if got := clockSyncTextLocked(now); got != "Not synced (PTP Listening)" {
		t.Errorf("other state: %q", got)
	}
	// A synced status that has not been refreshed for 3 polls is stale.
	clockSync = clockSyncStatus{checked: now.Add(-4 * clockSyncPollInterval), portState: "Slave", goodStreak: clockSyncStableSamples}
	if strings.HasPrefix(clockSyncTextLocked(now), "Synced") {
		t.Error("stale sync reported as synced")
	}
}

// HyperDeck transport fields: status/speed, clip id and timecode position
// for idle, recording, playing and paused.
func TestHyperdeckTransportFields(t *testing.T) {
	initTestHardware(t)
	resetTransportCleanup(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "hd_20990101_120000_ch2_48kHz.wav")
	b := filepath.Join(dir, "hd_20990101_120100_ch2_48kHz.wav")
	os.WriteFile(a, []byte("x"), 0644)
	os.WriteFile(b, []byte("x"), 0644)
	mutex.Lock()
	defer mutex.Unlock()
	origPath, origRec, origFile, origStart := RecordPath, isRecording, recordingFile, recordStart
	defer func() { RecordPath, isRecording, recordingFile, recordStart = origPath, origRec, origFile, origStart }()
	RecordPath = dir
	files := recordingFiles()
	if len(files) != 2 {
		t.Fatalf("fixture listing: %v", files)
	}

	currentState, isRecording, playbackFile = StateIdle, false, ""
	if s, sp := hyperdeckStatusLocked(); s != "stopped" || sp != 0 {
		t.Errorf("idle: %s %d", s, sp)
	}
	if id := hyperdeckClipIDLocked(); id != "none" {
		t.Errorf("idle clip id %q", id)
	}
	if e := hyperdeckElapsedLocked(); e != 0 {
		t.Errorf("idle elapsed %v", e)
	}

	isRecording, recordingFile, recordStart = true, files[1], time.Now().Add(-3*time.Second)
	if s, sp := hyperdeckStatusLocked(); s != "record" || sp != 100 {
		t.Errorf("recording: %s %d", s, sp)
	}
	if id := hyperdeckClipIDLocked(); id != "1" {
		t.Errorf("recording clip id %q, want 1", id)
	}
	if e := hyperdeckElapsedLocked(); e < 3*time.Second || e > 5*time.Second {
		t.Errorf("recording elapsed %v", e)
	}
	recordingFile = filepath.Join(dir, "not-listed-yet.wav")
	if id := hyperdeckClipIDLocked(); id != "2" {
		t.Errorf("unlisted active take clip id %q, want len(files)=2", id)
	}

	isRecording = false
	currentState, playbackFile, playbackStart = StatePlaying, files[0], time.Now().Add(-2*time.Second)
	if s, sp := hyperdeckStatusLocked(); s != "play" || sp != 100 {
		t.Errorf("playing: %s %d", s, sp)
	}
	if id := hyperdeckClipIDLocked(); id != "0" {
		t.Errorf("playing clip id %q, want 0", id)
	}
	currentState, playbackPausedElapsed = StatePaused, 7*time.Second
	if s, sp := hyperdeckStatusLocked(); s != "stopped" || sp != 0 {
		t.Errorf("paused: %s %d", s, sp)
	}
	if e := hyperdeckElapsedLocked(); e != 7*time.Second {
		t.Errorf("paused elapsed %v, want 7s", e)
	}
}

// HyperDeck commands at idle: seeks need a track ("not playing"),
// shuttle-to-zero is a stop (ok), only slot 1 exists, and clips get lists
// every take with its 0-based id.
func TestHyperdeckIdleCommandEdges(t *testing.T) {
	initTestHardware(t)
	resetTransportCleanup(t)
	dir := t.TempDir()
	for _, n := range []string{"cg_20990101_120000_ch2_48kHz.wav", "cg_20990101_120100_ch2_48kHz.wav"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0644)
	}
	mutex.Lock()
	origPath := RecordPath
	RecordPath = dir
	currentState = StateIdle
	mutex.Unlock()
	t.Cleanup(func() { mutex.Lock(); RecordPath = origPath; mutex.Unlock() })

	sc, c, cleanup := hyperdeckDial(t)
	defer cleanup()
	for cmd, wantPrefix := range map[string]string{
		"jog: speed: 1":           "103",
		"shuttle: speed: -200":    "103",
		"goto: timeline: 0":       "103",
		"shuttle: speed: 0":       "200",
		"slot select: slot id: 2": "102",
		"slot select: slot id: 1": "200",
		"slot info":               "202",
	} {
		if first, _ := hyperdeckCmd(t, sc, c, cmd); !strings.HasPrefix(first, wantPrefix) {
			t.Errorf("%q -> %q, want %s", cmd, first, wantPrefix)
		}
	}
	first, lines := hyperdeckCmd(t, sc, c, "clips get")
	if !strings.HasPrefix(first, "206") {
		t.Fatalf("clips get -> %q", first)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"clip id: 0", "clip id: 1", "name: cg_20990101_120000_ch2_48kHz.wav", "name: cg_20990101_120100_ch2_48kHz.wav"} {
		if !strings.Contains(joined, want) {
			t.Errorf("clips get lacks %q:\n%s", want, joined)
		}
	}
}

// Mid-take auto-stop: unstattable storage counts as "stop now", an unknown
// (0) estimate does not, and the threshold is exclusive of 0.
func TestShouldAutoStopTakeEdges(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	orig := RecordPath
	defer func() { RecordPath = orig }()
	RecordPath = t.TempDir()
	for remaining, want := range map[time.Duration]bool{
		0:                            false, // unknown rate, not "full"
		30 * time.Second:             true,
		midTakeDiskStopThreshold - 1: true,
		midTakeDiskStopThreshold:     false,
		10 * time.Minute:             false,
	} {
		if got := shouldAutoStopTake(remaining); got != want {
			t.Errorf("remaining %v: %v, want %v", remaining, got, want)
		}
	}
	RecordPath = filepath.Join(t.TempDir(), "unmounted", "rec")
	if !shouldAutoStopTake(10 * time.Minute) {
		t.Error("unstattable storage did not stop the take")
	}
	if !lowDisk() {
		t.Error("unstattable storage not reported as low disk")
	}
}

// The mid-take check stops a take whose storage vanished (a pulled or
// remounted disk), and is throttled to once a second.
func TestMidTakeDiskCheckStopsTakeOnLostStorage(t *testing.T) {
	startFakeTake(t)
	mutex.Lock()
	origPath, origCheck, origWarn := RecordPath, lastMidTakeDiskCheck, diskWarnUntil
	done := recordingDone
	RecordPath = filepath.Join(t.TempDir(), "gone", "rec")
	diskWarnUntil = time.Time{}
	lastMidTakeDiskCheck = time.Now() // inside the 1s throttle
	checkMidTakeDiskLocked()
	// stopRecording is asynchronous; the warning is set synchronously, so
	// it is what shows whether the throttled call acted.
	throttled := diskWarnUntil.IsZero() && isRecording
	lastMidTakeDiskCheck = time.Time{}
	checkMidTakeDiskLocked()
	warned := time.Now().Before(diskWarnUntil)
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		RecordPath, lastMidTakeDiskCheck, diskWarnUntil = origPath, origCheck, origWarn
		mutex.Unlock()
	})
	if !throttled {
		t.Fatal("check ran inside its 1s throttle")
	}
	waitReaped(t, done, "auto-stopped take")
	mutex.Lock()
	rec := isRecording
	mutex.Unlock()
	if rec {
		t.Fatal("take still recording on lost storage")
	}
	if !warned {
		t.Error("no LOW DISK warning raised")
	}
}

// USB copy: a cancel mid-file leaves neither the destination nor its .tmp
// behind, and a copy into a not-yet-existing day folder creates it.
func TestCopyFileCancelAndSubdir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "take.wav")
	if err := os.WriteFile(src, make([]byte, 3<<20), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "2099-01-01", "take.wav")
	calls := 0
	err := copyFile(src, dst, func() bool { calls++; return calls > 1 })
	if !errors.Is(err, errCopyCancelled) {
		t.Fatalf("cancel mid-file: err %v", err)
	}
	for _, p := range []string{dst, dst + ".tmp"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left behind after cancel", p)
		}
	}
	if err := copyFile(src, dst, func() bool { return false }); err != nil {
		t.Fatalf("copy into new day folder: %v", err)
	}
	if st, err := os.Stat(dst); err != nil || st.Size() != 3<<20 {
		t.Fatalf("copied file: %v %v", st, err)
	}
	if err := copyFile(filepath.Join(t.TempDir(), "missing.wav"), dst+"2", func() bool { return false }); err == nil {
		t.Error("missing source copied without error")
	}
}
