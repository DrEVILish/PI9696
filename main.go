package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"pi9696/hardware"

	"github.com/skip2/go-qrcode"
)

const (
	DisplayWidth        = 256
	DisplayHeight       = 64
	MaxChannelCount     = 128
	BitsPerSample       = 32 // internal FIFO/pipeline sample width, shown in status bar
	OutputBitsPerSample = 24 // actual pcm_s24le WAV written to disk, used for storage math
	RecordPath          = "/rec"
	RawPath             = "/rec/raw"
	USBMountPoint       = "/media/usb"
	meterSilence        = -100.0 // dB sentinel shown/reported when no recording is active
)

// InfernoBinary is the prebuilt Inferno server executable, produced once at
// install time (`cargo build --release`) rather than compiled at runtime. It's
// relative to the app's working directory (the systemd unit runs the app from
// its project dir). See the inferno template README for the CLI
// contract it implements.
const InfernoBinary = "inferno/target/release/inferno"

// ConfigPath is where the app persists non-destructive settings (unit name,
// format/channel/tag choices, meter preferences, WiFi config) across
// restarts. In sim/dev mode it defaults to a per-user file so nothing needs
// root; on a real Pi it lives in /etc. Override with PI9696_CONFIG.
var ConfigPath string

func init() {
	if p := os.Getenv("PI9696_CONFIG"); p != "" {
		ConfigPath = p
	} else if isSimMode() {
		ConfigPath = "/tmp/pi9696-config.json"
	} else {
		ConfigPath = "/etc/pi9696/config.json"
	}
}

// isSimMode mirrors hardware.simMode: PI9696_SIM=1 runs without real
// SPI/GPIO on a dev machine. gpioDetecting sim is needed in main too (config
// path, WiFi/AP no-ops), so it's re-derived here rather than exported.
func isSimMode() bool {
	return os.Getenv("PI9696_SIM") != ""
}

// OLED display brightness + auto-dim (Round 3 design decision).
//
// Brightness is a continuous 0-100% setting applied to the SSD1322's
// contrast current (see TTFDisplay.SetBrightness), settable from the OLED
// Settings -> Display submenu and the WebUI settings modal.
//
// Auto-dim keeps an idle panel dim instead of blazing at full brightness
// (an OLED's power draw is pixel-proportional, and this unit powers on
// continuously): after dimTimeout of no input the panel drops to
// dimDimBrightnessPct, after dimOffTimeout it goes to 0 (effectively off),
// and any input (encoder or buttons, including the WebUI equivalents, which
// flow through the same handlers) wakes it back to the user's brightness.
// It is a display-saver for idle use only: during an active recording or
// playback session the panel stays at full brightness (see
// displaySessionActive), because the OLED is the operator's live status
// surface there and must not go dark on its own.
const (
	dimTimeout          = 30 * time.Second
	dimOffTimeout       = 2 * time.Minute
	dimDimBrightnessPct = 20
)

// oledBrightnessPct is the user's display brightness (0-100), always the
// wake-from-dim target and normally the live value too.
var oledBrightnessPct = 100

// autoDimEnabled toggles the dim-then-off behavior. Default on.
var autoDimEnabled = true

// menuTimeoutOptions are the selectable idle delays after which an
// untouched OLED menu falls back to the Standby status screen; 0 is Off
// (the "yes/no" half of the setting - menus stay put until dismissed).
var menuTimeoutOptions = []time.Duration{0, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}

// menuTimeoutIdx selects into menuTimeoutOptions; 30s default.
var menuTimeoutIdx = 2

// lastInputTime is the most recent encoder/button/WebUI input, fed by
// noteActivity; zero until the app boots so a stale default can't trigger an
// immediate dim.
var lastInputTime time.Time

// displayDimState is the current auto-dim stage: 0 = full brightness,
// 1 = dimmed, 2 = off. Transitions are applied once in applyAutoDimLocked.
var displayDimState int

// loadPersistedConfig reads ConfigPath and restores the non-destructive
// settings onto the globals, clamping out-of-range values so a hand-edited
// config can't push an index past its slice. Called once at startup before
// the UI/loops start. A missing/corrupt config is not fatal - defaults stand.
func loadPersistedConfig() {
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		logDebugf("No persisted config at %s (%v) - using defaults", ConfigPath, err)
		if isSimMode() {
			applyLogLevel(LogDebug)
		}
		return
	}
	var c PersistedConfig
	if err := json.Unmarshal(data, &c); err != nil {
		logErrorf("Corrupt config at %s (%v) - using defaults", ConfigPath, err)
		if isSimMode() {
			applyLogLevel(LogDebug)
		}
		return
	}

	if c.DeviceName != "" && isValidDeviceName(c.DeviceName) {
		deviceName = c.DeviceName
	}
	if c.SampleRateIdx >= 0 && c.SampleRateIdx < len(sampleRates) {
		sampleRateIdx = c.SampleRateIdx
	}
	if c.ChannelCount >= 1 && c.ChannelCount <= MaxChannelCount {
		channelCount = c.ChannelCount
	}
	if c.TagPresetIdx >= 0 && c.TagPresetIdx < len(tagPresets) {
		tagPresetIdx = c.TagPresetIdx
	}
	if c.FilePrefix == "" || isValidFilePrefix(c.FilePrefix) {
		filePrefix = c.FilePrefix
	}
	if c.VURangeIdx >= 0 && c.VURangeIdx < len(vuRangeOptions) {
		vuRangeIdx = c.VURangeIdx
	}
	if c.PeakHoldIdx >= 0 && c.PeakHoldIdx < len(peakHoldOptions) {
		peakHoldIdx = c.PeakHoldIdx
	}
	if c.Theme != "" && isKnownTheme(c.Theme) {
		themeSlug = c.Theme
	}
	if c.TransportMode == "icon" || c.TransportMode == "text" {
		transportMode = c.TransportMode
	}
	if c.LogLevelIdx >= 0 && c.LogLevelIdx < len(logLevelNames) {
		applyLogLevel(LogLevel(c.LogLevelIdx))
	}
	if c.OledBrightnessPct != nil {
		if *c.OledBrightnessPct >= 0 && *c.OledBrightnessPct <= 100 {
			oledBrightnessPct = *c.OledBrightnessPct
		}
	}
	autoDimEnabled = !c.AutoDimDisabled
	if c.MenuTimeoutIdx >= 0 && c.MenuTimeoutIdx < len(menuTimeoutOptions) {
		menuTimeoutIdx = c.MenuTimeoutIdx
	}
	demoMode = c.DemoMode
	hyperdeckEnabled = c.HyperdeckEnabled

	wifiEnabled = c.WifiEnabled
	wifiSSID = c.WifiSSID
	wifiPassword = c.WifiPassword

	logInfof("Loaded persisted config from %s (device %q, %dkHz %dch WAV, log=%s)",
		ConfigPath, deviceName, sampleRates[sampleRateIdx]/1000, channelCount, logLevelNames[int(currentLogLevel())])
}

// persistConfig snapshots the current non-destructive settings to ConfigPath.
// Safe to call under the app mutex (it only reads globals); the write is
// atomic via a temp file + rename so a power cut mid-write can't truncate it.
func persistConfig() {
	cur := PersistedConfig{
		DeviceName:        deviceName,
		SampleRateIdx:     sampleRateIdx,
		ChannelCount:      channelCount,
		TagPresetIdx:      tagPresetIdx,
		FilePrefix:        filePrefix,
		VURangeIdx:        vuRangeIdx,
		PeakHoldIdx:       peakHoldIdx,
		TransportMode:     transportMode,
		Theme:             themeSlug,
		LogLevelIdx:       int(currentLogLevel()),
		OledBrightnessPct: &oledBrightnessPct,
		AutoDimDisabled:   !autoDimEnabled,
		MenuTimeoutIdx:    menuTimeoutIdx,
		DemoMode:          demoMode,
		HyperdeckEnabled:  hyperdeckEnabled,
		WifiEnabled:       wifiEnabled,
		WifiSSID:          wifiSSID,
		WifiPassword:      wifiPassword,
	}
	data, err := json.MarshalIndent(&cur, "", "  ")
	if err != nil {
		logErrorf("Failed to marshal config: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(ConfigPath), 0755); err != nil {
		logErrorf("Failed to create config dir: %v", err)
		return
	}
	tmp := ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logErrorf("Failed to write config: %v", err)
		return
	}
	if err := os.Rename(tmp, ConfigPath); err != nil {
		logErrorf("Failed to commit config: %v", err)
	}
	logDebugf("Persisted settings to %s", ConfigPath)
}

// settingChanged is a tiny helper for the handful of places a persistent
// setting is mutated, so they all funnel through persistConfig.
func settingChanged() {
	persistConfig()
}

// configExportName is the config profile a unit writes/reads on its USB drive
// (System -> Export/Import Config, or the WebUI Config group). It is plain
// JSON that a newer unit can still read (unknown fields are dropped by Go;
// absent fields keep the importer's current value via the clamps below).
const configExportName = "pi9696-config.json"

// exportConfig writes the current non-destructive settings to the USB drive
// as JSON, with the WiFi password blanked - config export is explicitly the
// non-secret profile for cloning a unit's setup (the access token is never
// persisted anywhere, so it is inherently excluded too). Must be called under
// the app mutex.
func exportConfig() error {
	if !usbMounted {
		return fmt.Errorf("no USB drive mounted")
	}
	return exportConfigTo(USBMountPoint)
}

// exportConfigTo does exportConfig's work into an arbitrary directory so the
// round-trip is testable without a mounted drive.
func exportConfigTo(dir string) error {
	profile := PersistedConfig{
		DeviceName:        deviceName,
		SampleRateIdx:     sampleRateIdx,
		ChannelCount:      channelCount,
		TagPresetIdx:      tagPresetIdx,
		FilePrefix:        filePrefix,
		VURangeIdx:        vuRangeIdx,
		PeakHoldIdx:       peakHoldIdx,
		TransportMode:     transportMode,
		Theme:             themeSlug,
		LogLevelIdx:       int(currentLogLevel()),
		OledBrightnessPct: &oledBrightnessPct,
		AutoDimDisabled:   !autoDimEnabled,
		MenuTimeoutIdx:    menuTimeoutIdx,
		DemoMode:          demoMode,
		HyperdeckEnabled:  hyperdeckEnabled,
		WifiEnabled:       wifiEnabled,
		WifiSSID:          wifiSSID,
		// WifiPassword deliberately omitted - it's a credential.
	}
	data, err := json.MarshalIndent(&profile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling config: %v", err)
	}
	dst := filepath.Join(dir, configExportName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %v", configExportName, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("committing %s: %v", configExportName, err)
	}
	logInfof("config exported to USB as %s", configExportName)
	return nil
}

// importConfig loads the USB profile written by exportConfig and applies it,
// clamping out-of-range indexes exactly like loadPersistedConfig does at boot
// (a hand-edited file can't push an index off its slice). The WiFi password
// is never adopted from the file - export blanks it, so the whole WiFi block
// is skipped unless the file actually carries a credential, preventing an
// import from turning on an open access point. Must be called under the app
// mutex.
func importConfig() error {
	if !usbMounted {
		return fmt.Errorf("no USB drive mounted")
	}
	return importConfigFrom(USBMountPoint)
}

// importConfigFrom does importConfig's work from an arbitrary directory so
// the round-trip is testable without a mounted drive.
func importConfigFrom(dir string) error {
	src := filepath.Join(dir, configExportName)
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("no %s on the USB drive: %v", configExportName, err)
	}
	var c PersistedConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("invalid config file: %v", err)
	}

	if c.DeviceName != "" && isValidDeviceName(c.DeviceName) {
		deviceName = c.DeviceName
	}
	if c.SampleRateIdx >= 0 && c.SampleRateIdx < len(sampleRates) {
		sampleRateIdx = c.SampleRateIdx
	}
	if c.ChannelCount >= 1 && c.ChannelCount <= MaxChannelCount {
		channelCount = c.ChannelCount
	}
	if c.TagPresetIdx >= 0 && c.TagPresetIdx < len(tagPresets) {
		tagPresetIdx = c.TagPresetIdx
	}
	if c.FilePrefix == "" || isValidFilePrefix(c.FilePrefix) {
		filePrefix = c.FilePrefix
	}
	if c.VURangeIdx >= 0 && c.VURangeIdx < len(vuRangeOptions) {
		vuRangeIdx = c.VURangeIdx
	}
	if c.PeakHoldIdx >= 0 && c.PeakHoldIdx < len(peakHoldOptions) {
		peakHoldIdx = c.PeakHoldIdx
	}
	if c.Theme != "" && isKnownTheme(c.Theme) {
		themeSlug = c.Theme
	}
	if c.TransportMode == "icon" || c.TransportMode == "text" {
		transportMode = c.TransportMode
	}
	if c.LogLevelIdx >= 0 && c.LogLevelIdx < len(logLevelNames) {
		applyLogLevel(LogLevel(c.LogLevelIdx))
	}
	if c.OledBrightnessPct != nil && *c.OledBrightnessPct >= 0 && *c.OledBrightnessPct <= 100 {
		oledBrightnessPct = *c.OledBrightnessPct
		if hwManager != nil {
			hwManager.SetBrightness(oledBrightnessPct)
		}
	}
	autoDimEnabled = !c.AutoDimDisabled
	if c.MenuTimeoutIdx >= 0 && c.MenuTimeoutIdx < len(menuTimeoutOptions) {
		menuTimeoutIdx = c.MenuTimeoutIdx
	}

	if c.WifiPassword != "" && c.WifiSSID != "" && len(c.WifiPassword) >= 8 && len(c.WifiSSID) <= 32 {
		wifiSSID, wifiPassword, wifiEnabled = c.WifiSSID, c.WifiPassword, c.WifiEnabled
		go applyWifiConfig(wifiSSID, wifiPassword, wifiEnabled)
	}

	checkInfernoRestart()
	persistConfig()
	logInfof("config imported from USB %s", configExportName)
	return nil
}

// sysNotice + sysNoticeUntil flash a one-shot status line on the idle screen
// for actions that have no screen of their own (e.g. "Config exported") - the
// same transient-message pattern as the low-disk refusal warning.
var sysNotice string
var sysNoticeUntil time.Time

func showSysNotice(msg string) {
	sysNotice = msg
	sysNoticeUntil = time.Now().Add(4 * time.Second)
}

// lastLoginPage is when the WebUI login page was last served (see
// handleLoginGet): while fresh, the idle screen shows the access token so
// the operator can read it straight off the panel. Guarded by the app mutex.
var lastLoginPage time.Time

// loginTokenShowFor is how long the idle screen keeps showing the token
// after the login page was opened - long enough to walk over and read it,
// short enough the token isn't parked on the panel indefinitely.
const loginTokenShowFor = 2 * time.Minute

func loginTokenFreshLocked() bool {
	return time.Since(lastLoginPage) < loginTokenShowFor
}

// applyAutoDimLocked advances the display's dim/off state to whatever the
// idle time dictates, applying a brightness transition only when the stage
// changes (SPI writes on every 100ms render are pointless churn). Must be
// called under the app mutex (render does). A zero lastInputTime (pre-boot)
// is treated as "active now" so the panel can't go dark before first render.
func applyAutoDimLocked(now time.Time) {
	if lastInputTime.IsZero() {
		lastInputTime = now
	}
	target := 0
	if autoDimEnabled && !displaySessionActive() {
		idle := now.Sub(lastInputTime)
		if idle > dimOffTimeout {
			target = 2
		} else if idle > dimTimeout {
			target = 1
		}
	}
	if target == displayDimState {
		return
	}
	displayDimState = target
	if hwManager == nil {
		return
	}
	switch target {
	case 2:
		hwManager.SetBrightness(0)
	case 1:
		hwManager.SetBrightness(dimDimBrightnessPct)
	default:
		hwManager.SetBrightness(oledBrightnessPct)
	}
}

// displaySessionActive reports whether the unit is in an active take or
// playback: states where the OLED is the operator's primary status surface
// (meters, transport, timecode) and must not dim/off just because the
// encoder hasn't been touched. Auto-dim is a display-saver for idle use, not
// for a live session the operator is watching.
func displaySessionActive() bool {
	return isRecording || playbackCmd != nil
}

// noteActivity records an input and wakes a dimmed/off display back to the
// user's brightness. Called from the encoder/button handlers (which hold the
// app mutex), so no locking here; the WebUI input endpoints route through the
// same handlers and therefore wake it too.
func noteActivity() {
	lastInputTime = time.Now()
	if displayDimState != 0 {
		displayDimState = 0
		if hwManager != nil {
			hwManager.SetBrightness(oledBrightnessPct)
		}
	}
}

// applyWifiConfig writes the hostapd configuration and starts/stops the
// PI9696 access point service. On a real Pi this drives systemctl; in
// sim/dev mode it only logs (there's no wlan0/AP hardware to touch, and a
// dev box's network must not be disturbed). Takes explicit args rather than
// reading globals so callers can capture the values under the app mutex and
// call it from a goroutine - applying the change can block on systemctl for a
// moment, which must never run under the UI mutex.
//
// WiFi is OFF by default: nothing enables it at install time, and wifiEnabled
// starts false unless the operator persisted an explicit on. wifiSSID (the AP
// name) defaults to the device name; the password is user-set via the web UI.
func applyWifiConfig(ssid, pass string, enabled bool) {
	if isSimMode() {
		logInfof("wifi: sim mode - AP %q enabled=%v (no hardware change)", ssid, enabled)
		wifiInited = true
		return
	}

	apConf := "/etc/hostapd/pi9696.conf"
	conf := "interface=wlan0\ndriver=nl80211\nssid=" + sanitizeHostapd(ssid) +
		"\nwpa=2\nwpa_passphrase=" + sanitizeHostapd(pass) +
		"\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nchannel=6\nhw_mode=g\nignore_broadcast_ssid=0\n"
	if err := os.WriteFile(apConf, []byte(conf), 0600); err != nil {
		logErrorf("wifi: failed to write %s: %v", apConf, err)
		return
	}

	var cmd *exec.Cmd
	if enabled {
		cmd = exec.Command("systemctl", "enable", "--now", "hostapd")
	} else {
		cmd = exec.Command("systemctl", "disable", "--now", "hostapd")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		logErrorf("wifi: hostapd %s failed: %v: %s", map[bool]string{true: "start", false: "stop"}[enabled], err, out)
	}
	wifiInited = true
	logInfof("wifi: access point %q %s", ssid, map[bool]string{true: "started", false: "stopped"}[enabled])
}

// setWifiEnabled flips the runtime AP state, persists it (so it's also the
// "on/off at startup" preference), and applies it off the UI mutex. Must be
// called under mutex (it reads wifiSSID/wifiPassword to pass to the apply
// goroutine).
func setWifiEnabled(on bool) {
	wifiEnabled = on
	ssid := wifiSSID
	pass := wifiPassword
	persistConfig()
	go applyWifiConfig(ssid, pass, on)
}

// updateWifiCredentials updates the AP SSID/password (e.g. set via the web
// dashboard) and re-applies the AP. Must be called under mutex.

// wifiQRContent builds the WiFi QR payload (WIFI: scheme) so a phone camera
// can join the AP directly. See renderWifiQRScreen.
func wifiQRContent() string {
	// WIFI:T:<security>;S:<ssid>;P:<password>;; - the de-facto standard QR
	// WiFi barcode format understood by iOS/Android camera apps.
	return fmt.Sprintf("WIFI:T:WPA;S:%s;P:%s;;", escapeWifiField(wifiSSID), escapeWifiField(wifiPassword))
}

// escapeWifiField escapes a WiFi QR text field per spec: \ ; , : " must be
// backslash-escaped or the code won't scan.
func escapeWifiField(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	return r.Replace(s)
}

// renderWifiQRScreen draws the WiFi join QR code plus the SSID/password text
// on the OLED, scaled to fit the 256x64 panel (the QR sits on the right,
// ~100px square, with the access point name and password read out on the
// left). Requires mutex held. If the AP is off or unconfigured it shows a
// short message instead.
func renderWifiQRScreen() {
	if !wifiEnabled {
		hwManager.DrawCenteredText("WiFi is OFF", "header", 28)
		hwManager.DrawCenteredText("enable in WiFi menu", "details", 40)
		return
	}

	hwManager.SwitchToContext("header")
	hwManager.DrawText(4, 14, "WiFi AP")

	hwManager.SwitchToContext("details")
	label := wifiSSID
	if label == "" {
		label = deviceName
	}
	hwManager.DrawText(4, 30, fitText("SSID "+label, 190))
	hwManager.DrawText(4, 42, fitText("Pass "+wifiPassword, 190))

	// QR access code, doubled to 2px modules where it fits (see
	// drawQRBitmapFit); the OLED black surround serves as quiet zone.
	bmp := qrBitmap(wifiQRContent())
	drawQRBitmapFit(bmp)
}

// qrBitmap returns a QR-code bitmap for content with the quiet-zone
// border disabled — the surrounding OLED black area acts as the quiet
// zone, letting us use 1px modules for the smallest possible code.
func qrBitmap(content string) [][]bool {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil
	}
	code.DisableBorder = true
	return code.Bitmap()
}

// drawQRBitmap renders a QR bitmap onto the OLED at modulePx pixels
// per module, top-left at (x0, y0).
func drawQRBitmap(bmp [][]bool, x0, y0, modulePx int) {
	if bmp == nil {
		return
	}
	hwManager.SwitchToContext("menu")
	n := len(bmp)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if bmp[i][j] {
				hwManager.FillBox(x0+j*modulePx, y0+i*modulePx, modulePx, modulePx, 0xFF)
			}
		}
	}
}

// drawQRBitmapFit renders a QR bitmap right-aligned at 2px modules when
// that fits the 64px panel height, else 1px, vertically centered - short
// payloads get a big code and long ones still fit. QR screens skip the
// status bar (see render) so a 2px code has the full height.
func drawQRBitmapFit(bmp [][]bool) {
	if bmp == nil {
		return
	}
	n := len(bmp)
	modulePx := 2
	if n*modulePx > DisplayHeight {
		modulePx = 1
	}
	size := n * modulePx
	drawQRBitmap(bmp, DisplayWidth-size, (DisplayHeight-size)/2, modulePx)
}

// fitText truncates s so it renders within maxPx in the current font
// context - keeps SSID/password/network lines clear of the QR code.
// Trims whole runes: cutting raw bytes could split a multi-byte UTF-8
// sequence and render garbage.
func fitText(s string, maxPx int) string {
	for len(s) > 0 && hwManager.GetTextWidth(s) > maxPx {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// signalTERM asks a transport process to exit. "Process already finished"
// is the normal race (the reaping goroutine got there first) and stays
// quiet; anything else is logged - a silently failed stop leaves the
// process running with the UI claiming otherwise.
func signalTERM(p *os.Process, what string) {
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logWarnf("%s: SIGTERM failed: %v", what, err)
	}
}

// sanitizeHostapd strips characters hostapd (or its parsing) would treat
// specially; SSIDs are otherwise free-form UTF-8.
func sanitizeHostapd(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '"' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sanitizeMDNSHost coerces a device name into a valid mDNS host label: mDNS
// (RFC 6762) labels are ASCII [A-Za-z0-9-], must start/end alphanumeric, and
// are case-insensitive - so the name is lowercased and non-alphanumerics are
// replaced with '-'.
func sanitizeMDNSHost(name string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(name) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := b.String()
	s = strings.Trim(s, "-")
	if s == "" {
		return "pi9696"
	}
	return s
}

// mdnsLoop keeps an mDNS (avahi) advertisement of the current device name
// alive on the local network, advertising <device>.local so phones/PCs can
// reach the web UI without knowing the IP. It restarts the advertisement
// whenever the device name changes at runtime. No-op in sim/dev mode - there's
// no avahi-daemon to drive and a dev box shouldn't start publishing on the
// reviewer's own network.
// mdnsCmd is the live avahi-publish-service child; guarded by the app
// mutex, killed by gracefulShutdown so a dead service isn't left
// advertising <device>.local after the process exits.
var mdnsCmd *exec.Cmd

func mdnsLoop() {
	if isSimMode() {
		return
	}
	lastName := ""
	for {
		mutex.Lock()
		name := deviceName
		mutex.Unlock()

		if name != lastName {
			mutex.Lock()
			if mdnsCmd != nil {
				mdnsCmd.Process.Kill()
				mdnsCmd.Wait()
				mdnsCmd = nil
			}
			host := sanitizeMDNSHost(name)
			// avahi-publish-service <name>._workstation._tcp <port> advertises
			// a browseable workstation service; the important bit is that
			// avahi also registers the local hostname so <host>.local resolves.
			c := exec.Command("avahi-publish-service", "-s", host, "_workstation._tcp", "9")
			if err := c.Start(); err != nil {
				logErrorf("mdns: avahi publish failed: %v", err)
			} else {
				mdnsCmd = c
				logInfof("mdns: advertising %s.local", host)
			}
			lastName = name
			mutex.Unlock()
		}
		time.Sleep(5 * time.Second)
	}
}

type AppState int

const (
	StateIdle AppState = iota
	StateRecording
	StatePlaying
	StatePaused
	StateSettings
	StateRemoteInfo
	StateCopyFiles
	StateCopying
	StateSystemOptions
	StateNetworkInfo
	StateConfirm
	StateIdleBrowse // encoder-driven idle browsing: paged VU meters, then a network/token page - see onEncoderRotate's StateIdle case
	StateWifi       // WiFi access-point submenu (enable/disable, show QR)
	StateWifiQR     // full-screen QR code for joining the WiFi AP
	StateAudio      // Audio submenu: Sample Rate, Channel Count, Tag, Prefix
	StateMetering   // Metering submenu: Meter Range, Peak Hold
	StateDisplay    // Display submenu: Brightness, Auto Dim, Menu Timeout, Back
	StateLogging    // Logging submenu: Error, Warn, Info, Debug, Back
)

// Recording output is WAV (PCM 24-bit) only - see OutputBitsPerSample and the
// ffmpeg args in startRecording. There's deliberately no FLAC/MP3 option and
// therefore no per-format ceiling to respect; WAV supports the full 1-128
// channel range.

// tagPresets are the selectable values for the recording-tag metadata field.
// Free-text annotation would need a text-entry UI this encoder-only,
// no-keyboard hardware doesn't have; a preset list is the practical
// alternative. "" (first entry) means no tag is written.
var tagPresets = []string{"", "Show", "Rehearsal", "Soundcheck", "Interview", "Backup"}

// filePrefixPresets are the OLED preset-list values for the filename prefix
// (see defaultFilePrefix below): the WebUI has a free-text Prefix field, and
// the encoder-only OLED hardware gets a preset picker instead (same "preset
// list like the Tag presets" pattern from the Round 3 design decision, which
// gave the WebUI the text field and the OLED the presets). "" (first entry)
// resets to the default prefix.
var filePrefixPresets = []string{"", "Live", "Rehearsal", "Show", "Soundcheck", "Interview"}

// defaultFilePrefix is what files are called when no custom prefix has been
// set; filePrefix (persisted) overrides it. Keeping "recording" as the
// default means an untouched unit keeps producing exactly the filenames it
// always has (and the WebUI's name parser stays correct for existing files).
const defaultFilePrefix = "recording"

type MenuMode int

const (
	SettingsMenu MenuMode = iota
	CopyFilesMenu
	SystemOptionsMenu
	NetworkInfoMenu
	DeleteConfirm
	FormatConfirm
	ShutdownConfirm
	RestartConfirm
	InfernoRestartConfirm
	ConfigImportConfirm
)

type ConfirmOption int

const (
	ConfirmNo ConfirmOption = iota
	ConfirmYes
)

// Inferno server states
type InfernoState int

const (
	InfernoStopped InfernoState = iota
	InfernoStarting
	InfernoRunning
	InfernoFailed
)

var (
	hwManager              *hardware.HardwareManager
	sampleRates            = []int{44100, 48000, 96000, 192000}
	sampleRateIdx          = 1 // Default to 48kHz
	channelCount           = 2
	tagPresetIdx           = 0
	filePrefix             = "" // "" means defaultFilePrefix; see effectiveFilePrefix
	isRecording            = false
	isCopying              = false
	recordStart            time.Time
	recordingFile          string
	playbackCmd            *exec.Cmd
	playbackFile           string
	playbackStart          time.Time
	playbackDone           chan struct{}
	recordingDone          chan struct{}
	meterPeakDB            = meterSilence
	meterRMSDB             = meterSilence
	meterChannelPeak       []float64 // raw per-channel instantaneous dBFS straight from ffmpeg, index 0 = channel 1; see meterReader
	meterChannelRMS        []float64
	meterGen               uint64 // session generation: bumped on every record/monitor start; a stale meterReader (old ffmpeg exiting late) flushes only if its generation is still current, so it can't corrupt the new session's meters
	meterChannelPeakHeld   []float64 // display-facing peak after hold/decay ballistics - see decayPeakHold; everything that shows a peak marker (OLED, WebUI) reads this, never meterChannelPeak directly
	peakHeldSetAt          []time.Time
	bootTime               time.Time // set at startup, used by uptime readout
	cpuPct                 []float64 // latest per-core usage % (cpuUsageLoop)
	displaySeq             uint64    // bumped when the panel framebuffer changes (see render); the WebUI mirror reloads on change, not on poll
	displayLastHash        uint64
	displayLastCanvasHash  uint64
	lastDisplayErrLog      time.Time // throttles display-push failure logs to 1/min
	displayPushed          bool      // first render always pushes (see render); afterwards only changed frames
	vuRangeIdx             = 3      // index into vuRangeOptions; -90dBFS default
	peakHoldIdx            = 4      // index into peakHoldOptions; 3s default (standard broadcast/DAW practice, see RESEARCH-FEATURES notes)
	transportMode          = "icon" // web dashboard transport buttons: "icon" or "text" labels - persisted, see PersistedConfig
	monitoring             bool     // input-monitor ffmpeg reading the Inferno FIFO for levels only, no recording - see startMonitor
	monitorCmd             *exec.Cmd
	monitorDone            chan struct{}
	monitoringOutput       bool          // playback's output-monitoring mode: the input monitor is stood down while a track plays (see startPlayback); UI shows "monitoring output" - no real output tap, so audio latency is untouched
	autoMonitor            bool          // true if the input monitor was started automatically at startup (see doStartInferno) rather than by the idle-browse flow; it persists across idle-browse sessions and is only stood down for recording/playback
	playbackPausedElapsed  time.Duration // frozen playback time captured the moment playback paused - see pausePlayback
	playbackDuration       time.Duration // total duration of the file currently playing, used to clamp seeks and show position as a relative offset
	idleBrowsePage         int           // current page within StateIdleBrowse - see onEncoderRotate
	idleBrowseMonitorOwned bool          // true if entering idle-browse started the monitor itself, so it knows to stop it again on exit rather than killing a monitor session started deliberately from the web UI
	editingParameter       bool          // true once a parameter row (Sample Rate/Channel/Tag in Settings) has been clicked into - only then does rotation adjust its value instead of navigating
	deviceName             = "PI9696"    // unit name shown on the login/dashboard and passed to Inferno as INFERNO_NAME; changeable only from the authenticated dashboard
	currentState           = StateIdle
	menuMode               = SettingsMenu
	selectedMenu           = 0
	menuScrollOffset       = 0
	confirmOption          = ConfirmNo
	usbMounted             = false
	usbSize                = ""
	filesToCopy            = make(map[string]bool)
	allFiles               []string
	copyProgress           = 0
	copyStarted            time.Time
	infernoCmd             *exec.Cmd
	ffmpegCmd              *exec.Cmd
	fifoPath               string
	infernoState           InfernoState
	lastSampleRate         int
	lastChannelCount       int
	// diskWarnUntil marks how long a "low disk" warning stays on the idle
	// screen after a refused record press (see onButtonPress/lowDisk).
	diskWarnUntil time.Time
	networkWasUp  bool
	mutex         sync.Mutex

	// WiFi access point settings - OFF by default. wifiSSID is the AP name
	// (device name by default), wifiPassword is user-set via the web UI.
	// wifiEnabled is the user's last on/off choice, persisted, so it's also
	// the "on or off at startup" preference (see applyWifiConfig).
	wifiEnabled  bool
	wifiSSID     string
	wifiPassword string
)

// PersistedConfig holds the non-destructive settings that survive a restart.
// WiFi access is deliberately OFF by default; everything else only ever
// changes through the normal menu/web flows, and is re-stored on change.
// See loadPersistedConfig/persistConfig. Password is stored in plaintext in
// the (root-only) config file - it's a credential for a range-limited AP on a
// local network, acceptable for this device; see the WiFi notes.
type PersistedConfig struct {
	DeviceName    string `json:"deviceName"`
	SampleRateIdx int    `json:"sampleRateIdx"`
	ChannelCount  int    `json:"channelCount"`
	TagPresetIdx  int    `json:"tagPresetIdx"`
	// FilePrefix is the recording filename prefix (see effectiveFilePrefix);
	// "" means the default. Empty, so old configs without the field are
	// already correct.
	FilePrefix    string `json:"filePrefix"`
	VURangeIdx    int    `json:"vuRangeIdx"`
	PeakHoldIdx   int    `json:"peakHoldIdx"`
	TransportMode string `json:"transportMode"`
	// Theme is the web dashboard's ftl-themes slug, or "none"/absent for the
	// built-in look. Absent in pre-theme configs, which therefore stay on the
	// built-in look after an upgrade.
	Theme string `json:"theme,omitempty"`
	// LogLevelIdx persists the current log threshold (0-3 = Error..Debug).
	LogLevelIdx int `json:"logLevelIdx"`
	// OledBrightnessPct holds the display brightness (0-100). Pointer so a
	// config without the field (pre-1.12 units) keeps the 100% default rather
	// than being indistinguishable from an explicit 0.
	OledBrightnessPct *int `json:"oledBrightnessPct,omitempty"`
	// AutoDimDisabled persists the auto-dim Off state; inverted because Go's
	// zero value (false) is the desired default of "enabled".
	AutoDimDisabled bool `json:"autoDimDisabled,omitempty"`
	// MenuTimeoutIdx persists the menu-timeout preset (0 = Off). No
	// omitempty: 0 is a real choice and must round-trip - and an old
	// config without the field decodes to 0, which preserves the
	// pre-feature behavior (menus never timed out).
	MenuTimeoutIdx int `json:"menuTimeoutIdx"`
	// DemoMode fakes the whole input chain for demonstrations (see
	// setDemoMode): a synthesized PCM source feeds the Inferno FIFO path so
	// monitoring/recording/VU pages behave exactly as with a live stream.
	// Plain bool: absent in old configs decodes to false (off), the only
	// safe default.
	DemoMode bool `json:"demoMode"`

	// HyperdeckEnabled is the Blackmagic HyperDeck control port toggle
	// (TCP 9993, unauthenticated by protocol design - hence default off).
	// Plain bool: absent in old configs decodes to false (off).
	HyperdeckEnabled bool `json:"hyperdeckEnabled,omitempty"`

	WifiEnabled  bool   `json:"wifiEnabled"`
	WifiSSID     string `json:"wifiSSID"`
	WifiPassword string `json:"wifiPassword"`
}

// wifiInited tracks whether applyWifiConfig has been run at least once so the
// startup OFF default and the "on/off at startup" toggle don't fight.
var wifiInited bool

func main() {
	var err error
	openLogFileSink()
	loadPersistedConfig()

	hwManager, err = hardware.NewHardwareManager()
	if err != nil {
		log.Fatalf("Failed to initialize hardware: %v", err)
	}

	// Apply the persisted brightness and seed the activity clock so the
	// auto-dim starts counting from boot (a zero lastInputTime must never
	// count as "idle for years").
	hwManager.SetBrightness(oledBrightnessPct)
	lastInputTime = time.Now()
	bootTime = time.Now()

	remoteToken = generateRemoteToken()
	// Sim mode has no hardware input path, so the OLED's Remote Access screen
	// (the only place the token is displayed) is unreachable - print it
	// directly to stderr instead (not through the leveled logger, which
	// defaults to Error-only). Real hardware keeps the token OLED-only.
	if isSimMode() {
		fmt.Fprintln(os.Stderr, "sim mode: remote access token", formatToken(remoteToken))
	}

	setupHardwareCallbacks()
	go infernoWorker()
	go systemOpWorker()
	go detectUSB()
	go updateLoop()
	go networkMonitorLoop()
	go peakHoldLoop()
	go cpuUsageLoop()
	go telemetryHistLoop()
	go telemetryWSLoop()
	go mdnsLoop()

	// A persisted demo mode starts its generator (and the always-on input
	// monitor over it) at boot, exactly like doStartInferno does for a real
	// server - the network loop never fires for it.
	if demoMode {
		mutex.Lock()
		syncDemoGeneratorLocked()
		maybeResumeInputMonitorLocked()
		mutex.Unlock()
	}

	// Bring the WiFi access point to the persisted startup state (OFF unless
	// explicitly enabled+startup-armed), once the network is up enough to
	// bring up wlan0.
	if wifiSSID == "" {
		wifiSSID = deviceName
	}
	applyWifiConfig(wifiSSID, wifiPassword, wifiEnabled)

	// PI9696_REMOTE_BIND is a manual test-only escape hatch: remoteControlLoop
	// binds 0.0.0.0 (every interface), which on a dev box with no real
	// network stack can still be reached, but if you need to pin the server to
	// one specific host (e.g. a loopback or a particular dev NIC) this sets it
	// explicitly and skips the loop. Never set this on a real deployment
	// unless you intend to restrict the bind address.
	if bindHost := os.Getenv("PI9696_REMOTE_BIND"); bindHost != "" {
		if _, err := startRemoteServer(bindHost); err != nil {
			log.Fatalf("PI9696_REMOTE_BIND: failed to bind %s:%s: %v", bindHost, remoteControlPort, err)
		}
		logInfof("TEST-ONLY remote control server: http://%s:%s (token on OLED: Settings -> Remote Access)", bindHost, remoteControlPort)
	} else {
		go remoteControlLoop()
	}

	// The HyperDeck control port only listens while its settings toggle is
	// on (unauthenticated by protocol design, so default off). The toggle
	// handler starts/stops it live; this covers the persisted-on-at-boot
	// case.
	if hyperdeckEnabled {
		mutex.Lock()
		setHyperdeckEnabledLocked(true)
		mutex.Unlock()
	}

	// Block until asked to stop, then clean up in order: finalize any
	// active recording (ffmpeg needs SIGTERM to write a valid WAV header -
	// verified directly against a FIFO - so dying instantly here would
	// leave a corrupt file), then stop Inferno, then release the display.
	// Without this, `systemctl restart` (or any signal, including Ctrl-C
	// when run manually outside systemd) mid-recording destroyed the take,
	// and the Inferno subprocess could be orphaned when not managed by
	// systemd's cgroup cleanup.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	// A second signal during the drain force-exits: gracefulShutdown waits
	// on subprocess reaps that a wedged ffmpeg could hold past systemd's
	// patience.
	go func() {
		<-sigCh
		log.Println("Second signal, force-exiting")
		os.Exit(1)
	}()
	gracefulShutdown()
}

// waitDone waits for a reaping goroutine with a shutdown-bounded timeout.
func waitDone(done <-chan struct{}, what string) {
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logWarnf("Shutdown: %s did not exit in time, continuing", what)
	}
}

func gracefulShutdown() {
	// Stop accepting new work first: a record/play arriving mid-drain would
	// start transport the drain below just stood down.
	closeRemoteServer()
	stopHyperdeckServer()
	// Drop the mDNS advertisement: otherwise avahi-publish-service is
	// orphaned by os.Exit and keeps answering for a dead host.
	mutex.Lock()
	if mdnsCmd != nil {
		mdnsCmd.Process.Kill()
		mdnsCmd.Wait()
		mdnsCmd = nil
	}
	mutex.Unlock()

	mutex.Lock()
	recording := isRecording
	playing := currentState == StatePlaying || currentState == StatePaused
	mon := monitoring
	var recDone, playDone, monDone chan struct{}
	if recording {
		log.Println("Stopping active recording before shutdown")
		recDone = recordingDone
		stopRecording()
	}
	if playing {
		log.Println("Stopping active playback before shutdown")
		playDone = playbackDone
		stopPlayback()
	}
	if mon {
		log.Println("Stopping input monitor before shutdown")
		monDone = monitorDone
		stopMonitor()
	}
	mutex.Unlock()

	// Wait for the owning goroutines (see startRecording/startPlayback/
	// startMonitor) to actually reap their processes before the app exits,
	// so ffmpeg isn't orphaned and the recording's WAV header gets
	// finalized. Bounded: a stuck ffmpeg used to hang shutdown forever until
	// systemd SIGKILLed mid-WAV-header; now we log and move on (systemd's
	// cgroup cleanup reaps the orphan).
	if recDone != nil {
		waitDone(recDone, "recording")
	}
	if playDone != nil {
		waitDone(playDone, "playback")
	}
	if monDone != nil {
		waitDone(monDone, "monitor")
	}

	// Stop the demo generator first so nothing is still writing the FIFO
	// while the recording/monitor ffmpeg processes below are reaped.
	mutex.Lock()
	stopDemoGeneratorLocked()
	mutex.Unlock()

	stopInfernoAndWait()
	hwManager.Close()
}

func setupHardwareCallbacks() {
	hwManager.SetEncoderCallbacks(
		onEncoderRotate,
		onEncoderClick,
		onEncoderHold,
	)

	hwManager.SetButtonCallback(hardware.RecordButton, onButtonPress)
	hwManager.SetButtonCallback(hardware.StopButton, onButtonPress)
	hwManager.SetButtonCallback(hardware.PlayButton, onButtonPress)
}

func onEncoderRotate(direction int) {
	mutex.Lock()
	defer mutex.Unlock()

	noteActivity()

	switch currentState {
	case StateIdle:
		// Rotating from the home screen opens the idle-browse flow: paged
		// per-channel VU meters, then a network/access-token page (see
		// onEncoderRotate's StateIdleBrowse case and render's
		// renderIdleBrowse). If nothing is already feeding the meters
		// (recording or an existing monitor session), start one so the
		// meters show real input levels rather than sitting silent.
		idleBrowsePage = 0
		currentState = StateIdleBrowse
		idleBrowseMonitorOwned = false
		if !monitoring {
			startMonitor()
			idleBrowseMonitorOwned = monitoring
		}

	case StateIdleBrowse:
		totalPages := idleVUPageCount() + 2 // + the waveform page, + the network/token page
		idleBrowsePage = ((idleBrowsePage+direction)%totalPages + totalPages) % totalPages

	case StateSettings:
		// The top-level Settings screen is pure navigation now - every
		// parameter lives inside a sub-menu (Audio / Metering / WiFi), so a
		// rotate just moves the cursor. Editing happens inside those
		// sub-menus (see StateAudio/StateMetering below).
		navigateMenu(direction)

	case StateAudio:
		// Param rows are press-to-edit, rotate-to-adjust, press-again-to-
		// confirm (see the comment that used to live in StateSettings).
		if !editingParameter {
			navigateMenu(direction)
			break
		}
		switch selectedMenu {
		case 0: // Sample Rate
			adjustSampleRate(direction)
		case 1: // Channel Count
			adjustChannelCount(direction)
		case 2: // Tag
			adjustRecordTag(direction)
		case 3: // Prefix
			adjustRecordPrefix(direction)
		}

	case StateMetering:
		if !editingParameter {
			navigateMenu(direction)
			break
		}
		switch selectedMenu {
		case 0: // Meter Range
			adjustVURange(direction)
		case 1: // Peak Hold
			adjustPeakHold(direction)
		}

	case StateDisplay:
		if !editingParameter {
			navigateMenu(direction)
			break
		}
		switch selectedMenu {
		case 0: // Brightness
			adjustOledBrightness(direction)
		case 1: // Auto Dim
			adjustAutoDim(direction)
		case 2: // Menu Timeout
			adjustMenuTimeout(direction)
		}

	case StateLogging:
		// The Logging submenu (Error/Warn/Info/Debug) is a direct-select
		// list, not edit-mode rows - clicking a level applies it at once.
		navigateMenu(direction)

	case StateCopyFiles:
		navigateMenu(direction)

	case StateSystemOptions:
		navigateMenu(direction)

	case StateWifi:
		navigateMenu(direction)

	case StateWifiQR:
		currentState = StateWifi
		selectedMenu = 0
		menuScrollOffset = 0

	case StatePaused:
		// Rotate while paused scrubs the playhead (see seekPlayback). While
		// actually playing, rotate is left alone so an accidental brush doesn't
		// restart the track with an audible gap.
		seekPlayback(direction)

	case StateConfirm:
		if confirmOption == ConfirmNo {
			confirmOption = ConfirmYes
		} else {
			confirmOption = ConfirmNo
		}
	}
}

func onEncoderClick() {
	mutex.Lock()
	defer mutex.Unlock()

	noteActivity()

	switch currentState {
	case StateIdle:
		if !isRecording {
			currentState = StateSettings
			selectedMenu = 0
			editingParameter = false
		}

	case StateIdleBrowse:
		exitIdleBrowse()

	case StateSettings:
		handleSettingsClick()

	case StateCopyFiles:
		handleCopyFilesClick()

	case StateSystemOptions:
		handleSystemOptionsClick()

	case StateWifi:
		handleWifiClick()

	case StateWifiQR:
		// A press anywhere on the QR view returns to the WiFi submenu.
		currentState = StateWifi
		selectedMenu = 0
		menuScrollOffset = 0

	case StateAudio:
		handleAudioClick()

	case StateMetering:
		handleMeteringClick()

	case StateDisplay:
		handleDisplayClick()

	case StateLogging:
		handleLoggingClick()

	case StatePlaying:
		pausePlayback()

	case StatePaused:
		resumePlayback()

	case StateConfirm:
		handleConfirmClick()
	}
}

func onEncoderHold() {
	mutex.Lock()
	defer mutex.Unlock()

	noteActivity()

	if currentState == StateCopying {
		isCopying = false
		currentState = StateIdle
	} else if currentState == StatePlaying || currentState == StatePaused {
		stopPlayback()
	} else if currentState == StateIdleBrowse {
		exitIdleBrowse()
	} else if currentState != StateIdle && currentState != StateRecording {
		currentState = StateIdle
		selectedMenu = 0
		menuScrollOffset = 0
		editingParameter = false
	}
}

// exitIdleBrowse returns from the idle-browse flow (see onEncoderRotate's
// StateIdle case) back to the home screen, stopping the input monitor only
// if idle-browse was the one that started it - a monitor session started
// deliberately from the web UI must keep running after leaving this view.
func exitIdleBrowse() {
	if idleBrowseMonitorOwned && monitoring {
		stopMonitor()
	}
	idleBrowseMonitorOwned = false
	currentState = StateIdle
}

func onButtonPress(buttonType hardware.ButtonType) {
	mutex.Lock()
	defer mutex.Unlock()

	noteActivity()

	switch buttonType {
	case hardware.RecordButton:
		// Also reachable from StateIdleBrowse: monitoring may already be
		// running there (see onEncoderRotate's StateIdle case), so hitting
		// Record after checking levels goes straight into recording -
		// startRecording stops the monitor itself before taking over the
		// FIFO (see its own comment).
		startRecordingGuarded()
	case hardware.StopButton:
		if isRecording {
			stopRecording()
		} else if currentState == StatePlaying || currentState == StatePaused {
			stopPlayback()
		}
	case hardware.PlayButton:
		// The PLAY key doubles as PAUSE while a track is running: it pauses
		// an active playback and resumes a paused one, so the same physical
		// key toggles the transport without needing a dedicated pause key.
		if currentState == StatePlaying {
			pausePlayback()
		} else if currentState == StatePaused {
			resumePlayback()
		} else if (currentState == StateIdle || currentState == StateIdleBrowse) && !isRecording {
			// Unlike Record, startPlayback doesn't need the FIFO and won't
			// stop a monitor itself - do it here so an idle-browse-owned
			// monitor doesn't keep running pointlessly through playback.
			if idleBrowseMonitorOwned && monitoring {
				stopMonitor()
				idleBrowseMonitorOwned = false
			}
			startPlayback()
		}
	}
}

// startRecordingGuarded is the single gate for starting a take, shared by the
// physical Record button (onButtonPress) and the WebUI start endpoint
// (handleAPIRecordStart) so both control surfaces enforce the same rules: a
// take can only begin from an idle state, never on top of another take, and
// never when low on space - less than diskWarnMinutes at the current rate
// would refuse to finish (see lowDisk). It returns true if a take actually
// started, false if it was refused.
func startRecordingGuarded() bool {
	if (currentState == StateIdle || currentState == StateIdleBrowse) && !isRecording {
		if lowDisk() {
			// Refuse to start a take there isn't room to finish: flash a
			// warning on the idle screen instead (see renderIdleScreen).
			diskWarnUntil = time.Now().Add(5 * time.Second)
			logWarnf("Refusing to record: less than 30 minutes of space remains")
			return false
		}
		startRecording()
		return true
	}
	return false
}

func adjustSampleRate(direction int) {
	sampleRateIdx += direction
	if sampleRateIdx < 0 {
		sampleRateIdx = len(sampleRates) - 1
	} else if sampleRateIdx >= len(sampleRates) {
		sampleRateIdx = 0
	}
	// Check if we need to restart Inferno server
	checkInfernoRestart()
	settingChanged()
}

func adjustChannelCount(direction int) {
	channelCount += direction
	if channelCount < 1 {
		channelCount = 1
	} else if channelCount > MaxChannelCount {
		channelCount = MaxChannelCount
	}

	// WAV supports the full 1-MaxChannelCount range - no per-format ceiling
	// to fall back over.

	// Page count depends on channel count: clamp the browse cursor so it
	// can't point past the last page after a shrink.
	if total := idleVUPageCount() + 2; idleBrowsePage >= total {
		idleBrowsePage = total - 1
	}

	// Check if we need to restart Inferno server
	checkInfernoRestart()
	settingChanged()
}

// adjustRecordTag cycles through tagPresets, wrapping in both directions.
func adjustRecordTag(direction int) {
	tagPresetIdx = ((tagPresetIdx+direction)%len(tagPresets) + len(tagPresets)) % len(tagPresets)
	settingChanged()
}

// adjustRecordPrefix cycles through filePrefixPresets, wrapping in both
// directions. The empty first preset resets filePrefix to "" which means the
// built-in default (see effectiveFilePrefix); the WebUI's free-text Prefix
// field sets arbitrary values, and when the current prefix isn't on the OLED
// list the next step lands on the first preset in the stepped direction.
func adjustRecordPrefix(direction int) {
	current := -1
	for i, p := range filePrefixPresets {
		if p == "" && filePrefix == "" || p != "" && p == filePrefix {
			current = i
			break
		}
	}
	next := (current + direction) % len(filePrefixPresets)
	if next < 0 {
		next += len(filePrefixPresets)
	}
	filePrefix = filePrefixPresets[next]
	settingChanged()
}

// adjustOledBrightness moves the 0-100% display brightness by one step and
// applies it live (under mutex from onEncoderRotate; rotations change the
// panel immediately so the operator sees the effect as they adjust).
func adjustOledBrightness(direction int) {
	oledBrightnessPct += direction
	if oledBrightnessPct < 0 {
		oledBrightnessPct = 0
	} else if oledBrightnessPct > 100 {
		oledBrightnessPct = 100
	}
	if hwManager != nil {
		hwManager.SetBrightness(oledBrightnessPct)
	}
	settingChanged()
}

// adjustAutoDim toggles the auto-dim-then-off behavior (a two-option cycle;
// direction is ignored like other 2-option wraps).
func adjustAutoDim(_ int) {
	autoDimEnabled = !autoDimEnabled
	settingChanged()
}

// menuTimeoutLabel renders the current menu-timeout preset for the Display
// submenu row - "Off" or a compact duration.
func menuTimeoutLabel() string {
	d := menuTimeoutOptions[menuTimeoutIdx]
	if d == 0 {
		return "Off"
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// adjustMenuTimeout cycles the menu-timeout preset (Off, 15s, 30s, 60s,
// 2min), persisting like every other OLED setting.
func adjustMenuTimeout(direction int) {
	menuTimeoutIdx = ((menuTimeoutIdx+direction)%len(menuTimeoutOptions) + len(menuTimeoutOptions)) % len(menuTimeoutOptions)
	settingChanged()
}

// applyMenuTimeoutLocked drops an untouched menu back to the Standby status
// screen once its idle delay has elapsed. Only menu-ish states time out:
// transport (recording/playing/paused), an in-progress copy, and the home
// screen itself are never touched. StateIdleBrowse goes through
// exitIdleBrowse so an auto-started input monitor is stood down correctly.
// Must be called under the app mutex (render does).
func applyMenuTimeoutLocked(now time.Time) {
	d := menuTimeoutOptions[menuTimeoutIdx]
	if d == 0 || lastInputTime.IsZero() {
		return
	}
	if now.Sub(lastInputTime) < d {
		return
	}
	switch currentState {
	case StateIdleBrowse:
		exitIdleBrowse()
	case StateSettings, StateAudio, StateMetering, StateDisplay, StateLogging,
		StateCopyFiles, StateSystemOptions, StateNetworkInfo, StateRemoteInfo,
		StateWifi, StateWifiQR, StateConfirm:
		currentState = StateIdle
		selectedMenu = 0
		menuScrollOffset = 0
		editingParameter = false
	}
}

func navigateMenu(direction int) {
	var maxItems int

	switch currentState {
	case StateSettings:
		maxItems = 12 // Audio, Metering, Display, Logging, Copy Files, System Options, Network Info, Remote Access, Restart Inferno, Monitoring, WiFi, Exit
	case StateAudio:
		maxItems = 5 // Sample Rate, Channel Count, Tag, Prefix, Back
	case StateMetering:
		maxItems = 3 // Meter Range, Peak Hold, Back
	case StateDisplay:
		maxItems = 4 // Brightness, Auto Dim, Menu Timeout, Back
	case StateLogging:
		maxItems = len(logLevelNames) + 1 // Error, Warn, Info, Debug, Back
	case StateCopyFiles:
		maxItems = len(allFiles) + 3 // Start Copy, [All], [NONE], files...
	case StateSystemOptions:
		maxItems = 8 // Delete All, Format USB, Export Config, Import Config, Shutdown, Restart, Demo Mode, Exit
	case StateWifi:
		maxItems = 3 // Enable AP, Show QR, Back
	}

	selectedMenu += direction
	if selectedMenu < 0 {
		selectedMenu = maxItems - 1
	} else if selectedMenu >= maxItems {
		selectedMenu = 0
	}
}

func handleSettingsClick() {
	// A click while a parameter is being edited confirms/exits back to
	// navigation, regardless of which row is selected - it does not also
	// act on that row (e.g. clicking out of editing Sample Rate must not
	// simultaneously enter Channel Count).
	if editingParameter {
		editingParameter = false
		return
	}

	switch selectedMenu {
	case 0: // Audio submenu (Sample Rate, Channel Count, Tag)
		currentState = StateAudio
		selectedMenu = 0
		menuScrollOffset = 0
	case 1: // Metering submenu (Meter Range, Peak Hold)
		currentState = StateMetering
		selectedMenu = 0
		menuScrollOffset = 0
	case 2: // Display submenu (Brightness, Auto Dim, Menu Timeout)
		currentState = StateDisplay
		selectedMenu = 0
		menuScrollOffset = 0
	case 3: // Logging submenu (Error, Warn, Info, Debug)
		currentState = StateLogging
		selectedMenu = 0
		menuScrollOffset = 0
	case 4: // Copy Files
		if usbMounted {
			loadFilesToCopy()
			currentState = StateCopyFiles
			selectedMenu = 0
			menuScrollOffset = 0
		}
	case 5: // System Options
		currentState = StateSystemOptions
		selectedMenu = 0
		menuScrollOffset = 0
	case 6: // Network Info
		currentState = StateNetworkInfo
		selectedMenu = 0
		menuScrollOffset = 0
	case 7: // Remote Access
		currentState = StateRemoteInfo
		selectedMenu = 0
		menuScrollOffset = 0
	case 8: // Restart Inferno
		menuMode = InfernoRestartConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 9: // Monitoring: immediate toggle like the WiFi AP row
		if monitoring {
			autoMonitor = false
			stopMonitor()
		} else {
			autoMonitor = true
			startMonitor()
		}
	case 10: // WiFi submenu (enable/disable + QR)
		currentState = StateWifi
		selectedMenu = 0
		menuScrollOffset = 0
	case 11: // Exit
		currentState = StateIdle
		menuScrollOffset = 0
	}
}

// handleAudioClick drives the Audio submenu (StateAudio): the four
// parameter rows behave exactly like they did when they were top-level
// settings - a click enters edit mode, rotate adjusts, click again confirms.
func handleAudioClick() {
	if editingParameter {
		editingParameter = false
		return
	}
	switch selectedMenu {
	case 0, 1, 2, 3: // Sample Rate, Channel Count, Tag, Prefix
		editingParameter = true
	case 4: // Back
		currentState = StateSettings
		selectedMenu = 0
		menuScrollOffset = 0
	}
}

// handleMeteringClick drives the Metering submenu (StateMetering): Meter
// Range and Peak Hold, both press-to-edit, plus Back.
func handleMeteringClick() {
	if editingParameter {
		editingParameter = false
		return
	}
	switch selectedMenu {
	case 0, 1: // Meter Range, Peak Hold
		editingParameter = true
	case 2: // Back
		currentState = StateSettings
		selectedMenu = 1
		menuScrollOffset = 0
	}
}

// handleLoggingClick drives the Logging submenu (StateLogging). It's a
// direct-select list - clicking a level applies it immediately (and presses
// it into the persisted config) rather than the press-to-edit dance the
// numeric Audio/Metering params need; the last row is Back.
func handleLoggingClick() {
	switch selectedMenu {
	case 0, 1, 2, 3: // Error, Warn, Info, Debug
		setLogLevel(LogLevel(selectedMenu))
	case 4: // Back
		currentState = StateSettings
		selectedMenu = 3
		menuScrollOffset = 0
	}
}

// handleDisplayClick drives the Display submenu (StateDisplay): Brightness,
// Auto Dim and Menu Timeout are press-to-edit / rotate-to-adjust rows plus
// Back, the same interaction as the Audio/Metering parameter rows.
func handleDisplayClick() {
	if editingParameter {
		editingParameter = false
		return
	}
	switch selectedMenu {
	case 0, 1, 2: // Brightness, Auto Dim, Menu Timeout
		editingParameter = true
	case 3: // Back
		currentState = StateSettings
		selectedMenu = 2
		menuScrollOffset = 0
	}
}

func handleCopyFilesClick() {
	if selectedMenu == 0 { // Start Copy
		startCopyOperation()
	} else if selectedMenu == 1 { // [All]
		for file := range filesToCopy {
			filesToCopy[file] = true
		}
	} else if selectedMenu == 2 { // [NONE]
		for file := range filesToCopy {
			filesToCopy[file] = false
		}
	} else if selectedMenu >= 3 && selectedMenu-3 < len(allFiles) {
		file := allFiles[selectedMenu-3]
		filesToCopy[file] = !filesToCopy[file]
	}
}

func handleSystemOptionsClick() {
	switch selectedMenu {
	case 0: // Delete All Recordings
		menuMode = DeleteConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 1: // Format USB Drive
		if usbMounted {
			menuMode = FormatConfirm
			currentState = StateConfirm
			confirmOption = ConfirmNo
		}
	case 2: // Export Config
		if !usbMounted {
			showSysNotice("NO USB DRIVE")
			break
		}
		if err := exportConfig(); err != nil {
			showSysNotice("EXPORT FAILED")
			logErrorf("config export: %v", err)
		} else {
			showSysNotice("CONFIG EXPORTED")
		}
	case 3: // Import Config
		if usbMounted {
			menuMode = ConfigImportConfirm
			currentState = StateConfirm
			confirmOption = ConfirmNo
		} else {
			showSysNotice("NO USB DRIVE")
		}
	case 4: // Shutdown System
		menuMode = ShutdownConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 5: // Restart System
		menuMode = RestartConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 6: // Demo Mode: immediate toggle like the WiFi AP row, no confirm
		setDemoModeLocked(!demoMode)
	case 7: // Exit
		currentState = StateSettings
		selectedMenu = 0
		menuScrollOffset = 0
	}
}

// handleWifiClick drives the WiFi access-point submenu (StateWifi). WiFi
// config is intentionally thin on the OLED - there's no keyboard - so the SSID
// and password are set from the web dashboard; this screen only toggles the AP
// on/off (persisted as the startup state) and jumps to the QR view.
func handleWifiClick() {
	switch selectedMenu {
	case 0: // Enable/disable the AP
		setWifiEnabled(!wifiEnabled)
		logInfof("wifi: AP toggled %v via OLED (SSID %q)", wifiEnabled, wifiSSID)
	case 1: // Show the join QR code
		currentState = StateWifiQR
	case 2: // Back to settings
		currentState = StateSettings
		selectedMenu = 9
		menuScrollOffset = 0
	}
}

func handleConfirmClick() {
	if confirmOption == ConfirmYes {
		switch menuMode {
		case DeleteConfirm:
			// Never delete under an active take/playback/copy: ffmpeg
			// may be writing/reading the very files being removed.
			if isRecording || playbackCmd != nil || isCopying {
				showSysNotice("BUSY - STOP FIRST")
				break
			}
			deleteAllRecordings()
		case FormatConfirm:
			// Same guard as DeleteConfirm: mkfs/umount under a live take,
			// playback or copy corrupts the media and the copy target.
			if isRecording || playbackCmd != nil || isCopying {
				showSysNotice("BUSY - STOP FIRST")
				break
			}
			enqueueSystemOp(opFormatUSB)
		case ShutdownConfirm:
			enqueueSystemOp(opShutdown)
		case RestartConfirm:
			enqueueSystemOp(opRestart)
		case InfernoRestartConfirm:
			restartInfernoServer()
		case ConfigImportConfirm:
			if err := importConfig(); err != nil {
				showSysNotice("IMPORT FAILED")
				logErrorf("config import: %v", err)
			} else {
				showSysNotice("CONFIG IMPORTED")
			}
		}
	}
	currentState = StateIdle
}

// Network monitoring loop to start/restart Inferno server when eth0 comes up
// Inferno lifecycle is fully owned by infernoWorker, the only goroutine that
// ever mutates infernoCmd/fifoPath/infernoState mid-operation. Everything
// else only ever enqueues a request and returns immediately.
//
// Actually stopping or starting the subprocess means signaling it and
// waiting for it to exit, or creating a FIFO and spawning cargo - work that
// can take an unbounded amount of time (longer if the process ignores
// SIGTERM). That used to run directly under the single app-wide mutex that
// render() and every button/encoder callback also need, which froze the
// entire UI - display stopped updating, buttons stopped responding - for as
// long as the stop/start took. Routing it through a dedicated worker keeps
// that blocking work off the UI-facing mutex, and serializes start/stop/
// restart so concurrent requests (e.g. rapid sample-rate changes) can't
// race on the shared state.
type infernoCommand int

const (
	infernoCmdStart infernoCommand = iota
	infernoCmdStop
	infernoCmdRestart
)

type infernoRequest struct {
	cmd  infernoCommand
	done chan struct{} // closed when this request finishes; nil for fire-and-forget
}

var infernoReqCh = make(chan infernoRequest, 8)

// enqueueInferno sends a fire-and-forget request. The buffer is large
// enough that a full channel only happens if the worker is stuck, in which
// case dropping is preferable to blocking the caller - which typically
// holds the app mutex.
func enqueueInferno(cmd infernoCommand) {
	select {
	case infernoReqCh <- infernoRequest{cmd: cmd}:
	default:
		logWarnf("Inferno command queue full, dropping request")
	}
}

// stopInfernoAndWait enqueues a stop and blocks until it completes. Used
// only during shutdown, where cleanup must actually finish before the
// process exits. Both the send and the wait are bounded: a worker stuck in
// TERM->KILL must not hang shutdown forever.
func stopInfernoAndWait() {
	done := make(chan struct{})
	select {
	case infernoReqCh <- infernoRequest{cmd: infernoCmdStop, done: done}:
	case <-time.After(5 * time.Second):
		logWarnf("Shutdown: inferno worker unresponsive, skipping stop")
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logWarnf("Shutdown: inferno stop timed out, continuing")
	}
}

// preemptMonitorForRestart synchronously disowns the input monitor ahead of
// an Inferno restart. doStopInferno removes the FIFO while the old monitor
// ffmpeg still holds the unlinked fd, and doStartInferno's startMonitor then
// early-returns on monitoring==true - so without this the orphan exits later,
// its reaper clears monitoring, and nobody ever starts a fresh monitor: VU
// pages stay permanently silent. Same disown pattern startRecording uses.
func preemptMonitorForRestart() {
	mutex.Lock()
	cmd := monitorCmd
	active := monitoring
	mutex.Unlock()
	if !active {
		return
	}
	if cmd != nil && cmd.Process != nil {
		signalTERM(cmd.Process, "monitor")
	}
	mutex.Lock()
	if monitorCmd == cmd {
		monitorCmd = nil
		monitoring = false
	}
	mutex.Unlock()
}

func infernoWorker() {	for req := range infernoReqCh {
		switch req.cmd {
		case infernoCmdStop:
			// Exempt from the recording guard below: this only ever comes
			// from stopInfernoAndWait during shutdown, by which point
			// gracefulShutdown has already stopped ffmpeg.
			doStopInferno()

		case infernoCmdStart, infernoCmdRestart:
			mutex.Lock()
			recording := isRecording
			mutex.Unlock()

			if recording && req.cmd == infernoCmdRestart {
				// A restart was enqueued (e.g. by checkInfernoRestart) but
				// a recording started before the worker got to it. Tearing
				// down Inferno now would kill the FIFO's writer out from
				// under ffmpeg mid-recording: ffmpeg sees EOF and quietly
				// finalizes a truncated file, but nothing clears
				// isRecording, so the UI keeps showing "● REC" with the
				// timer still running while no more audio is being
				// captured. Defer instead - stopRecording() re-enqueues
				// this restart once it's safe.
				logWarnf("Deferring Inferno restart: recording in progress")
				break
			}

			if req.cmd == infernoCmdRestart {
				preemptMonitorForRestart()
				doStopInferno()
				time.Sleep(1 * time.Second) // give the old process a moment to fully release the audio device
				if !hwManager.IsNetworkAvailable() {
					break
				}
			}
			doStartInferno()

			// Coalesce: settings may have changed again while this request
			// sat in the queue or while the (slow) stop/start was in
			// flight. Keep restarting until the running server actually
			// matches current settings, instead of leaving Inferno running
			// at a stale rate/channel count while the status bar - and any
			// new recording's filename - claim otherwise. Same recording
			// guard as above applies here.
			for {
				mutex.Lock()
				recording := isRecording
				mismatch := infernoState == InfernoRunning &&
					(sampleRates[sampleRateIdx] != lastSampleRate || channelCount != lastChannelCount)
				mutex.Unlock()
			if recording || !mismatch {
				break
			}
			preemptMonitorForRestart()
			doStopInferno()
			time.Sleep(1 * time.Second)
			doStartInferno()
			}
		}

		if req.done != nil {
			close(req.done)
		}
	}
}

func networkMonitorLoop() {
	for {
		// Probe outside the lock: network I/O must never stall
		// render()/input handling behind the app mutex.
		networkUp := hwManager.IsNetworkAvailable()
		mutex.Lock()

		if networkUp && !networkWasUp {
			// Network just came up, start Inferno if not running (never in
			// demo mode - the generator already owns the input chain)
			if infernoState != InfernoRunning && !demoMode {
				logInfof("Network available, starting Inferno server")
				enqueueInferno(infernoCmdStart)
			}
		} else if !networkUp && networkWasUp {
			// Network went down
			logWarnf("Network unavailable")
		}

		networkWasUp = networkUp
		mutex.Unlock()

		time.Sleep(5 * time.Second) // Check every 5 seconds
	}
}

// Check if Inferno server needs to be restarted due to setting changes.
// Callers (adjustSampleRate, adjustChannelCount) are always invoked from
// onEncoderRotate, which already holds mutex; enqueueInferno only sends on
// a buffered channel, so this stays fast.
func checkInfernoRestart() {
	currentSampleRate := sampleRates[sampleRateIdx]
	if demoMode {
		// The demo generator re-reads settings per chunk, so there is
		// nothing to restart - and must be no worker traffic either.
		return
	}
	if (currentSampleRate != lastSampleRate || channelCount != lastChannelCount) && infernoState == InfernoRunning {
		logInfof("Settings changed, restarting Inferno server")
		enqueueInferno(infernoCmdRestart)
	}
}

// Demo mode fakes the whole input chain for investor demonstrations: a
// synthesized PCM source feeds a FIFO on the same path the Inferno server
// would, so monitoring, recording, VU pages and the deck behave exactly as
// with a live stream - and playback runs against a timer instead of ALSA.
// It deliberately never touches infernoCmd/fifoPath/infernoState (those stay
// infernoWorker-owned); everything downstream keys off infernoUp() and
// audioFifoPath() instead, which is why enabling demo needs no Inferno
// binary, no network and no audio hardware.
var demoMode bool
var demoFifoPath string
var demoGenQuit chan struct{}
var demoGenRunning bool

// infernoUp reports whether audio is flowing, real or simulated. Every
// "can we monitor/record/show link" gate routes through here; callers hold
// the app mutex like they did for the raw infernoState comparison.
func infernoUp() bool {
	return infernoState == InfernoRunning || demoMode
}

// audioFifoPath is the FIFO ffmpeg readers (monitor, record) open: the demo
// FIFO while demo mode owns the input chain, else the Inferno one. Callers
// hold the app mutex.
func audioFifoPath() string {
	if demoMode && demoFifoPath != "" {
		return demoFifoPath
	}
	return fifoPath
}

// syncDemoGeneratorLocked makes the generator match the flag: start it when
// demo just turned on (or died unexpectedly - render() calls this every
// tick), stop it when demo turned off. Fast-gated so the steady state is
// two bool checks; callers hold the app mutex.
func syncDemoGeneratorLocked() {
	if demoMode && !demoGenRunning {
		startDemoGeneratorLocked()
	} else if !demoMode && demoGenRunning {
		stopDemoGeneratorLocked()
	}
}

// Linux fcntl pipe-size commands (no syscall package names for these).
const (
	linuxFSetPipeSz = 1031
	linuxFGetPipeSz = 1032
)

// enlargeFifo bumps a freshly created FIFO's kernel buffer past the 64KB
// default: at 128ch/48kHz s32le the stream runs ~24MB/s, so 64KB holds
// ~2.6ms of audio and any reader stall back-pressures the writer into a
// gap. 4MB holds ~160ms - enough to ride out scheduling jitter (the kernel
// clamps to pipe-max-size, 1MB here, still 16x). Best-effort: failure keeps
// the default size. O_RDWR open never blocks on a FIFO.
func enlargeFifo(path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), linuxFSetPipeSz, 4<<20); errno != 0 {
		logDebugf("fifo %s kept default pipe size: %v", path, errno)
	}
}

func demoFifoName() string {
	// Nanosecond stamp: two generations started within the same second
	// (toggle off/on, or a test right after another) must never share a
	// path, or the exiting generator's deferred remove deletes the live
	// FIFO out from under its successor.
	return filepath.Join(RawPath, fmt.Sprintf("demo_%d.raw", time.Now().UnixNano()))
}

func startDemoGeneratorLocked() {
	os.MkdirAll(RawPath, 0755)
	path := demoFifoName()
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0666); err != nil {
		logErrorf("demo: failed to create FIFO %s: %v", path, err)
		return
	}
	enlargeFifo(path)
	demoFifoPath = path
	quit := make(chan struct{})
	demoGenQuit = quit
	demoGenRunning = true
	go demoGenLoop(path, quit)
	logInfof("demo: generator started on %s", path)
}

// stopDemoGeneratorLocked signals the generator and forgets it (same
// fire-and-forget discipline as stopMonitor/stopRecording): the loop exits
// within one chunk, closes its fds and removes its own FIFO file.
func stopDemoGeneratorLocked() {
	if demoGenQuit != nil {
		close(demoGenQuit)
		demoGenQuit = nil
	}
	demoGenRunning = false
	demoFifoPath = ""
}

// setDemoModeLocked flips demo mode from the OLED/WebUI toggles: syncs the
// generator and persists. Callers hold the app mutex. Refused (false) while a
// take or playback is running: disabling mid-take pulls the demo FIFO out
// from under the recording ffmpeg, which sees EOF and silently finalizes a
// truncated take.
func setDemoModeLocked(on bool) bool {
	if on != demoMode && (isRecording || playbackCmd != nil) {
		logWarnf("demo: refusing toggle during active transport (BUSY - STOP FIRST)")
		return false
	}
	demoMode = on
	syncDemoGeneratorLocked()
	settingChanged()
	logInfof("demo: mode %v", map[bool]string{true: "ON (simulated audio)", false: "off"}[on])
	return true
}

// demoGenLoop synthesizes s32le PCM into the demo FIFO: per-channel sine
// stacks with independent slow tremolos plus a whisper of noise, so every
// meter dances on its own and astats reports genuinely varying levels (not
// canned dB numbers). Settings are re-read per chunk so rate/channel changes
// apply to new readers.
//
// I/O uses raw syscalls, not os.File: Go's runtime parks os.File writes in
// its poller instead of returning EAGAIN, which would wedge the loop with no
// reader draining and ignore quit forever. Raw O_NONBLOCK writes give true
// EAGAIN so every iteration stays responsive to quit.
func demoGenLoop(path string, quit <-chan struct{}) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		logErrorf("demo: failed to open FIFO %s: %v", path, err)
		mutex.Lock()
		demoGenRunning = false
		if demoFifoPath == path {
			demoFifoPath = ""
		}
		mutex.Unlock()
		return
	}
	defer syscall.Close(fd)
	defer os.Remove(path)

	const chunkFrames = 2048
	var t float64
	var lcg uint64 = 0x12345678
	for {
		select {
		case <-quit:
			return
		default:
		}
		mutex.Lock()
		sr := sampleRates[sampleRateIdx]
		ch := channelCount
		mutex.Unlock()
		if ch < 1 {
			ch = 1
		}
		if ch > MaxChannelCount {
			ch = MaxChannelCount
		}
		buf := make([]byte, chunkFrames*ch*4)
		for i := 0; i < chunkFrames; i++ {
			tt := t + float64(i)/float64(sr)
			for c := 0; c < ch; c++ {
				cf := float64(c + 1)
				lfo := 0.55 + 0.45*math.Sin(2*math.Pi*0.13*cf*tt+float64(c)*1.7)
				lfo2 := 0.6 + 0.4*math.Sin(2*math.Pi*0.07*(cf+1)*tt)
				lcg = lcg*6364136223846793005 + 1442695040888963407
				noise := (float64(lcg>>33)/float64(1<<31) - 1) * 0.05
				s := 0.42*math.Sin(2*math.Pi*110*cf*tt)*lfo +
					0.21*math.Sin(2*math.Pi*220*cf*1.007*tt)*lfo2 + noise
				if s > 1 {
					s = 1
				} else if s < -1 {
					s = -1
				}
				binary.LittleEndian.PutUint32(buf[(i*ch+c)*4:], uint32(int32(s*2147483647)))
			}
		}
		t += float64(chunkFrames) / float64(sr)
		for off := 0; off < len(buf); {
			select {
			case <-quit:
				return
			default:
			}
			n, err := syscall.Write(fd, buf[off:])
			if err != nil {
				if err == syscall.EAGAIN {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				// Unexpected (reader tore the FIFO down around us): mark
				// not-running so the render-tick sync restarts us.
				mutex.Lock()
				demoGenRunning = false
				mutex.Unlock()
				return
			}
			if n == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			off += n
		}
	}
}

// doStartInferno starts the Inferno Audio over IP server. Must only be
// called from infernoWorker - it manages its own locking in short critical
// sections rather than expecting the caller to hold mutex for the whole
// call, since creating the FIFO and starting the subprocess can take a
// while and must not block render() or input handling.
func doStartInferno() {
	mutex.Lock()
	if infernoState == InfernoRunning || infernoState == InfernoStarting {
		mutex.Unlock()
		return
	}
	if demoMode {
		// The demo generator already owns the input chain; never start a
		// real server underneath it.
		mutex.Unlock()
		return
	}

	infernoState = InfernoStarting
	sampleRate := sampleRates[sampleRateIdx]
	channels := channelCount
	name := deviceName

	// Create a persistent FIFO for Inferno output. Nanosecond name like the
	// demo path: a same-second restart must not make doStopInferno's
	// os.Remove(path) delete the successor's live FIFO.
	timestamp := fmt.Sprintf("%d", time.Now().UnixNano())
	baseFileName := fmt.Sprintf("inferno_%s_ch%d_%dkHz.raw", timestamp, channels, sampleRate/1000)
	path := fmt.Sprintf("%s/%s", RawPath, baseFileName)
	mutex.Unlock()

	// Ensure directories exist
	os.MkdirAll(RawPath, 0755)

	// Remove old FIFO if exists
	os.Remove(path)

	// Create new FIFO
	if err := syscall.Mkfifo(path, 0666); err != nil {
		logErrorf("Failed to create Inferno FIFO %s: %v", path, err)
		mutex.Lock()
		infernoState = InfernoFailed
		mutex.Unlock()
		return
	}
	enlargeFifo(path)

	// The Inferno server is built once during installation
	// (`cargo build --release`), so at runtime we start the prebuilt binary
	// directly instead of invoking cargo - starting cargo at runtime made
	// every restart spend the compile/link time again and, worse, blocked on
	// cargo run while the (absent during build) FIFO was unavailable, which
	// is what the recording-start guard in infernoWorker is about. See the
	// inferno template README (written at install time) for the CLI contract this
	// binary has to satisfy: -c <channels> -o <output_fifo> and
	// INFERNO_SAMPLE_RATE/INFERNO_NAME env vars.
	//
	// Running the actual binary (not a shell) means the PID is the inferno
	// process itself. Setpgid still puts it in its own process group so a
	// reaping signal reaches any grandchild it daemonizes.
	//
	// Config (sample rate, device name) is passed via cmd.Env, never the
	// argument vector - deviceName is user-settable from the web dashboard,
	// and env vars set this way are never shell-parsed, so it can't be used
	// for command injection even with shell metacharacters in the name.
	binary := InfernoBinary
	if _, err := os.Stat(binary); err != nil {
		logErrorf("Cannot start Inferno server: built binary %s not found (%v) - build the Inferno binary first", binary, err)
		mutex.Lock()
		infernoState = InfernoFailed
		mutex.Unlock()
		os.Remove(path)
		return
	}

	cmd := exec.Command(binary, "-c", fmt.Sprintf("%d", channels), "-o", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("INFERNO_SAMPLE_RATE=%d", sampleRate),
		"INFERNO_NAME="+name,
	)

	if err := cmd.Start(); err != nil {
		logErrorf("Failed to start Inferno server: %v", err)
		os.Remove(path)
		mutex.Lock()
		infernoState = InfernoFailed
		mutex.Unlock()
		return
	}

	mutex.Lock()
	infernoCmd = cmd
	fifoPath = path
	infernoState = InfernoRunning
	lastSampleRate = sampleRate
	lastChannelCount = channels
	// Input monitoring should be on from the moment the unit boots, not only
	// once the user turns the knob into the idle-browse view - so as soon as
	// the Inferno server is up, start the lightweight FIFO reader that feeds
	// the live VU meters. startMonitor is expected to run under the app
	// mutex (same as every other caller) and guards itself against
	// recording/duplicate sessions, so this never steps on a take.
	if !isRecording {
		startMonitor()
		if monitoring {
			autoMonitor = true
		}
	}
	mutex.Unlock()
	logInfof("Inferno server started with %dkHz, %d channels", sampleRate/1000, channels)
}

// doStopInferno stops the Inferno server. Must only be called from
// infernoWorker (see doStartInferno). Captures what it needs under lock,
// then releases it before signaling and waiting on the subprocess, which
// can take an unbounded amount of time if it doesn't respond to SIGTERM
// promptly.
func doStopInferno() {
	mutex.Lock()
	cmd := infernoCmd
	path := fifoPath
	infernoCmd = nil
	fifoPath = ""
	infernoState = InfernoStopped
	mutex.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Signal the whole process group (see Setpgid comment in
		// doStartInferno) so the inferno binary and any grandchild it
		// spawned actually get SIGTERM instead of being orphaned, then wait
		// with a timeout and escalate to SIGKILL if it won't die - a hung
		// server must never stall infernoWorker (and through
		// stopInfernoAndWait, gracefulShutdown) forever.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

		waitCh := make(chan struct{})
		go func() {
			cmd.Wait()
			close(waitCh)
		}()
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			logWarnf("Inferno server did not exit after SIGTERM, sending SIGKILL")
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
	}

	if path != "" {
		os.Remove(path)
	}

	logInfof("Inferno server stopped")
}

// Restart the Inferno server. Called from handleConfirmClick under mutex;
// enqueueInferno only sends on a buffered channel, so this stays fast.
func restartInfernoServer() {
	enqueueInferno(infernoCmdRestart)
}

// systemOp and systemOpCh serialize the slow, destructive system operations
// (USB format, shutdown, restart) onto a dedicated worker instead of running
// them under the UI mutex. formatUSB in particular blocks for the length of a
// mkfs plus a settle sleep - running that under the app mutex froze
// render()/input the same way pre-worker Inferno stop/start used to (that
// freeze is exactly what infernoWorker was added to eliminate). The sudo
// calls also need a worker: on a real Pi NOPASSWD sudo is configured at
// install time, but if it isn't, sudo blocks on a password prompt with no TTY -
// which must never hang the UI-facing mutex.
type systemOp int

const (
	opFormatUSB systemOp = iota
	opShutdown
	opRestart
)

var systemOpCh = make(chan systemOp, 4)

func enqueueSystemOp(op systemOp) {
	select {
	case systemOpCh <- op:
	default:
		logWarnf("system op: channel full, dropping %d", op)
	}
}

func systemOpWorker() {
	for op := range systemOpCh {
		switch op {
		case opFormatUSB:
			formatUSB()
		case opShutdown:
			log.Println("Shutting down system via menu")
			exec.Command("sudo", "shutdown", "-h", "now").Run()
		case opRestart:
			log.Println("Restarting system via menu")
			exec.Command("sudo", "reboot").Run()
		}
	}
}

func startRecording() {
	if !infernoUp() {
		logErrorf("Cannot start recording: Inferno server not running")
		return
	}

	// A named FIFO only supports one real reader at a time - concurrent
	// readers would split audio frames between them and corrupt both
	// streams. stopMonitor is fire-and-forget (SIGTERM, no wait - see its
	// own comment), so there's a brief sub-100ms window where the outgoing
	// monitor ffmpeg's read can still race the new recording ffmpeg's for
	// the FIFO's first few frames; accepted as a minor startup blip rather
	// than building a fully synchronous handoff.
	if monitoring {
		stopMonitor()
		// Claim monitor ownership synchronously: the old ffmpeg exits
		// asynchronously, and its reaping goroutine clears the meter
		// slices when monitorCmd still points at it - which would wipe
		// the fresh take's slices allocated below and leave the whole
		// take meterless. Disowning first makes that check fail.
		monitorCmd = nil
		monitoring = false
	}

	recordStart = time.Now()
	timestamp := recordStart.Format("20060102_150405")
	sampleRate := sampleRates[sampleRateIdx]
	// Filename prefix comes from the WebUI text field or the OLED preset
	// list (Round 3 design: prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav); an
	// unset prefix keeps the historical "recording_..." default.
	recordingFile = filepath.Join(recordingSubdir(recordStart),
		fmt.Sprintf("%s_%s_ch%d_%dkHz.wav", effectiveFilePrefix(), timestamp, channelCount, sampleRate/1000))
	// Second-resolution timestamps collide when takes start within the same
	// second (ffmpeg would truncate the previous take): suffix -1, -2...
	for n := 1; ; n++ {
		if _, err := os.Stat(recordingFile); os.IsNotExist(err) {
			break
		}
		recordingFile = filepath.Join(recordingSubdir(recordStart),
			fmt.Sprintf("%s_%s_ch%d_%dkHz-%d.wav", effectiveFilePrefix(), timestamp, channelCount, sampleRate/1000, n))
	}

	// Create recording directory
	os.MkdirAll(filepath.Dir(recordingFile), 0755)

	// Start FFmpeg to convert raw stream from existing Inferno FIFO to final
	// output. "-fflags nobuffer" was previously here, but on this ffmpeg
	// (7.x) it makes reading s32le from a FIFO produce a WAV with zero
	// audio frames every time (verified against a real FIFO fed by a
	// background writer: identical command differing only in that flag
	// produced a 102-byte empty file with it, a correct multi-second file
	// without it) - every recording would have been silent.
	args := []string{
		"-nostdin",
		// The FIFO writer (Inferno) never pauses; the default 8-packet
		// input queue overruns at high channel counts whenever the filter
		// chain stalls, and an overrun on a recording input is a gap in
		// the take. 512 packets of headroom costs ~2MB worst case.
		"-thread_queue_size", "512",
		"-f", "s32le", "-sample_rate", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channelCount),
		"-i", audioFifoPath(),
	}
	// Only output format is WAV (PCM 24-bit) - see OutputBitsPerSample.
	args = append(args, "-c:a", "pcm_s24le")

	// ffmpeg's -metadata maps onto the WAV container's LIST/INFO chunk. WAV's
	// INFO chunk only maps a fixed field set and silently drops arbitrary keys
	// like "software"; "date" and "comment" are verified to round-trip. "date"
	// is always written for provenance; "comment" only when a tag preset is
	// selected (see tagPresets - there's no text-entry UI on this encoder-only
	// hardware for free-form annotations).
	args = append(args, "-metadata", "date="+recordStart.Format(time.RFC3339))
	if tag := tagPresets[tagPresetIdx]; tag != "" {
		args = append(args, "-metadata", "comment="+tag)
	}

	// astats+ametadata=print is a pass-through filter chain - it reads
	// samples and prints level stats without altering them (verified
	// against a real FIFO: file duration/size identical with and without
	// it) - piped to this process's own stdout (file=-) rather than mixed
	// into ffmpeg's stderr logging, so the parsing goroutine below only
	// ever sees clean "key=value" lines.
	args = append(args, "-af", "astats=metadata=1:reset=1,ametadata=print:file=-")

	args = append(args, recordingFile)

	cmd := exec.Command("ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logErrorf("Failed to attach FFmpeg stdout: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		logErrorf("Failed to start FFmpeg: %v", err)
		return
	}

	ffmpegCmd = cmd
	isRecording = true
	currentState = StateRecording
	meterPeakDB = meterSilence
	meterRMSDB = meterSilence
	meterGen++
	meterChannelPeak = make([]float64, channelCount)
	meterChannelRMS = make([]float64, channelCount)
	for i := range meterChannelPeak {
		meterChannelPeak[i] = meterSilence
		meterChannelRMS[i] = meterSilence
	}
	done := make(chan struct{})
	recordingDone = done

	go meterReader(stdout, meterGen)

	// Deliberately NOT waiting for meterReader to see stdout EOF before
	// calling cmd.Wait() below, even though the exec docs call concurrent
	// Wait()+pipe-reads incorrect: tried exactly that gating (a meterDone
	// channel closed by meterReader, received before Wait()) and it
	// deadlocked in testing - if anything downstream of ffmpeg's own exit
	// holds the stdout fd open a moment longer (an orphaned child process
	// inheriting it, e.g.), the pipe never EOFs, meterReader never returns,
	// and stopRecording's cleanup - along with graceful shutdown - hangs
	// forever. Calling Wait() unconditionally is what actually reclaims the
	// process; losing the last frame or two of level data to the pipe
	// closing under the reader is a real but harmless cost by comparison.
	//
	// cmd.Wait must only ever be called once, and this goroutine is its
	// sole owner - whether the recording ends because ffmpeg hit EOF on its
	// own or because stopRecording sent SIGTERM, this is what reaps the
	// process and flips state back to idle. Mirrors startPlayback's
	// playbackDone pattern: stopRecording() previously called Wait()
	// directly while holding the app mutex, which was fine when the only
	// caller was the physical Stop button, but handleAPIRecordStop (the
	// remote-control HTTP handler) now calls stopRecording() too, and
	// blocking the mutex on ffmpeg's exit from an HTTP request would freeze
	// render() and every button/encoder callback for as long as that took.
	go func() {
		cmd.Wait()
		var closedFile string
		mutex.Lock()
		if ffmpegCmd == cmd {
			closedFile = recordingFile
			ffmpegCmd = nil
			isRecording = false
			meterPeakDB = meterSilence
			meterRMSDB = meterSilence
			meterChannelPeak = nil
			meterChannelRMS = nil
			if currentState == StateRecording {
				currentState = StateIdle
			}
			// A settings-triggered restart may have been deferred by
			// infernoWorker while this recording was in progress (see the
			// recording guard there). Now that it's safe, let it proceed
			// instead of leaving Inferno running at a stale rate/channel
			// count indefinitely.
			if infernoState == InfernoRunning &&
				(sampleRates[sampleRateIdx] != lastSampleRate || channelCount != lastChannelCount) {
				enqueueInferno(infernoCmdRestart)
			} else {
				// No deferred restart in flight - bring the always-on input
				// monitor back up now that the take has ended.
				maybeResumeInputMonitorLocked()
			}
		}
		mutex.Unlock()
		// The take's WAV file is now fully written and closed by ffmpeg.
		// fsync it (a foreground, deliberate write to stable storage) before
		// the take counts as done, so a power loss right after recording
		// can't leave the just-finished take as a zero-/partially-drained
		// journal cache entry. Done outside the app mutex so a slow flush
		// to a USB stick doesn't freeze the UI.
		if closedFile != "" {
			if f, err := os.OpenFile(closedFile, os.O_RDWR, 0); err == nil {
				if err := f.Sync(); err != nil {
					logErrorf("fsync of take %s failed: %v", closedFile, err)
				}
				f.Close()
			} else {
				logErrorf("fsync of take %s: open failed: %v", closedFile, err)
			}
		}
		close(done)
	}()
}

// meterChannelLineRe matches astats' per-channel ametadata lines, e.g.
// "lavfi.astats.3.Peak_level=-6.020600" for channel 3 - distinct from the
// "lavfi.astats.Overall.*" lines, which stay a plain prefix check below
// since they don't need a captured index.
var meterChannelLineRe = regexp.MustCompile(`^lavfi\.astats\.(\d+)\.(Peak|RMS)_level=(.+)$`)

// sanitizeMeterDB clamps a parsed astats level to a JSON-safe value. ffmpeg's
// astats reports -inf (Go parses "-inf" to -Inf with no error) for a silent
// block, and encoding/json refuses to marshal +/-Inf and NaN - which would
// otherwise empty out every /api/meter and /ws/meter payload the moment the
// input goes silent. Map anything non-finite back to the silence sentinel so
// the websocket meter keeps flowing.
func sanitizeMeterDB(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return meterSilence
	}
	return v
}

// meterReader parses astats/ametadata's "key=value" lines off the recording
// ffmpeg's stdout (see the -af comment in startRecording) into the
// package-level meter vars. Exits on its own once ffmpeg closes stdout
// (process exit) - no separate stop signal needed.
func meterReader(stdout io.Reader, gen uint64) {
	scanner := bufio.NewScanner(stdout)
	// astats lines for 128ch takes exceed the 64KB default: one long line
	// would silently kill meters for the whole take.
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	// astats emits thousands of lines/sec at high channel counts; taking
	// the app mutex per line serializes render and every handler behind
	// the meter parser. Parse lock-free into a small batch and flush under
	// one lock - meters refresh at 10Hz downstream, so coarser shared
	// writes are invisible. The trailing flush covers short inputs.
	const batchSize = 32
	type update struct {
		channel int // 1-based, or 0 for an Overall value
		isRMS   bool
		v       float64
	}
	var pending [batchSize]update
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		mutex.Lock()
		// Stale session check: the previous ffmpeg (e.g. a preempted
		// monitor) can still be flushing while the new session's slices
		// are live - drop its updates instead of corrupting them.
		if gen == meterGen {
			for _, u := range pending[:n] {
				if u.channel == 0 {
					if u.isRMS {
						meterRMSDB = u.v
					} else {
						meterPeakDB = u.v
					}
					continue
				}
				dest := meterChannelPeak
				if u.isRMS {
					dest = meterChannelRMS
				}
				if u.channel >= 1 && u.channel <= len(dest) {
					dest[u.channel-1] = u.v
				}
			}
		}
		mutex.Unlock()
		n = 0
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "lavfi.astats.Overall.Peak_level="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(line, "lavfi.astats.Overall.Peak_level="), 64); err == nil {
				pending[n] = update{v: sanitizeMeterDB(v)}
				n++
			}
		case strings.HasPrefix(line, "lavfi.astats.Overall.RMS_level="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(line, "lavfi.astats.Overall.RMS_level="), 64); err == nil {
				pending[n] = update{isRMS: true, v: sanitizeMeterDB(v)}
				n++
			}
		default:
			if m := meterChannelLineRe.FindStringSubmatch(line); m != nil {
				idx, _ := strconv.Atoi(m[1])
				v, err := strconv.ParseFloat(m[3], 64)
				if err != nil {
					continue
				}
				pending[n] = update{channel: idx, isRMS: m[2] == "RMS", v: sanitizeMeterDB(v)}
				n++
			}
		}
		if n == batchSize {
			flush()
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		logWarnf("meter reader ended: %v", err)
	}
}

// stopRecording signals ffmpeg to stop and returns immediately without
// waiting for it to exit - the goroutine started by startRecording owns
// cmd.Wait() and does the actual state cleanup once ffmpeg exits, so a
// second Wait() here would race it (see startRecording's comment).
func stopRecording() {
	if ffmpegCmd != nil && ffmpegCmd.Process != nil {
		signalTERM(ffmpegCmd.Process, "recording")
	}
}

// startMonitor runs a lightweight ffmpeg reader on the Inferno FIFO purely
// for level metering - no output file, no encode - so the same per-channel
// VU meters used during recording (see meterReader) can show live input
// levels beforehand. Mutually exclusive with an actual recording: see
// startRecording's comment on why a FIFO can't have two real readers.
func startMonitor() {
	if !infernoUp() || isRecording || monitoring {
		return
	}

	cmd := exec.Command("ffmpeg", "-nostdin",
		"-thread_queue_size", "512",
		"-f", "s32le", "-sample_rate", fmt.Sprintf("%d", sampleRates[sampleRateIdx]),
		"-ac", fmt.Sprintf("%d", channelCount),
		"-i", audioFifoPath(),
		"-af", "astats=metadata=1:reset=1,ametadata=print:file=-",
		"-f", "null", "-")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logErrorf("Failed to attach monitor ffmpeg stdout: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		logErrorf("Failed to start monitor ffmpeg: %v", err)
		return
	}

	monitorCmd = cmd
	monitoring = true
	meterGen++
	meterChannelPeak = make([]float64, channelCount)
	meterChannelRMS = make([]float64, channelCount)
	for i := range meterChannelPeak {
		meterChannelPeak[i] = meterSilence
		meterChannelRMS[i] = meterSilence
	}
	done := make(chan struct{})
	monitorDone = done

	go meterReader(stdout, meterGen)

	// Same fire-and-forget-Wait() pattern as startRecording, for the same
	// reason: this goroutine is the sole owner of cmd.Wait() and is what
	// actually reaps the process and clears the meter state, whether the
	// monitor was stopped explicitly or preempted by a real recording.
	go func() {
		cmd.Wait()
		mutex.Lock()
		if monitorCmd == cmd {
			monitorCmd = nil
			monitoring = false
			meterPeakDB = meterSilence
			meterRMSDB = meterSilence
			meterChannelPeak = nil
			meterChannelRMS = nil
		}
		mutex.Unlock()
		close(done)
	}()
}

// stopMonitor signals the monitor ffmpeg to stop without waiting for it to
// exit - see stopRecording's comment for why (blocking here would freeze
// the app mutex on ffmpeg's exit).
func stopMonitor() {
	if monitorCmd != nil && monitorCmd.Process != nil {
		signalTERM(monitorCmd.Process, "monitor")
	}
}

// recordingSubdir returns the per-day subfolder (under RecordPath) that a
// recording starting at t should live in, e.g. /rec/2026-08-30. Grouping by
// capture date keeps the flat /rec directory from growing without bound and
// makes the Copy Files list scannable by day instead of one huge list.
func recordingSubdir(t time.Time) string {
	return filepath.Join(RecordPath, t.Format("2006-01-02"))
}

// recordingFiles lists all finished recordings (always WAV - the only output
// format) and every location they can live. Recordings are
// written into a per-day subfolder under RecordPath (e.g. /rec/2026-08-30/) so
// the storage stays browsable at scale, but legacy flat files at the top level
// are still picked up. Copy/Delete/Play/download all route through here, so
// the layout is an implementation detail they never see - files are matched by
// their leaf name everywhere downstream.
func recordingFiles() []string {
	// Listings are rescanned constantly (browse renders, downloads,
	// clips count) while the set changes rarely (take finalize, delete).
	// Key the cache on directory mtimes: creating/deleting/renaming a
	// take touches its day-dir's mtime, so a key hit is exact - no TTL
	// staleness, no invalidation hooks. Takes are never overwritten in
	// place (unique timestamped names), which is the one change mtimes
	// can't see. Own mutex - callers vary on holding the app lock.
	// Every hit returns a fresh copy: latestRecording sorts in place.
	if key, ok := recordingFilesKey(); ok {
		// Fast path under lock; the scan itself runs unlocked so
		// concurrent misses don't serialize on disk I/O.
		recFilesMu.Lock()
		if key == recFilesKey {
			out := append([]string(nil), recFilesCached...)
			recFilesMu.Unlock()
			return out
		}
		recFilesMu.Unlock()
		files := recordingFilesScan()
		// Store only if nothing changed mid-scan: the scan isn't atomic
		// with the key, so re-verify before caching.
		if key2, ok := recordingFilesKey(); ok && key2 == key {
			recFilesMu.Lock()
			recFilesKey, recFilesCached = key, files
			recFilesMu.Unlock()
		}
		return append([]string(nil), files...)
	}
	return recordingFilesScan()
}

// recordingFilesKey fingerprints the take set from directory mtimes only:
// one Stat per day-dir, no per-file stats. False only when /rec itself is
// unreadable, in which case the caller falls back to a live scan.
func recordingFilesKey() (string, bool) {
	top, err := os.Stat(RecordPath)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d;", top.ModTime().UnixNano())
	entries, err := os.ReadDir(RecordPath)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := os.Stat(filepath.Join(RecordPath, e.Name()))
		if err != nil {
			return "", false
		}
		fmt.Fprintf(&b, "%s:%d;", e.Name(), st.ModTime().UnixNano())
	}
	return b.String(), true
}

var (
	recFilesMu     sync.Mutex
	recFilesKey    string
	recFilesCached []string
)

func recordingFilesScan() []string {
	seen := map[string]bool{}
	var files []string
	patterns := []string{
		filepath.Join(RecordPath, "*.wav"),
		filepath.Join(RecordPath, "*", "*.wav"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	return files
}

// latestRecording returns the most recently created recording, sorted by
// modification time rather than name: with a custom filename prefix the
// fixed-width timestamp no longer sits at the start of every filename, so
// lexicographic order would not be chronological across prefixes.
func latestRecording() string {
	files := recordingFiles()
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool {
		fi, errI := os.Stat(files[i])
		fj, errJ := os.Stat(files[j])
		if errI != nil || errJ != nil {
			return files[i] < files[j]
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	return files[len(files)-1]
}

// startPlayback plays the most recent recording through the default ALSA
// device via ffmpeg, which understands the WAV container directly. Refuses
// unless idle and neither recording nor already playing: exclusion previously
// rested entirely on each caller, and one direct caller would overlap record
// and ALSA playback (meter modes, monitoringOutput, and FIFO teardown all
// assume exclusivity).
func startPlayback() {
	if currentState != StateIdle && currentState != StateIdleBrowse {
		logWarnf("startPlayback refused: not idle (state %d)", currentState)
		return
	}
	if isRecording || playbackCmd != nil {
		logWarnf("startPlayback refused: transport busy")
		return
	}
	file := latestRecording()
	if file == "" {
		logWarnf("No recordings to play")
		return
	}
	playbackDuration = playbackFileDuration(file)

	// Playback switches the dashboard OLED/WebUI out of input-monitoring
	// into output-monitoring mode: stand the FIFO input monitor down (it's
	// no longer what the meters should be showing) and signal that the
	// playback output is the source. This is purely a metering-mode flip -
	// it touches only the input-monitor ffmpeg, never the ALSA playback
	// process, so it can't degrade output responsiveness.
	//
	// In demo mode there is no ALSA output to show, so the input monitor
	// stays up instead: its live demo-synth levels stand in for output
	// levels and the deck/VU pages keep dancing through the take.
	if monitoring && !demoMode {
		stopMonitor()
	}
	autoMonitor = false
	monitoringOutput = true
	playbackPausedElapsed = 0

	cmd := playbackCmdFor(file, 0)
	if err := cmd.Start(); err != nil {
		logErrorf("Failed to start playback: %v", err)
		monitoringOutput = false
		return
	}

	playbackCmd = cmd
	playbackFile = file
	playbackStart = time.Now()
	currentState = StatePlaying
	done := make(chan struct{})
	playbackDone = done
	armDemoPlaybackEnd(cmd, playbackDuration)

	// cmd.Wait must only ever be called once, and this goroutine is its sole
	// owner - whether playback finishes naturally (EOF) or is interrupted by
	// stopPlayback/gracefulShutdown sending SIGTERM, this is what reaps the
	// process and flips the state back to idle.
	go func() {
		cmd.Wait()
		mutex.Lock()
		if playbackCmd == cmd {
			playbackCmd = nil
			monitoringOutput = false
			playbackPausedElapsed = 0
			if currentState == StatePlaying || currentState == StatePaused {
				currentState = StateIdle
			}
		}
		// Back to idle and the input monitor is expected to be a persistent,
		// always-on thing (started at startup), so bring it back up.
		maybeResumeInputMonitorLocked()
		mutex.Unlock()
		close(done)
	}()
}

// playbackCmdFor builds the playback subprocess: real ffmpeg to ALSA, or in
// demo mode a sleep stand-in with the same signal semantics (SIGSTOP pauses,
// SIGCONT resumes, SIGTERM ends, Wait reaps) so pause/resume/seek/stop all
// work unmodified and only the sound itself is faked.
func playbackCmdFor(file string, pos time.Duration) *exec.Cmd {
	if demoMode {
		return exec.Command("sleep", "86400")
	}
	if pos > 0 {
		return exec.Command("ffmpeg", "-nostdin", "-ss", fmt.Sprintf("%.3f", pos.Seconds()), "-i", file, "-f", "alsa", "default")
	}
	return exec.Command("ffmpeg", "-nostdin", "-i", file, "-f", "alsa", "default")
}

// armDemoPlaybackEnd ends a simulated take when its duration elapses: a real
// ffmpeg exits at EOF on its own, but the sleep stand-in never does. No-op
// for real playback and for unknown (zero) durations. Arming stops any
// previous timer, which retires stale timers across seeks/stops. Pausing
// stops the timer and resume re-arms with the remainder, so the countdown
// tracks play position instead of wall clock (a long-paused demo track used
// to be SIGTERMed early). Caller must hold the app mutex.
var demoEndTimer *time.Timer
var demoEndCmd *exec.Cmd
var demoEndRemaining time.Duration

func stopDemoEndTimerLocked() {
	if demoEndTimer != nil {
		demoEndTimer.Stop()
		demoEndTimer = nil
	}
}

func armDemoPlaybackEnd(cmd *exec.Cmd, total time.Duration) {
	stopDemoEndTimerLocked()
	demoEndCmd = nil
	if !demoMode || total <= 0 {
		return
	}
	demoEndCmd = cmd
	demoEndTimer = time.AfterFunc(total, func() {
		mutex.Lock()
		cur, endCmd := playbackCmd, demoEndCmd
		stillDemo := demoMode
		demoEndTimer = nil
		mutex.Unlock()
		if !stillDemo || cur != cmd || cur != endCmd || cmd.Process == nil {
			return
		}
		// SIGCONT first: a paused stand-in is SIGSTOP'd and would
		// defer the TERM forever (same trap stopPlayback handles).
		if err := cmd.Process.Signal(syscall.SIGCONT); err == nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	})
}

// process stops: no output, no position advance) and flips the UI into the
// paused state. It doesn't reap the process - the goroutine started by
// startPlayback remains the sole owner of cmd.Wait().
func pausePlayback() {
	if currentState != StatePlaying || playbackCmd == nil || playbackCmd.Process == nil {
		return
	}
	playbackPausedElapsed = time.Since(playbackStart)
	if err := playbackCmd.Process.Signal(syscall.SIGSTOP); err != nil {
		logWarnf("Playback pause failed: %v", err)
		return
	}
	currentState = StatePaused
	// Freeze the demo end-of-track countdown with the playhead; resume
	// re-arms with the remainder.
	if demoMode {
		stopDemoEndTimerLocked()
		demoEndRemaining = playbackDuration - playbackPausedElapsed
		if demoEndRemaining < 0 {
			demoEndRemaining = 0
		}
	}
	logInfof("Playback paused")
}

// resumePlayback unpauses a paused ffmpeg with SIGCONT and returns the UI to
// the playing state.
func resumePlayback() {
	if currentState != StatePaused || playbackCmd == nil || playbackCmd.Process == nil {
		return
	}
	if err := playbackCmd.Process.Signal(syscall.SIGCONT); err != nil {
		logWarnf("Playback resume failed: %v", err)
		return
	}
	// The wall-clock start is now stale (it includes the paused gap), so
	// slide it forward by that gap to keep the elapsed readout accurate.
	playbackStart = playbackStart.Add(time.Since(playbackStart) - playbackPausedElapsed)
	playbackPausedElapsed = 0
	currentState = StatePlaying
	// Restart the demo end-of-track countdown from where the pause froze it.
	if demoMode {
		armDemoPlaybackEnd(playbackCmd, demoEndRemaining)
	}
	logInfof("Playback resumed")
}

// maybeResumeInputMonitorLocked must be called with mutex held. It restores
// the always-on input monitor that a recording or playback stood down, as
// long as we're back at a quiet idle with the Inferno server up - so
// metering returns automatically rather than staying dark after a take ends
// or a track plays through.
func maybeResumeInputMonitorLocked() {
	if monitoring || isRecording {
		return
	}
	if currentState != StateIdle && currentState != StateIdleBrowse {
		return
	}
	if !infernoUp() {
		return
	}
	startMonitor()
	if monitoring {
		autoMonitor = true
	}
}

// stopPlayback signals playback to stop and returns immediately without
// waiting for the process to exit - the goroutine started by startPlayback
// owns cmd.Wait() and does the actual state cleanup once ffmpeg exits, so a
// second Wait() here would race it.
func stopPlayback() {
	// monitoringOutput/playbackPausedElapsed are deliberately NOT cleared
	// here: the reaping goroutine owns the state flip and clears them, so
	// clearing synchronously would flash Paused + 00:00:00 in the
	// SIGTERM-to-Wait window.
	// Cancel a seek handoff in flight: restartPlaybackAt checks this after
	// its wait and bails instead of resurrecting playback from under Stop.
	seekingPlayback = false
	// A stopped track needs no end-of-track countdown.
	stopDemoEndTimerLocked()
	if playbackCmd != nil && playbackCmd.Process != nil {
		signalTERM(playbackCmd.Process, "playback")
		// A paused track is frozen with SIGSTOP (see pausePlayback), and a
		// stopped process defers signal delivery until it's continued: the
		// SIGTERM above would sit pending forever, ffmpeg would never exit,
		// the reaping goroutine would never run, and the UI would be stuck
		// in Paused (gracefulShutdown waits on that goroutine, so
		// shutdown-while-paused would hang the whole app). SIGCONT wakes it
		// so the TERM is delivered; for a running process SIGCONT is a
		// harmless no-op.
		playbackCmd.Process.Signal(syscall.SIGCONT)
	}
}

// playbackFileDuration returns the total duration of a WAV recording by
// parsing its channel count and sample rate from the filename and deriving
// the length from the actual file size (see recordingDuration). It returns 0
// if the name doesn't match the app's own convention or the file can't be
// stat'd, in which case seeks are clamped to the running position and the
// progress readout just shows elapsed without a total.
func playbackFileDuration(path string) time.Duration {
	name := filepath.Base(path)
	m := recFilenameRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	channels, _ := strconv.Atoi(m[4])
	sampleRate, _ := strconv.Atoi(m[5])
	return recordingDuration(path, channels, sampleRate*1000)
}

// playbackPosition returns the current playhead as a file offset. While
// playing it advances with the wall clock from playbackStart (which is slid
// forward on resume to exclude the paused gap); while paused it's the frozen
// playbackPausedElapsed.
func playbackPosition() time.Duration {
	if currentState == StatePaused {
		return playbackPausedElapsed
	}
	return time.Since(playbackStart)
}

// seekPlayback moves the playhead by seekStep per encoder detent and restarts
// ffmpeg at the new offset. Only reachable while a track is running (the
// encoder routes to it from StatePaused/StatePlaying - see onEncoderRotate);
// it restarts the process with -ss so the new position takes effect, keeping
// the paused state paused and the playing state playing.
func seekPlayback(direction int) {
	if currentState != StatePaused && currentState != StatePlaying {
		return
	}
	if playbackCmd == nil || playbackCmd.Process == nil || playbackFile == "" {
		return
	}

	const seekStep = 5 * time.Second
	pos := playbackPosition() + time.Duration(direction)*seekStep
	if pos < 0 {
		pos = 0
	}
	if playbackDuration > 0 && pos > playbackDuration {
		pos = playbackDuration
	}
	restartPlaybackAt(pos)
}

// restartPlaybackAt starts a fresh ffmpeg at the given file offset. The
// previous process (if any) is signalled to stop and - unlike the old async
// handoff - waited on (mutex released meanwhile) before the new process
// opens the output: on an exclusive (non-dmix) ALSA device the new open
// fails while the old process still holds it, and one seek detent would end
// the whole track. If the track was paused, the new process is immediately
// SIGSTOP'd so the playhead lands at the seek point still paused.
// seekingPlayback serializes overlapping detents (extras during the ~ms
// handoff are dropped; the next detent applies from the new position) and
// lets stopPlayback cancel a handoff in flight.
var seekingPlayback bool

func restartPlaybackAt(pos time.Duration) {
	if seekingPlayback {
		return
	}
	seekingPlayback = true

	old := playbackCmd
	oldDone := playbackDone
	// Capture before the handoff: the old reaper runs during the wait below
	// and flips state to Idle, so reading paused-ness afterwards always
	// says "playing".
	wasPaused := currentState == StatePaused
	if old != nil && old.Process != nil {
		signalTERM(old.Process, "playback (seek handoff)")
		// Seek only ever happens while paused, so the outgoing process is
		// usually SIGSTOP'd - and a stopped process defers SIGTERM until
		// it's continued (the same trap stopPlayback hit; see its comment).
		// Without the SIGCONT the old ffmpeg would stay frozen forever:
		// one leaked stopped process per seek detent, each still holding
		// the ALSA output open - on an exclusive (non-dmix) ALSA device
		// that would stop the new process from opening the output at all.
		// SIGCONT is a harmless no-op if it's already running.
		old.Process.Signal(syscall.SIGCONT)
	}

	if oldDone != nil {
		// Release the device before opening it again; never hold the app
		// mutex while waiting on a subprocess.
		mutex.Unlock()
		select {
		case <-oldDone:
		case <-time.After(2 * time.Second):
			logWarnf("seek: old playback did not exit in 2s, killing")
			if old != nil && old.Process != nil {
				old.Process.Signal(syscall.SIGKILL)
			}
			<-oldDone
		}
		mutex.Lock()
	}
	cancelled := !seekingPlayback
	seekingPlayback = false
	// Stop pressed mid-handoff cancels the seek: don't resurrect playback.
	// A fresh playback started meanwhile (playbackCmd replaced) and a
	// recording started meanwhile must also survive untouched. Note the
	// state check is deliberately absent: the old generation's reaper
	// always runs during the wait above and flips state to Idle - that is
	// the expected handoff, not a user stop (which clears the flag via
	// stopPlayback).
	if cancelled || isRecording {
		return
	}
	if playbackCmd != nil && playbackCmd != old {
		return
	}

	cmd := playbackCmdFor(playbackFile, pos)
	if err := cmd.Start(); err != nil {
		// The old process is confirmed dead here, so drive to idle cleanly
		// instead of leaving a stale cmd behind.
		logErrorf("Failed to seek playback: %v", err)
		playbackCmd = nil
		monitoringOutput = false
		playbackPausedElapsed = 0
		if currentState == StatePlaying || currentState == StatePaused {
			currentState = StateIdle
		}
		maybeResumeInputMonitorLocked()
		return
	}

	playbackCmd = cmd
	// The old reaper may have run during the handoff wait: it clears
	// monitoringOutput and can stand the input monitor back up (state
	// briefly reads Idle). Restore output-metering mode like startPlayback,
	// and the pre-seek play/pause state explicitly.
	if monitoring && !demoMode {
		stopMonitor()
	}
	monitoringOutput = true
	if wasPaused {
		currentState = StatePaused
		playbackPausedElapsed = pos
		cmd.Process.Signal(syscall.SIGSTOP)
	} else {
		currentState = StatePlaying
		playbackStart = time.Now().Add(-pos)
		playbackPausedElapsed = 0
	}

	done := make(chan struct{})
	playbackDone = done
	armDemoPlaybackEnd(cmd, playbackDuration-pos)
	go func() {
		cmd.Wait()
		mutex.Lock()
		if playbackCmd == cmd {
			playbackCmd = nil
			monitoringOutput = false
			playbackPausedElapsed = 0
			if currentState == StatePlaying || currentState == StatePaused {
				currentState = StateIdle
			}
			// Only the current generation resumes the monitor: a stale
			// reaper from a pre-seek process must not stand the monitor
			// back up while the new playback is running.
			maybeResumeInputMonitorLocked()
		}
		mutex.Unlock()
		close(done)
	}()
}

func loadFilesToCopy() {
	allFiles = []string{}
	filesToCopy = make(map[string]bool)

	// Store recordings as paths relative to RecordPath (e.g.
	// "2026-08-30/recording_...wav") so the Copy Files selection can show the
	// per-day folder and startCopyOperation can recreate the same subfolder
	// structure on the USB stick, keeping a busy /rec's contents organized
	// when backed up.
	for _, path := range recordingFiles() {
		rel, err := filepath.Rel(RecordPath, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		allFiles = append(allFiles, rel)
		filesToCopy[rel] = true
	}

	sort.Strings(allFiles)
}

// startCopyOperation snapshots the selection and copies it on a worker.
// Caller must hold mutex (the sole caller is handleCopyFilesClick via
// onEncoderClick, which does): taking it here would self-deadlock the
// non-reentrant app mutex and freeze render/input/HTTP on ▶ Start Copy.
func startCopyOperation() {
	if !usbMounted || isCopying {
		return
	}

	currentState = StateCopying
	isCopying = true
	copyProgress = 0
	copyStarted = time.Now()
	// Snapshot the selection while held: the goroutine below reads this
	// without the mutex, and filesToCopy is mutated under mutex elsewhere
	// (concurrent map read+write panics).
	selectedFiles := []string{}
	for file, selected := range filesToCopy {
		if selected {
			selectedFiles = append(selectedFiles, file)
		}
	}

	go func() {
		if len(selectedFiles) == 0 {
			mutex.Lock()
			isCopying = false
			currentState = StateIdle
			mutex.Unlock()
			return
		}

		for i, file := range selectedFiles {
			mutex.Lock()
			cancelled := !isCopying
			// Never copy the take currently being written: it would
			// back up a half-finalized WAV.
			active := isRecording && filepath.Join(RecordPath, file) == recordingFile
			mutex.Unlock()
			if cancelled {
				break
			}
			if active {
				logWarnf("Skipping in-progress take %s during copy", file)
				continue
			}

			src := filepath.Join(RecordPath, file)
			dst := filepath.Join(USBMountPoint, file)

			err := copyFile(src, dst, func() bool {
				mutex.Lock()
				defer mutex.Unlock()
				return !isCopying
			})
			if err == errCopyCancelled {
				break
			}
			if err != nil {
				logErrorf("Failed to copy %s: %v", file, err)
			}
			mutex.Lock()
			copyProgress = int(float64(i+1) / float64(len(selectedFiles)) * 100)
			mutex.Unlock()
		}

		mutex.Lock()
		isCopying = false
		currentState = StateIdle
		mutex.Unlock()
	}()
}

// copyFile streams src to dst rather than reading it fully into memory:
// multi-channel high-sample-rate recordings can reach many GB (e.g. 128ch at
// 192kHz/32-bit is ~98MB/s), which would exhaust RAM with os.ReadFile.
// Writes to dst.tmp + Sync + Rename so a failed/cancelled copy never leaves
// a truncated file masquerading as a good backup; partial removed on error.
// Copies in 1MB chunks so a hold-to-cancel lands mid-file instead of after
// a whole multi-GB take.
var errCopyCancelled = fmt.Errorf("copy cancelled")

func copyFile(src, dst string, cancelled func() bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Recreate the recording's subfolder (e.g. /media/usb/2026-08-30/) on
	// the USB stick so a copied library stays organized by day.
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		if cancelled() {
			out.Close()
			os.Remove(tmp)
			return errCopyCancelled
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(tmp)
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			out.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func deleteAllRecordings() {
	for _, file := range recordingFiles() {
		os.Remove(file)
	}
}

func formatUSB() {
	// Snapshot under lock: detectUSB mutates usbMounted concurrently, and
	// a stick pulled between lookup and mkfs must never format the wrong
	// device (see the re-verify before each destructive step below).
	mutex.Lock()
	mounted := usbMounted
	mutex.Unlock()
	if !mounted {
		logErrorf("Cannot format USB: not mounted")
		return
	}

	device, err := usbDevicePath()
	if err != nil {
		logErrorf("Cannot format USB: %v", err)
		return
	}

	// umount, then wipe and create a filesystem: exFAT first (no 4GB file
	// ceiling, which matters at high channel counts), FAT32 fallback when
	// exfatprogs isn't installed. If NOPASSWD sudo is configured (done at
	// install time), these run unattended; otherwise the password prompt
	// would block - hence this running on systemOpWorker, not the UI mutex.
	// Re-verify the same device is still mounted here: a pull between the
	// lookup above and now must abort, not mkfs a stale path.
	if cur, err := usbDevicePath(); err != nil || cur != device {
		logErrorf("format USB: device changed mid-format (was %s), aborting", device)
		return
	}
	if out, err := exec.Command("sudo", "umount", USBMountPoint).CombinedOutput(); err != nil {
		logErrorf("format USB: umount failed: %v: %s", err, out)
		return
	}
	// Re-verify AFTER umount: a pull/reinsert in that window can hand the
	// /dev name to a different stick, and mkfs on the stale path would wipe
	// it. Abort unless the same device is still mounted here.
	if cur, err := usbDevicePath(); err != nil || cur != device {
		logErrorf("format USB: device changed during umount (was %s), aborting", device)
		return
	}
	formatted := "exFAT"
	if out, err := exec.Command("sudo", "mkfs.exfat", device).CombinedOutput(); err != nil {
		logWarnf("format USB: mkfs.exfat failed (%v: %s) - falling back to FAT32", err, out)
		if out, err := exec.Command("sudo", "mkfs.vfat", "-F", "32", device).CombinedOutput(); err != nil {
			logErrorf("format USB: mkfs failed: %v: %s", err, out)
			return
		}
		formatted = "FAT32"
	}
	time.Sleep(2 * time.Second)

	// Remount so the stick is usable (and correctly detected as mounted)
	// again - previously this never remounted, yet detectUSB only checked
	// that the mountpoint directory existed, so the app kept believing USB
	// was mounted and the next "copy to USB" wrote plain files into the empty
	// mountpoint dir on the SD card's root filesystem.
	if err := os.MkdirAll(USBMountPoint, 0755); err != nil {
		logErrorf("format USB: mkdir mountpoint failed: %v", err)
	}
	if out, err := exec.Command("sudo", "mount", device, USBMountPoint).CombinedOutput(); err != nil {
		logErrorf("format USB: remount failed: %v: %s", err, out)
		return
	}
	logInfof("USB drive formatted (%s) and remounted", formatted)
}

// usbDevicePath looks up the block device currently mounted at USBMountPoint.
// formatUSB previously hardcoded /dev/sda1, which would format the wrong
// disk (or even a boot/root drive) on any system where the USB stick isn't
// enumerated as the first device.
func usbDevicePath() (string, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == USBMountPoint {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no device mounted at %s", USBMountPoint)
}

func detectUSB() {
	for {
		// Mount-aware: previously this only checked that the mountpoint
		// directory existed - which is always true (and stays true after a
		// format that never remounted) - so the app could believe USB was
		// mounted when nothing was actually there, then copy files onto the
		// SD card. Now USB counts as mounted only if the mountpoint is a real
		// mount (present in /proc/mounts).
		mounted := false
		if _, err := usbDevicePath(); err == nil {
			mounted = true
		}
		if mounted {
			mutex.Lock()
			usbMounted = true
			usbSize = getUSBSize()
			mutex.Unlock()
		} else {
			mutex.Lock()
			usbMounted = false
			usbSize = ""
			mutex.Unlock()
		}
		time.Sleep(1 * time.Second)
	}
}

func getUSBSize() string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(USBMountPoint, &stat); err != nil {
		return ""
	}

	// Actual capacity, not snapped to a power of two: a 500GB drive's
	// 465GiB used to display as "512GB", and 480GB as "256GB".
	totalBytes := uint64(stat.Blocks) * uint64(stat.Bsize)

	if totalBytes < 1024*1024*1024 { // Less than 1GB
		return fmt.Sprintf("%dmb", totalBytes/(1024*1024))
	} else if totalBytes < 1024*1024*1024*1024 { // Less than 1TB
		return fmt.Sprintf("%dGB", totalBytes/(1024*1024*1024))
	} else {
		return fmt.Sprintf("%dTB", totalBytes/(1024*1024*1024*1024))
	}
}

func updateLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		render()
	}
}

// updateButtonLampsLocked drives the REC/PLAY button backlights (Round 3
// hardware; STOP has no lamp): REC lit while a take is running, PLAY lit
// while playback is active and flashing at ~2Hz while paused so a paused deck
// reads as "standing by" rather than finished. Called from the 100ms render
// tick; LampManager.Set only writes a pin when its state changes, so the
// steady-state lamps cost nothing. Must hold the app mutex.
func updateButtonLampsLocked() {
	if hwManager == nil || hwManager.Lamps == nil {
		return
	}
	lm := hwManager.Lamps
	lm.Set(hardware.RecLamp, isRecording)
	switch currentState {
	case StatePlaying:
		lm.Set(hardware.PlayLamp, true)
	case StatePaused:
		lm.Set(hardware.PlayLamp, time.Now().UnixMilli()/250%2 == 0)
	default:
		lm.Set(hardware.PlayLamp, false)
	}
}

func render() {
	mutex.Lock()
	defer mutex.Unlock()

	// Auto-dim/off the panel when nobody has touched it for a while; done
	// here (100ms render tick, under the app mutex) so the dim stage follows
	// wall-clock idle with no extra timers.
	applyAutoDimLocked(time.Now())

	// Drop an untouched menu back to the Standby status screen - same tick,
	// same lock, same idle clock as auto-dim above.
	applyMenuTimeoutLocked(time.Now())

	// Keep the demo generator matching the flag (self-heals a generator
	// that died unexpectedly); steady state is two bool checks.
	syncDemoGeneratorLocked()

	pushWaveformSample()

	updateButtonLampsLocked()

	// Mid-take low-space auto-stop (Round 3 design: a take must never be
	// allowed to run into no room and have ffmpeg die mid-write, corrupting
	// the WAV header). Checks at most once per second under the render tick,
	// which is far cheaper than the Statfs syscall and keeps the behaviour on
	// the same lock everything else uses.
	if isRecording {
		checkMidTakeDiskLocked()
	}

	hwManager.ClearDisplay()

	// QR screens (WiFi join code, idle network/token page) need the full
	// 64px height for 2px QR modules, so the status bar steps aside there.
	qrScreen := currentState == StateWifiQR ||
		(currentState == StateIdleBrowse && idleBrowsePage > idleVUPageCount())
	if !qrScreen {
		renderStatusBar()
	}

	switch currentState {
	case StateIdle:
		renderIdleScreen()
	case StateIdleBrowse:
		renderIdleBrowse()
	case StateRecording:
		renderRecordingScreen()
	case StatePlaying:
		renderPlayingScreen()
	case StatePaused:
		renderPlayingScreen()
	case StateSettings:
		renderSettingsMenu()
	case StateCopyFiles:
		renderCopyFilesMenu()
	case StateCopying:
		renderCopyProgress()
	case StateSystemOptions:
		renderSystemOptionsMenu()
	case StateNetworkInfo:
		renderNetworkInfo()
	case StateRemoteInfo:
		renderRemoteInfo()
	case StateWifi:
		renderWifiMenu()
	case StateWifiQR:
		renderWifiQRScreen()
	case StateAudio:
		renderAudioMenu()
	case StateMetering:
		renderMeteringMenu()
	case StateDisplay:
		renderDisplayMenu()
	case StateLogging:
		renderLoggingMenu()
	case StateConfirm:
		renderConfirmDialog()
	}

	// Skip the SPI push when the framebuffer is unchanged: Update() always
	// writes addr commands plus the full 8KB frame, and static screens
	// (idle, paused) re-render identical pixels at 10Hz. The hash compare
	// reuses the WebUI mirror's signal (see noteDisplayFrame); the
	// first-push flag covers a theoretical first-frame hash collision so
	// boot can never leave a dark panel.
	if !displayPushed || hwManager.FrameHash() != displayLastHash {
		// A dead SPI bus used to show a frozen-but-"fine" UI with nothing
		// in the logs. Report push failures, throttled: render ticks at
		// 10Hz and a hard bus fault would otherwise flood.
		if err := hwManager.UpdateDisplay(); err != nil {
			if now := time.Now(); now.Sub(lastDisplayErrLog) >= time.Minute {
				lastDisplayErrLog = now
				logErrorf("display push failed: %v", err)
			}
		}
		displayPushed = true
	}
	noteDisplayFrame()
}

// noteDisplayFrame bumps displaySeq when the panel framebuffer differs from
// the last render, so the WebUI mirror reloads on change instead of polling.
// Tracks the packed buffer (what the panel shows, and what gates the SPI
// push above) and the canvas (what the mirror PNG is encoded from) -
// canvas-only shifts would otherwise leave the mirror stale indefinitely.
// Must be called under the app mutex (render does).
func noteDisplayFrame() {
	h, c := hwManager.FrameHash(), hwManager.CanvasHash()
	if h != displayLastHash || c != displayLastCanvasHash {
		displayLastHash = h
		displayLastCanvasHash = c
		displaySeq++
	}
}

func renderStatusBar() {
	sampleRate := sampleRates[sampleRateIdx]
	// Use FiraCode ligatures: >= <= != === !== -> <- =>
	// WAV is uncompressed PCM at the output bit depth shown below.
	formatStr := fmt.Sprintf("%s WAV %dbit %dkHz %dch", time.Now().Format("15:04"), OutputBitsPerSample, sampleRate/1000, channelCount)

	// Right side - USB status with enhanced typography
	rightSide := ""
	if usbMounted && usbSize != "" {
		// Use arrow ligature -> for better visual connection
		rightSide = fmt.Sprintf("%s [USB]", usbSize)
	} else {
		rightSide = "[---]"
	}

	// Use context-aware FiraCode rendering with Inferno status
	infernoRunning := infernoUp()
	hwManager.DrawStatusBarWithInferno(formatStr, rightSide, infernoRunning)
}

func renderIdleScreen() {
	// Use context-aware rendering for standby state
	hwManager.DrawCenteredText("Standby", "idle", 32)

	// One-shot status flash for OLED actions that have no screen of their
	// own (config export/import), transient like the low-disk warning below.
	if time.Now().Before(sysNoticeUntil) {
		if time.Now().UnixMilli()/500%2 == 0 {
			hwManager.SwitchToContext("selected")
			hwManager.DrawCenteredText(sysNotice, "selected", 48)
		}
		return
	}

	// If a record was just refused for lack of space (see onButtonPress),
	// surface an explicit flashing warning instead of the usual remaining-time
	// readout so the operator knows why the button did nothing.
	if time.Now().Before(diskWarnUntil) {
		blinkOn := time.Now().UnixMilli()/500%2 == 0
		if blinkOn {
			hwManager.SwitchToContext("selected")
			hwManager.DrawCenteredText("LOW DISK <30m", "selected", 48)
			hwManager.DrawCenteredText("cannot record", "details", 58)
		}
		return
	}

	// Someone just opened the WebUI login page (see handleLoginGet): show
	// the access token on the panel so the operator can read it off without
	// digging into Settings -> Remote Access. Must hold the app mutex.
	if loginTokenFreshLocked() {
		hwManager.DrawCenteredText("Web login token:", "details", 48)
		hwManager.DrawCenteredText("Token: "+formatToken(remoteToken), "selected", 58)
		return
	}

	// Time remaining with enhanced formatting using FiraCode features
	remaining := estimateRemainingTime()
	storage := getRemainingStorage()
	timeText := fmt.Sprintf("%s (%s) available", formatDuration(remaining), storage)
	hwManager.DrawCenteredText(timeText, "details", 48)

	hwManager.DrawCenteredText("Rotate for input levels", "details", 58)
}

// idleVUChannelsPerPage caps how many channels' worth of meters fit
// legibly across the 256px-wide panel at once (see renderIdleVUPage) -
// wider bars with room for a channel-number label read better on a small
// OLED than cramming every channel into one page.
const idleVUChannelsPerPage = 12

// idleVUPageCount is how many VU-meter pages idle-browse needs to cover
// every recording channel; onEncoderRotate's StateIdleBrowse case pages
// through these before wrapping into the network/token page.
func idleVUPageCount() int {
	pages := (channelCount + idleVUChannelsPerPage - 1) / idleVUChannelsPerPage
	if pages < 1 {
		pages = 1
	}
	return pages
}

// vuRangeOptions are the selectable VU-meter floor presets (Settings ->
// Meter Range); 0dBFS is always the top of the scale.
var vuRangeOptions = []float64{-48, -60, -72, -90, -120}

// peakHoldOptions are the selectable peak-hold durations (Settings -> Peak
// Hold) before decayPeakHold starts falling a channel's held peak back
// toward its instantaneous level; 0 means no hold at all. A ~3s default is
// the de-facto standard: ITU-R BS.1771 mandates at least 150ms, Pro Tools
// defaults to 3s, Logic offers 2/4/6s and broadcast practice is 3-5s.
var peakHoldOptions = []time.Duration{0, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second}

func adjustVURange(direction int) {
	vuRangeIdx = ((vuRangeIdx+direction)%len(vuRangeOptions) + len(vuRangeOptions)) % len(vuRangeOptions)
	settingChanged()
}

func adjustPeakHold(direction int) {
	peakHoldIdx = ((peakHoldIdx+direction)%len(peakHoldOptions) + len(peakHoldOptions)) % len(peakHoldOptions)
	settingChanged()
}

func peakHoldLabel() string {
	d := peakHoldOptions[peakHoldIdx]
	if d == 0 {
		return "Off"
	}
	return d.String()
}

// idleVUCurve is the standard audio-meter log taper shared by the OLED and
// WebUI meters (see remote.go's client-side VU_CURVE): more of the scale's
// display height given to the top of the range than the bottom, rather
// than a plain linear mapping. Expressed as {fraction of the configured
// range from floor (0.0) to 0dBFS (1.0), display %} pairs so the same
// curve shape applies whatever floor Settings -> Meter Range currently
// selects - see idleVUPct.
var idleVUCurve = [][2]float64{
	{0, 0}, {0.1667, 7}, {0.3333, 15}, {0.4444, 22}, {0.5556, 30}, {0.6667, 40},
	{0.7333, 48}, {0.8, 58}, {0.8667, 70}, {0.9, 78}, {0.9333, 85}, {0.9667, 92}, {1, 100},
}

func idleVUPct(db float64) float64 {
	floor := vuRangeOptions[vuRangeIdx]
	frac := (db - floor) / (0 - floor)
	if frac <= 0 {
		return idleVUCurve[0][1]
	}
	if frac >= 1 {
		return 100
	}
	for i := 1; i < len(idleVUCurve); i++ {
		if frac <= idleVUCurve[i][0] {
			lo, hi := idleVUCurve[i-1], idleVUCurve[i]
			t := (frac - lo[0]) / (hi[0] - lo[0])
			return lo[1] + t*(hi[1]-lo[1])
		}
	}
	return 100
}

// decayPeakHold applies peak-hold ballistics to meterChannelPeakHeld: a
// fresh instantaneous peak from meterReader always wins
// immediately, but a falling level holds at its peak for
// peakHoldOptions[peakHoldIdx] before decaying back down at a fixed
// ~20dB/s rate - the classic "peak lamp" behavior on a real meter, rather
// than the raw astats value jumping around every reset window. Ticked from
// peakHoldLoop every 100ms; callers must hold mutex.
func decayPeakHold() {
	n := len(meterChannelPeak)
	if n == 0 {
		meterChannelPeakHeld = nil
		peakHeldSetAt = nil
		return
	}
	if len(meterChannelPeakHeld) != n {
		meterChannelPeakHeld = append([]float64(nil), meterChannelPeak...)
		peakHeldSetAt = make([]time.Time, n)
		now := time.Now()
		for i := range peakHeldSetAt {
			peakHeldSetAt[i] = now
		}
		return
	}

	now := time.Now()
	hold := peakHoldOptions[peakHoldIdx]
	for i, instant := range meterChannelPeak {
		if instant >= meterChannelPeakHeld[i] {
			meterChannelPeakHeld[i] = instant
			peakHeldSetAt[i] = now
			continue
		}
		if hold == 0 || now.Sub(peakHeldSetAt[i]) > hold {
			meterChannelPeakHeld[i] -= 2.0 // 20dB/s at this loop's 100ms tick
			if meterChannelPeakHeld[i] < instant {
				meterChannelPeakHeld[i] = instant
			}
		}
	}
}

// peakHoldLoop runs decayPeakHold for the app's lifetime, independent of
// whether new astats samples are currently arriving - a real recording's
// peaks only update every astats reset window, but the display-facing
// held value needs to keep decaying smoothly in between those updates too.
func peakHoldLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		mutex.Lock()
		decayPeakHold()
		mutex.Unlock()
	}
}

// renderIdleBrowse dispatches to a VU-meter page or, once past the last
// one, the network/token page - see onEncoderRotate's StateIdleBrowse case
// for how idleBrowsePage cycles between them.
func renderIdleBrowse() {
	vuPages := idleVUPageCount()
	switch {
	case idleBrowsePage < vuPages:
		renderIdleVUPage(idleBrowsePage)
	case idleBrowsePage == vuPages:
		renderIdleWaveformPage()
	default:
		renderIdleInfoPage()
	}
}

// waveformCap bounds how many samples the scrolling waveform history keeps
// - one per render() tick (see pushWaveformSample), sized to just cover the
// panel's width so old samples fall off the left edge as new ones arrive
// on the right, like a real level-history scope trace.
const waveformCap = DisplayWidth - 4

var waveformHistory []float64

// pushWaveformSample appends the current loudest channel's instantaneous
// peak (or the overall meterPeakDB if no per-channel data exists, e.g.
// while idle-browse hasn't started a monitor) to the waveform history.
// Called from render() every tick regardless of which screen is showing,
// so the trace is already populated by the time someone pages to it rather
// than starting blank.
func pushWaveformSample() {
	level := meterPeakDB
	for _, v := range meterChannelPeak {
		if v > level {
			level = v
		}
	}
	waveformHistory = append(waveformHistory, level)
	if len(waveformHistory) > waveformCap {
		waveformHistory = waveformHistory[len(waveformHistory)-waveformCap:]
	}
}

// renderIdleWaveformPage draws the waveform history as a classic
// symmetric-around-center amplitude trace (like an audio editor's
// waveform), using the same log taper as the VU meters (idleVUPct) so a
// loud transient reads the same height here as it would as a VU deflection.
func renderIdleWaveformPage() {
	hwManager.SwitchToContext("details")
	hwManager.DrawText(2, 22, "Waveform")

	const top, bottom = 28, 54
	mid := (top + bottom) / 2
	halfH := (bottom - top) / 2

	hwManager.DrawBox(2, mid, waveformCap, 1, 4) // center baseline

	x := 2
	for _, db := range waveformHistory {
		h := int(idleVUPct(db) / 100 * float64(halfH))
		if h > 0 {
			hwManager.FillBox(x, mid-h, 1, 2*h+1, 12)
		}
		x++
	}

	hwManager.DrawText(2, 58, "Hold to return")
}

// renderIdleVUPage draws one page of up to idleVUChannelsPerPage input
// level meters, each as a vertical bar (RMS fill plus a peak-hold cap
// line) with a shared dB scale - explicitly including -12 and -40 markers
// - up the left edge. Entering idle-browse (see onEncoderRotate's StateIdle
// case) starts an input monitor if nothing else is already feeding
// meterChannelPeak/RMS, so these show live levels even before recording.
func renderIdleVUPage(page int) {
	pages := idleVUPageCount()
	// y=22 is the established safe first-content line elsewhere in this
	// file (see renderNetworkInfo etc.) - the status bar occupies the rows
	// above it, and drawing any earlier overlaps its text.
	hwManager.SwitchToContext("details")

	const top, bottom = 30, 54
	const scaleW = 18
	barAreaX := scaleW
	barAreaW := DisplayWidth - barAreaX - 2

	// Ticks scale with the configured floor (Settings -> Meter Range)
	// rather than fixed values, always at 0, floor, and two points between.
	floor := vuRangeOptions[vuRangeIdx]
	for _, db := range []float64{0, -12, floor / 2, floor} {
		y := bottom - int(idleVUPct(db)/100*float64(bottom-top))
		hwManager.DrawText(0, y+3, fmt.Sprintf("%d", int(db)))
		hwManager.DrawBox(scaleW-4, y, 4, 1, 10)
	}

	start := page * idleVUChannelsPerPage
	end := start + idleVUChannelsPerPage
	if end > channelCount {
		end = channelCount
	}
	n := end - start
	if n < 1 {
		n = 1
	}
	barW := barAreaW / n
	w := barW - 3
	if w < 6 {
		w = 6
	}

	for i := 0; i < n; i++ {
		ch := start + i
		x := barAreaX + i*barW
		rms, peak := meterSilence, meterSilence
		if ch < len(meterChannelRMS) {
			rms = meterChannelRMS[ch]
		}
		if ch < len(meterChannelPeakHeld) {
			peak = meterChannelPeakHeld[ch]
		}

		hwManager.DrawBox(x, top, w, bottom-top, 6)
		filledH := int(idleVUPct(rms) / 100 * float64(bottom-top))
		if filledH > 1 {
			hwManager.FillBox(x+1, bottom-filledH, w-2, filledH-1, 13)
		}
		peakY := bottom - int(idleVUPct(peak)/100*float64(bottom-top))
		hwManager.DrawBox(x+1, peakY, w-2, 1, 15)

		label := fmt.Sprintf("%d", ch+1)
		tw := hwManager.GetTextWidth(label)
		hwManager.DrawText(x+(w-tw)/2, 58, label)
	}
	ind := fmt.Sprintf("%d/%d", page+1, pages)
	hwManager.DrawText(DisplayWidth-hwManager.GetTextWidth(ind)-2, 58, ind)
}

// renderIdleInfoPage is the page after the last VU meter: the same network
// details Settings -> Network Info shows, plus the remote-control access
// token (see remoteAccessInfo) for anyone who wants to hop on the WebUI
// after checking input levels here.
func renderIdleInfoPage() {
	details := hwManager.GetDetailedNetworkInfo()
	hwManager.SwitchToContext("details")
	y := 22
	for i, d := range details {
		if i >= 2 {
			break
		}
		hwManager.DrawText(4, y, fitText(d, 190))
		y += 10
	}
	ip := anyInterfaceIP()
	if ip != "" {
		bmp := qrBitmap("http://" + ip + ":" + remoteControlPort + "/#t=" + remoteToken)
		drawQRBitmapFit(bmp)
	} else {
		hwManager.DrawCenteredText("Token: "+formatToken(remoteToken), "selected", y+4)
	}
	hwManager.DrawCenteredText("Click or hold to return", "details", 58)
}

func renderRecordingScreen() {
	elapsed := time.Since(recordStart)
	remaining := estimateRemainingTime()
	storage := getRemainingStorage()

	// Use FiraCode's context-aware recording display with enhanced typography
	elapsedStr := formatDuration(elapsed)
	remainingStr := fmt.Sprintf("%s (%s)", formatDuration(remaining), storage)

	// When less than diskWarnMinutes of space remains at the current rate,
	// flash the two-line display on/off every half-second so a long take can't
	// quietly run out of room. On the blink phase the recording readout is
	// cleared and replaced by a "LOW DISK" tag (see below), so operators can
	// see the warning without losing the take.
	if cachedLowDisk() && time.Now().UnixMilli()/500%2 == 1 {
		hwManager.ClearDisplay()
	}

	// The third row shows the level meter rather than the filename while
	// recording - "am I getting signal" matters more during a live take
	// than the exact filename, and there's no vertical room on a 64px
	// display for a fourth line. The filename is still discoverable via
	// Copy Files, and is deterministic from the timestamp/settings already
	// shown elsewhere.
	hwManager.DrawRecordingStatus(elapsedStr, remainingStr, formatMeter())

	if cachedLowDisk() && time.Now().UnixMilli()/500%2 == 1 {
		// Overlay a flashing "LOW DISK" tag on the blink phase so the warning
		// is legible rather than the whole screen just turning off.
		hwManager.SwitchToContext("selected")
		hwManager.DrawText(8, 22, "LOW DISK")
	}
}

// formatMeter renders meterPeakDB/meterRMSDB (updated by meterReader off the
// recording ffmpeg's astats output - see startRecording) as a compact
// dB readout. Callers must hold mutex.
func formatMeter() string {
	if meterPeakDB <= meterSilence {
		return "Peak: -- RMS: --"
	}
	return fmt.Sprintf("Peak: %.1fdB  RMS: %.1fdB", meterPeakDB, meterRMSDB)
}

func renderPlayingScreen() {
	pos := playbackPosition()
	filename := ""
	if playbackFile != "" {
		filename = filepath.Base(playbackFile)
	}
	progress := 0.0
	if playbackDuration > 0 {
		progress = float64(pos) / float64(playbackDuration)
		if progress < 0 {
			progress = 0
		} else if progress > 1 {
			progress = 1
		}
	}
	hwManager.DrawPlaybackStatus(formatDuration(pos), formatDuration(playbackDuration), progress, filename, currentState == StatePaused)
}

func renderSettingsMenu() {
	// No separate header: at 256x64 there isn't room for a title row above a
	// scrollable list without it colliding with either the status bar above
	// or the first item below, so the list starts right under the status bar.

	// Menu items using FiraCode MenuItem rendering. The top level carries
	// only sub-menu entries and actions; every parameter lives one level down
	// (Audio / Metering / WiFi) so the screen stays to a couple of focused
	// pages instead of a long scroll of ten-odd rows.
	allItems := []hardware.MenuItem{
		{Label: "Audio →", Value: fmt.Sprintf("WAV %dch", channelCount)},
		{Label: "Metering →", Value: fmt.Sprintf("%ddB", int(vuRangeOptions[vuRangeIdx]))},
		{Label: "Display →", Value: fmt.Sprintf("%d%%", oledBrightnessPct)},
		{Label: "Logging →", Value: logLevelNames[int(currentLogLevel())]},
		{Label: "Copy Files →", Value: ""},
		{Label: "System Options →", Value: ""},
		{Label: "Network Info →", Value: ""},
		{Label: "Remote Access →", Value: ""},
		{Label: "Restart Inferno", Value: getInfernoStatusText()},
		{Label: "Monitoring", Value: map[bool]string{true: "on", false: "off"}[monitoring]},
		{Label: "WiFi →", Value: map[bool]string{true: "on", false: "off"}[wifiEnabled]},
		{Label: "Exit", Value: ""},
	}

	// Calculate scrolling parameters
	totalItems := len(allItems)
	maxVisibleItems := 4 // matches what actually fits below the status bar at this font size

	// Update scroll offset based on selected item
	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}

	// Ensure scroll offset doesn't go past the end
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	// Create visible items slice
	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := allItems[menuScrollOffset:endIdx]

	// Adjust selected index for visible items
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	// Draw visible items
	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		// Switch to emphasis font for selected items
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			// "»" (editing this row's value now, rotate to adjust) vs ">"
			// (navigation cursor, rotate to move) - the two modes rotate
			// does very different things in, so the row needs to say which
			// one is active rather than leaving it to be discovered by trial.
			if editingParameter {
				prefix = "» "
			} else {
				prefix = "> "
			}
		}

		// Draw label
		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		// Draw right-aligned value if present
		if item.Value != "" {
			// Extra right margin (32, not 16) vs. the scroll-indicator
			// column: with several of these items carrying a value, any of
			// them can land as the top visible row when scrolled, so the up
			// arrow at (240,22) needs guaranteed clearance rather than
			// relying on empty-value rows happening to end up there.
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}

		y += fontHeight
	}

	// Draw scroll indicators if needed
	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		// Up arrow if we can scroll up
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		// Down arrow if we can scroll down
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

// tagStatusText is the Settings-menu row value for "Tag".
func tagStatusText() string {
	if tagPresets[tagPresetIdx] == "" {
		return "None"
	}
	return tagPresets[tagPresetIdx]
}

// effectiveFilePrefix returns the filename prefix recordings are actually
// given: "" on the global means "use the default" (so an untouched unit keeps
// producing recording_... names and old configs need no migration), while any
// real prefix - from the WebUI text field or an OLED preset - is returned
// verbatim.
func effectiveFilePrefix() string {
	if filePrefix == "" {
		return defaultFilePrefix
	}
	return filePrefix
}

// prefixStatusText is the OLED row value for "Prefix": the default prefix is
// shown as "Default" so it's clear the "" global means "not customized", and
// a set prefix is shown as-is.
func prefixStatusText() string {
	if filePrefix == "" {
		return "Default"
	}
	return filePrefix
}

// isValidFilePrefix constrains what a custom prefix may contain. It ends up
// as a literal filename segment, the first field of every recording, so it
// must be filesystem-safe and unambiguous to parse: the char set is the same
// safe set the device name uses (letters/digits/space/hyphen), but the
// underscore is excluded because "_" is the leader-suffix separator the
// recFilenameRe parser keys on (keeping the WebUI recordings list able to
// read its own files back). 1-32 chars.
func isValidFilePrefix(s string) bool {
	if s == "" || len(s) > 32 || s != strings.TrimSpace(s) {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ' || c == '-') {
			return false
		}
	}
	return true
}

// Get Inferno server status text for display
func getInfernoStatusText() string {
	switch infernoState {
	case InfernoRunning:
		return "Running"
	case InfernoStarting:
		return "Starting..."
	case InfernoFailed:
		return "Failed"
	default:
		return "Stopped"
	}
}

func renderCopyFilesMenu() {
	// No separate header - see renderSettingsMenu for why: at 256x64 there's
	// no room for a title row without it colliding with the list below it.

	// Create fixed menu items
	fixedMenuItems := []hardware.MenuItem{
		{Label: "▶ Start Copy", Value: ""},
		{Label: "☑ Select All", Value: fmt.Sprintf("(%d files)", len(allFiles))},
		{Label: "☐ Clear All", Value: ""},
	}

	// Calculate scrolling parameters for file list
	maxVisibleFiles := 1 // matches what actually fits below the 3 fixed items at this font size
	totalItems := len(fixedMenuItems) + len(allFiles)
	fixedItemsCount := len(fixedMenuItems)

	// Update scroll offset based on selected item
	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+fixedItemsCount+maxVisibleFiles {
		menuScrollOffset = selectedMenu - fixedItemsCount - maxVisibleFiles + 1
	}

	// Ensure scroll offset doesn't go past the end
	if menuScrollOffset > totalItems-fixedItemsCount-maxVisibleFiles {
		menuScrollOffset = totalItems - fixedItemsCount - maxVisibleFiles
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	// Draw fixed menu items first
	y := 22
	fontHeight := hwManager.GetFontHeight()

	for i, item := range fixedMenuItems {
		if selectedMenu == i {
			hwManager.SwitchToContext("selected")
		} else {
			hwManager.SwitchToContext("menu")
		}

		prefix := "  "
		if selectedMenu == i {
			prefix = "> "
		}

		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-16, y, item.Value)
		}

		y += fontHeight
	}

	// Draw visible file items with scrolling
	fileStartIdx := 0
	if selectedMenu >= fixedItemsCount {
		fileOffset := selectedMenu - fixedItemsCount
		if fileOffset >= maxVisibleFiles {
			fileStartIdx = fileOffset - maxVisibleFiles + 1
		}
	}

	endIdx := fileStartIdx + maxVisibleFiles
	if endIdx > len(allFiles) {
		endIdx = len(allFiles)
	}

	for i := fileStartIdx; i < endIdx; i++ {
		file := allFiles[i]
		itemIndex := fixedItemsCount + i

		if selectedMenu == itemIndex {
			hwManager.SwitchToContext("selected")
		} else {
			hwManager.SwitchToContext("menu")
		}

		prefix := "  "
		if selectedMenu == itemIndex {
			prefix = "> "
		}

		checkbox := "[ ]"
		if filesToCopy[file] {
			checkbox = "[X]"
		}

		displayName := file
		maxTextWidth := DisplayWidth - 32 // Account for margins and checkbox
		if hwManager.GetTextWidth(prefix+checkbox+" "+displayName) > maxTextWidth {
			// Truncate filename if too long
			for len(displayName) > 0 && hwManager.GetTextWidth(prefix+checkbox+" "+displayName+"...") > maxTextWidth {
				displayName = displayName[:len(displayName)-1]
			}
			if len(displayName) > 0 {
				displayName = displayName + "..."
			}
		}

		hwManager.DrawText(8, y, fmt.Sprintf("%s%s %s", prefix, checkbox, displayName))
		y += fontHeight
	}

	// Draw scroll indicators if needed
	if len(allFiles) > maxVisibleFiles {
		hwManager.SwitchToContext("details")
		// Up arrow if we can scroll up
		if fileStartIdx > 0 {
			hwManager.DrawText(240, 50, "↑")
		}
		// Down arrow if we can scroll down
		if endIdx < len(allFiles) {
			hwManager.DrawText(240, 63, "↓")
		}
	}
}

func renderCopyProgress() {
	// Use FiraCode progress bar with enhanced typography
	title := "Copying to USB..."

	// Calculate estimated remaining time from wall-clock progress.
	remainingText := "Calculating..."
	if copyProgress > 0 {
		elapsed := time.Since(copyStarted)
		remaining := elapsed * time.Duration(100-copyProgress) / time.Duration(copyProgress)
		remainingText = "~" + remaining.Round(time.Minute).String() + " remaining"
	}

	// Folded into one line - a 64px display has no room for the bar,
	// percentage, remaining time, and a cancel hint as four separate rows.
	details := remainingText + " - hold 3s to cancel"

	hwManager.DrawProgressBar(title, float64(copyProgress), details)
}

// renderWifiMenu draws the WiFi access-point submenu (StateWifi): a tiny
// three-row list - Enable AP (on/off toggle), Show QR, Back. Reuses the
// same scrolling/menu logic pattern as renderSettingsMenu.
func renderWifiMenu() {
	items := []hardware.MenuItem{
		{Label: "WiFi AP →", Value: map[bool]string{true: "on", false: "off"}[wifiEnabled]},
		{Label: "WiFi QR →", Value: ""},
		{Label: "← Back", Value: ""},
	}
	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			prefix = "> "
		}

		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

// renderAudioMenu draws the Audio submenu (StateAudio): Sample Rate, Channel
// Count, Tag, Back. Reuses the same scrolling/menu logic pattern as
// renderWifiMenu, with the editing-cursor prefix from renderSettingsMenu.
func renderAudioMenu() {
	items := []hardware.MenuItem{
		{Label: "Sample Rate →", Value: fmt.Sprintf("%dkHz", sampleRates[sampleRateIdx]/1000)},
		{Label: "Channel Count →", Value: fmt.Sprintf("%d", channelCount)},
		{Label: "Tag →", Value: tagStatusText()},
		{Label: "Prefix →", Value: prefixStatusText()},
		{Label: "← Back", Value: ""},
	}
	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			// "»" (editing this row's value now, rotate to adjust) vs ">"
			// (navigation cursor, rotate to move) - the two modes rotate
			// does very different things in, so the row needs to say which
			// one is active rather than leaving it to be discovered by trial.
			if editingParameter {
				prefix = "» "
			} else {
				prefix = "> "
			}
		}

		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

// renderMeteringMenu draws the Metering submenu (StateMetering): Meter Range,
// Peak Hold, Back. Reuses the same scrolling/menu logic pattern as
// renderWifiMenu, with the editing-cursor prefix from renderSettingsMenu.
func renderMeteringMenu() {
	items := []hardware.MenuItem{
		{Label: "Meter Range →", Value: fmt.Sprintf("%ddB", int(vuRangeOptions[vuRangeIdx]))},
		{Label: "Peak Hold →", Value: peakHoldLabel()},
		{Label: "← Back", Value: ""},
	}
	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			// "»" (editing this row's value now, rotate to adjust) vs ">"
			// (navigation cursor, rotate to move) - the two modes rotate
			// does very different things in, so the row needs to say which
			// one is active rather than leaving it to be discovered by trial.
			if editingParameter {
				prefix = "» "
			} else {
				prefix = "> "
			}
		}

		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

// renderLoggingMenu draws the Logging submenu (StateLogging): a direct-select
// list of the four levels plus Back. The active level is marked so it reads as
// a picker (the value is what a selection means, not an editable parameter).
func renderLoggingMenu() {
	items := make([]hardware.MenuItem, 0, len(logLevelNames)+1)
	for i, name := range logLevelNames {
		mark := " "
		if LogLevel(i) == currentLogLevel() {
			mark = "●"
		}
		items = append(items, hardware.MenuItem{Label: name, Value: mark})
	}
	items = append(items, hardware.MenuItem{Label: "← Back", Value: ""})
	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			prefix = "> "
		}
		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

// renderDisplayMenu draws the Display submenu (StateDisplay): Brightness
// (0-100%), Auto Dim (On/Off) and Menu Timeout (Off/15s/30s/60s/2min) as
// press-to-edit rows plus Back - same interaction as the Audio/Metering
// parameter rows.
func renderDisplayMenu() {
	items := []hardware.MenuItem{
		{Label: "Brightness →", Value: fmt.Sprintf("%d%%", oledBrightnessPct)},
		{Label: "Auto Dim →", Value: map[bool]string{true: "On", false: "Off"}[autoDimEnabled]},
		{Label: "Menu Timeout →", Value: menuTimeoutLabel()},
		{Label: "← Back", Value: ""},
	}
	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13

	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			if editingParameter {
				prefix = "» "
			} else {
				prefix = "> "
			}
		}
		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-32, y, item.Value)
		}
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

func renderSystemOptionsMenu() { // No separate header - see renderSettingsMenu for why. 5 items don't all
	// fit at once either, so this scrolls the same way Settings does.
	items := []hardware.MenuItem{
		{Label: "Delete All Recordings", Value: ""},
		{Label: "Format USB Drive", Value: ""},
		{Label: "Export Config", Value: ""},
		{Label: "Import Config", Value: ""},
		{Label: "Shutdown System", Value: ""},
		{Label: "Restart System", Value: ""},
		{Label: "Demo Mode: " + map[bool]string{true: "On", false: "Off"}[demoMode], Value: ""},
		{Label: "← Exit", Value: ""},
	}

	totalItems := len(items)
	maxVisibleItems := 4

	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset+maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}
	if menuScrollOffset > totalItems-maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := items[menuScrollOffset:endIdx]
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	y := 22
	fontHeight := 13
	for i, item := range visibleItems {
		if i == visibleSelectedIndex {
			hwManager.SwitchToContext("selected")
		} else {
			hwManager.SwitchToContext("menu")
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			prefix = "> "
		}
		hwManager.DrawText(8, y, prefix+item.Label)
		y += fontHeight
	}

	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 22, "↑")
		}
		if menuScrollOffset+maxVisibleItems < totalItems {
			hwManager.DrawText(240, 61, "↓")
		}
	}
}

func renderConfirmDialog() {
	var title, message1, message2 string

	switch menuMode {
	case DeleteConfirm:
		title = "CONFIRM DELETE"
		message1 = "Delete ALL recordings?"
		message2 = "This action cannot be undone!"
	case FormatConfirm:
		title = "CONFIRM FORMAT"
		message1 = "Format USB drive?"
		message2 = "All data will be lost!"
	case ShutdownConfirm:
		title = "SHUTDOWN"
		message1 = "Power off the system?"
		message2 = ""
	case RestartConfirm:
		title = "RESTART"
		message1 = "Restart the system?"
		message2 = ""
	case InfernoRestartConfirm:
		title = "RESTART INFERNO"
		message1 = "Restart Inferno server?"
		message2 = "Will reconnect audio stream"
	case ConfigImportConfirm:
		title = "IMPORT CONFIG"
		message1 = "Import settings from USB?"
		message2 = "Overwrites current settings"
	}

	// Use FiraCode context-aware confirmation dialog
	selectedOption := 0 // NO is default (safer)
	if confirmOption == ConfirmYes {
		selectedOption = 1
	}

	hwManager.DrawConfirmationDialog(title, message1, message2, selectedOption)
}

func renderNetworkInfo() {
	// No separate header - see renderSettingsMenu for why.
	networkDetails := hwManager.GetDetailedNetworkInfo()

	y := 22
	maxLines := 3 // Limit to fit above the footer line
	for i, detail := range networkDetails {
		if i >= maxLines {
			break
		}

		// Use different contexts for different types of info. "selected"
		// (bold but same point size as neighbors) highlights the connected
		// status without the layout jump "emphasis" causes at its 16pt size.
		context := "details"
		if i == 0 { // Interface name
			context = "menu"
		} else if strings.Contains(detail, "Status:") {
			if strings.Contains(detail, "Connected") {
				context = "selected"
			} else {
				context = "details"
			}
		}

		hwManager.DrawCenteredText(detail, context, y)
		y += 11
	}

	// Add back instruction
	hwManager.DrawCenteredText("Hold encoder to return", "details", 58)
}

func renderRemoteInfo() {
	// No separate header - see renderSettingsMenu for why.
	lines := remoteAccessInfo()

	y := 22
	for i, line := range lines {
		context := "details"
		if i == 0 {
			context = "menu"
		}
		hwManager.DrawCenteredText(line, context, y)
		y += 11
	}

	hwManager.DrawCenteredText("Hold encoder to return", "details", 58)
}

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
}

func estimateRemainingTime() time.Duration {
	free := getFreeSpace()
	bytesPerSec := recordingBytesPerSecond()
	if bytesPerSec <= 0 {
		return 0
	}
	return time.Duration(float64(free)/bytesPerSec) * time.Second
}

// lowDisk reports whether the current storage can't sustain more than
// diskWarnMinutes of recording at the currently configured rate. An
// unstatable/unwritable path counts as low: refusing the take up front beats
// letting ffmpeg die mid-write with a corrupt header.
const diskWarnMinutes = 30

func lowDisk() bool {
	if storageUnwritable() {
		return true
	}
	// Exactly-full is stattable, so storageUnwritable misses it - and the
	// estimate below is 0, which means "unknown". Check full explicitly.
	if diskFull() {
		return true
	}
	r := estimateRemainingTime()
	return r > 0 && r < diskWarnMinutes*time.Minute
}

// diskFull reports a stattable-but-completely-full recording volume.
// estimateRemainingTime returns 0 both for this and for an unknowable rate,
// so the guards need the explicit distinction: full must refuse/stop takes,
// unknown must not.
func diskFull() bool {
	if storageUnwritable() {
		return false
	}
	return getFreeSpace() == 0
}

// cachedLowDisk memoizes lowDisk at 1Hz for the 100ms render tick: two
// Statfs per frame at 20 frames/s stalled the whole UI on slow media.
// Event-driven callers (record-start guard) keep calling lowDisk directly.
var cachedLowDiskAt time.Time
var cachedLowDiskVal bool

func cachedLowDisk() bool {
	if now := time.Now(); now.Sub(cachedLowDiskAt) >= time.Second {
		cachedLowDiskAt = now
		cachedLowDiskVal = lowDisk()
	}
	return cachedLowDiskVal
}

// midTakeDiskStopThreshold is the remaining-time below which an in-progress
// take is auto-stopped so ffmpeg finalizes the WAV (headers, length) while
// there is still room, instead of running out of space mid-write and leaving
// a corrupt file with no usable take.
const midTakeDiskStopThreshold = time.Minute

// lastMidTakeDiskCheck gates the mid-take Statfs check to at most once per
// second (it runs under the render tick).
var lastMidTakeDiskCheck time.Time

// checkMidTakeDiskLocked auto-stops an in-progress take when the estimated
// remaining disk time drops below midTakeDiskStopThreshold. Callers must hold
// mutex. Unwritable storage also triggers (see shouldAutoStopTake).
func checkMidTakeDiskLocked() {
	if now := time.Now(); now.Sub(lastMidTakeDiskCheck) < time.Second {
		return
	} else {
		lastMidTakeDiskCheck = now
	}
	if shouldAutoStopTake(estimateRemainingTime()) || diskFull() {
		diskWarnUntil = time.Now().Add(5 * time.Second)
		logWarnf("Auto-stopping take: under a minute of space remains")
		stopRecording()
	}
}

// shouldAutoStopTake reports whether an in-progress take should be stopped
// because the remaining disk time has dropped below the auto-stop threshold.
// Unstatable storage also triggers: same reasoning as lowDisk - stop while
// ffmpeg can still finalize the WAV instead of dying mid-write.
func shouldAutoStopTake(remaining time.Duration) bool {
	if storageUnwritable() {
		return true
	}
	return remaining > 0 && remaining < midTakeDiskStopThreshold
}

// recordingBytesPerSecond estimates on-disk output rate. WAV is uncompressed
// PCM, so it's derived exactly from the raw PCM rate.
func recordingBytesPerSecond() float64 {
	sampleRate := sampleRates[sampleRateIdx]
	return float64(sampleRate * channelCount * OutputBitsPerSample / 8)
}

func getRemainingStorage() string {
	free := getFreeSpace()
	if free < 1024*1024 {
		return fmt.Sprintf("%dKB", free/1024)
	} else if free < 1024*1024*1024 {
		return fmt.Sprintf("%dMB", free/(1024*1024))
	} else {
		return fmt.Sprintf("%dGB", free/(1024*1024*1024))
	}
}

// storageUnwritable reports whether the recording path can't be stat'd
// (unmounted/unwritable). One Statfs per call; callers already gate to
// ~1Hz (checkMidTakeDiskLocked) or event-driven (lowDisk on record start).
func storageUnwritable() bool {
	var stat syscall.Statfs_t
	return syscall.Statfs(RecordPath, &stat) != nil
}

// getFreeSpace returns the free bytes on the recording media (RecordPath).
// It always measures RecordPath, never the USB stick: recordings are written
// to /rec (the SD card) regardless of whether a USB drive is mounted, and USB
// is only a copy/export target (startCopyOperation). lowDisk() gates whether
// recording is allowed at all, and getRemainingStorage() is what the idle
// screen / WebUI report - both must reflect the volume a new take will
// actually land on. If the recording path can't be stat'd (unmounted/unwritable)
// it returns 0; lowDisk() treats that as low via storageUnwritable, and an
// exactly-full (stattable, 0 free) volume via diskFull. Results are cached for 1s: the idle/recording screens
// call it twice per render at 10Hz, and free space never needs fresher
// than the mid-take disk check's own 1s throttle (see
// checkMidTakeDiskLocked). Own mutex - callers hold the app mutex or not
// depending on path.
var (
	freeSpaceMu    sync.Mutex
	freeSpaceAt    time.Time
	freeSpaceBytes uint64
)

func getFreeSpace() uint64 {
	freeSpaceMu.Lock()
	defer freeSpaceMu.Unlock()
	if time.Since(freeSpaceAt) < time.Second {
		return freeSpaceBytes
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(RecordPath, &stat); err != nil {
		return 0
	}
	freeSpaceBytes = stat.Bavail * uint64(stat.Bsize)
	freeSpaceAt = time.Now()
	return freeSpaceBytes
}

// ---- telemetry helpers ----

const appVersion = "1.20.0"

type cpuStat struct{ total, idle int64 }

func readCPUStat() ([]cpuStat, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, false
	}
	var stats []cpuStat
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) < 5 || line[:3] != "cpu" {
			continue
		}
		if line[3] < '0' || line[3] > '9' {
			continue // skip aggregate "cpu" line
		}
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		var total int64
		for _, s := range f[1:] {
			v, _ := strconv.ParseInt(s, 10, 64)
			total += v
		}
		idle, _ := strconv.ParseInt(f[4], 10, 64)
		iow, _ := strconv.ParseInt(f[5], 10, 64)
		stats = append(stats, cpuStat{total, idle + iow})
	}
	return stats, true
}

func cpuUsageLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	prev, _ := readCPUStat()
	<-ticker.C
	for range ticker.C {
		curr, ok := readCPUStat()
		mutex.Lock()
		if ok && len(curr) > 0 && len(prev) > 0 && len(curr) == len(prev) {
			cpuPct = make([]float64, len(curr))
			for i := range curr {
				dt := curr[i].total - prev[i].total
				di := curr[i].idle - prev[i].idle
				if dt > 0 {
					cpuPct[i] = (1 - float64(di)/float64(dt)) * 100
				}
			}
		}
		mutex.Unlock()
		prev = curr
	}
}

func readProcKV(pid int, key string) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	prefix := key + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				return v
			}
		}
	}
	return -1
}

func ramMB(pid int, key string) float64 {
	kb := readProcKV(pid, key)
	if kb < 0 {
		return -1
	}
	return float64(kb) / 1024
}

func systemRAM() (used, total float64) {
	var memTotal, memAvail int64
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					memTotal, _ = strconv.ParseInt(f[1], 10, 64)
				}
			} else if strings.HasPrefix(line, "MemAvailable:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					memAvail, _ = strconv.ParseInt(f[1], 10, 64)
				}
			}
		}
	}
	if memTotal == 0 {
		return 0, 0
	}
	return float64(memTotal-memAvail) / 1024, float64(memTotal) / 1024
}

func readCPUTemp() (float64, bool) {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return -1, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return -1, false
	}
	return float64(v) / 1000, true
}

type telemetryData struct {
	Uptime      string
	AppVersion  string
	CPUPerCore  []float64
	RAMApp      float64
	RAMInferno  float64
	RAMSysUsed  float64
	RAMSysTotal float64
	CPUTemp     float64
	DiskTotal   float64
	DiskFree    float64
	RecordTime  string
}

func snapshotTelemetry() telemetryData {
	// Copy the guarded scalars, then do every /proc|/sys read and Statfs
	// lock-free: holding the app mutex across filesystem I/O stalled
	// render()/buttons/HTTP on slow storage.
	mutex.Lock()
	cores := append([]float64(nil), cpuPct...)
	infernoPid := 0
	if infernoCmd != nil && infernoCmd.Process != nil {
		infernoPid = infernoCmd.Process.Pid
	}
	bps := float64(sampleRates[sampleRateIdx] * channelCount * OutputBitsPerSample / 8)
	mutex.Unlock()

	v := telemetryData{
		Uptime:     time.Since(bootTime).String(),
		AppVersion: appVersion,
		CPUPerCore: cores,
		RAMApp:     ramMB(os.Getpid(), "VmRSS"),
		CPUTemp:    -1,
	}
	if infernoPid != 0 {
		v.RAMInferno = ramMB(infernoPid, "VmRSS")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(RecordPath, &stat); err == nil {
		v.DiskTotal = float64(stat.Blocks*uint64(stat.Bsize)) / 1e9
		v.DiskFree = float64(stat.Bavail*uint64(stat.Bsize)) / 1e9
	}
	if bps > 0 && v.DiskFree > 0 {
		v.RecordTime = formatDuration(time.Duration(v.DiskFree*1e9/bps) * time.Second)
	} else {
		v.RecordTime = "\u2014"
	}
	ramSysUsed, ramSysTotal := systemRAM()
	v.RAMSysUsed, v.RAMSysTotal = ramSysUsed, ramSysTotal
	if t, ok := readCPUTemp(); ok {
		v.CPUTemp = t
	}
	if v.RAMInferno == 0 {
		v.RAMInferno = -1
	}
	return v
}

// Telemetry history feeds the dashboard's CPU/RAM time graphs: one sample
// every teleHistStep, newest last, capped at teleHistN (150 x 2s = 5 min).
// CPU stores the cross-core average - per-core lines would fan out with core
// count while the interesting signal on a recorder is overall load.
const teleHistN = 150
const teleHistStep = 2 * time.Second

var teleHistT []int64
var teleHistCPU, teleHistRAMApp, teleHistRAMSys []float64
var teleHistTemp, teleHistDisk []float64

// teleHistCores mirrors teleHistT row-for-row: one per-core snapshot each.
// Empty cpuPct repeats the previous row so columns never go ragged.
var teleHistCores [][]float64

// appendTelemetryHist records one history sample; trims equally so the
// parallel slices can never drift apart in length. Filesystem sampling
// happens lock-free (see snapshotTelemetry); only the slice appends hold
// the mutex.
func appendTelemetryHist() {
	mutex.Lock()
	cores := append([]float64(nil), cpuPct...)
	if len(cores) == 0 && len(teleHistCores) > 0 {
		cores = append([]float64(nil), teleHistCores[len(teleHistCores)-1]...)
	}
	avg := 0.0
	for _, p := range cores {
		avg += p
	}
	if len(cores) > 0 {
		avg /= float64(len(cores))
	}
	mutex.Unlock()

	sysUsed, _ := systemRAM()
	temp, _ := readCPUTemp()
	diskFree := -1.0
	var stat syscall.Statfs_t
	if err := syscall.Statfs(RecordPath, &stat); err == nil {
		diskFree = float64(stat.Bavail*uint64(stat.Bsize)) / 1e9
	}
	ramApp := ramMB(os.Getpid(), "VmRSS")

	mutex.Lock()
	defer mutex.Unlock()
	teleHistCores = append(teleHistCores, cores)
	teleHistT = append(teleHistT, time.Now().Unix())
	teleHistCPU = append(teleHistCPU, avg)
	teleHistRAMApp = append(teleHistRAMApp, ramApp)
	teleHistRAMSys = append(teleHistRAMSys, sysUsed)
	teleHistTemp = append(teleHistTemp, temp)
	teleHistDisk = append(teleHistDisk, diskFree)
	if len(teleHistT) > teleHistN {
		cut := len(teleHistT) - teleHistN
		teleHistT = append([]int64(nil), teleHistT[cut:]...)
		teleHistCPU = append([]float64(nil), teleHistCPU[cut:]...)
		teleHistRAMApp = append([]float64(nil), teleHistRAMApp[cut:]...)
		teleHistRAMSys = append([]float64(nil), teleHistRAMSys[cut:]...)
		teleHistCores = append([][]float64(nil), teleHistCores[cut:]...)
		teleHistTemp = append([]float64(nil), teleHistTemp[cut:]...)
		teleHistDisk = append([]float64(nil), teleHistDisk[cut:]...)
	}
}

func telemetryHistLoop() {
	ticker := time.NewTicker(teleHistStep)
	defer ticker.Stop()
	for range ticker.C {
		appendTelemetryHist()
	}
}
