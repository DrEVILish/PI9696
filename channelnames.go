package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Per-recording channel names.
//
// Owner decision 2026-10-05: channel names are read from inferno (whatever
// a network controller has named the unit's RX channels) and snapshotted
// into each recording when the take starts; a recording's channels can then
// be renamed from the WebUI/API only. pi9696 never writes channel names to
// inferno - the snapshot and its renames live in a sidecar next to the take
// (<take>.channels.json), carried along by downloads, USB copies and deletes.

// infernoStateDir is where the in-process inferno keeps its saved state
// (INFERNO_STATE_DIR, fork 0501a56): channel names and subscriptions. A
// var so tests can point it at a temp dir.
var infernoStateDir = "/var/lib/pi9696/inferno-state"

// recChannel is one channel of a recording.
type recChannel struct {
	Number int    `json:"number"`
	Name   string `json:"name"`             // the recording's name for it (renamable)
	Source string `json:"source,omitempty"` // what fed it when the take started, e.g. "Left@AVIO-USB"
	Device string `json:"device,omitempty"` // the unit's RX channel name at the time, as the controller showed it
}

// recChannelsFile is the sidecar's content.
type recChannelsFile struct {
	Channels []recChannel `json:"channels"`
}

func channelsSidecar(wav string) string {
	return strings.TrimSuffix(wav, filepath.Ext(wav)) + ".channels.json"
}

// parseInfernoChannelsTOML reads inferno's [[channels]] state files (the
// handful of "key = value" lines the fork writes; no TOML library needed)
// into one map per entry.
func parseInfernoChannelsTOML(r io.Reader) []map[string]string {
	var out []map[string]string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "[[channels]]":
			out = append(out, map[string]string{})
		case line == "" || strings.HasPrefix(line, "#") || len(out) == 0:
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			}
			out[len(out)-1][strings.TrimSpace(k)] = v
		}
	}
	return out
}

// infernoRxChannels returns the unit's first n RX channels as inferno has
// them now: names as set from a controller (default "RX <n>") and their
// current subscription source. Read-only: nothing here writes to inferno.
func infernoRxChannels(n int) []recChannel {
	names := map[int]string{}
	sources := map[int]string{}
	if f, err := os.Open(filepath.Join(infernoStateDir, "rx_channels.toml")); err == nil {
		for _, e := range parseInfernoChannelsTOML(f) {
			if id, err := strconv.Atoi(e["id"]); err == nil && e["friendly_name"] != "" {
				names[id] = e["friendly_name"]
			}
		}
		f.Close()
	}
	if f, err := os.Open(filepath.Join(infernoStateDir, "rx_subscriptions.toml")); err == nil {
		for _, e := range parseInfernoChannelsTOML(f) {
			if id, err := strconv.Atoi(e["local_channel_id"]); err == nil && e["tx_channel_name"] != "" {
				sources[id] = e["tx_channel_name"] + "@" + e["tx_hostname"]
			}
		}
		f.Close()
	}
	out := make([]recChannel, n)
	for i := range out {
		id := i + 1
		dev := names[id]
		if dev == "" {
			dev = fmt.Sprintf("RX %d", id)
		}
		out[i] = recChannel{Number: id, Name: dev, Source: sources[id], Device: dev}
	}
	return out
}

// snapshotRecordingChannels writes a new take's sidecar from inferno's
// current channel names. A failure only costs the names (logged): the
// take itself must never depend on it.
func snapshotRecordingChannels(wav string, channels int) {
	if err := writeRecordingChannels(wav, infernoRxChannels(channels)); err != nil {
		logWarnf("channel names for %s: %v", filepath.Base(wav), err)
	}
}

func writeRecordingChannels(wav string, chans []recChannel) error {
	data, err := json.MarshalIndent(recChannelsFile{Channels: chans}, "", "  ")
	if err != nil {
		return err
	}
	path := channelsSidecar(wav)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordingChannels returns a take's channels: its sidecar, else (older
// takes, or a lost sidecar) numbered defaults for the channel count in its
// filename.
func recordingChannels(wav string) []recChannel {
	if data, err := os.ReadFile(channelsSidecar(wav)); err == nil {
		var f recChannelsFile
		if json.Unmarshal(data, &f) == nil && len(f.Channels) > 0 {
			sort.Slice(f.Channels, func(i, j int) bool { return f.Channels[i].Number < f.Channels[j].Number })
			return f.Channels
		}
	}
	n := 0
	if m := recFilenameRe.FindStringSubmatch(filepath.Base(wav)); m != nil {
		n, _ = strconv.Atoi(m[4])
	}
	out := make([]recChannel, n)
	for i := range out {
		out[i] = recChannel{Number: i + 1, Name: fmt.Sprintf("Ch %d", i+1)}
	}
	return out
}

// maxChannelNameLen bounds a recording channel name (it ends up in table
// cells, manifests and filenames of exports).
const maxChannelNameLen = 32

// validChannelName: printable, no control characters, 1-32 runes.
func validChannelName(s string) bool {
	if s == "" || len([]rune(s)) > maxChannelNameLen {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// renameRecordingChannels sets a take's channel names (1-based number ->
// name), keeping each channel's source and device name. WebUI/API only;
// inferno is never touched.
func renameRecordingChannels(wav string, names map[int]string) error {
	chans := recordingChannels(wav)
	for num, name := range names {
		name = strings.TrimSpace(name)
		if num < 1 || num > len(chans) {
			return fmt.Errorf("channel %d: the take has %d channels", num, len(chans))
		}
		if !validChannelName(name) {
			return fmt.Errorf("channel %d: name must be 1-%d printable characters", num, maxChannelNameLen)
		}
		chans[num-1].Name = name
	}
	return writeRecordingChannels(wav, chans)
}

// migrateInfernoState moves inferno's saved state into infernoStateDir the
// first time the app pins it there: until then the plugin kept it under
// the per-user state dir in a subdirectory named after its IP-derived
// device id. The most recently written such directory wins.
func migrateInfernoState() {
	if entries, err := os.ReadDir(infernoStateDir); err == nil && len(entries) > 0 {
		return
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		base = filepath.Join(home, ".local", "state")
	}
	dirs, _ := filepath.Glob(filepath.Join(base, "inferno_aoip", "*"))
	var newest string
	var newestMod int64
	for _, d := range dirs {
		if fi, err := os.Stat(filepath.Join(d, "rx_subscriptions.toml")); err == nil && fi.ModTime().UnixNano() > newestMod {
			newest, newestMod = d, fi.ModTime().UnixNano()
		}
	}
	if err := os.MkdirAll(infernoStateDir, 0o755); err != nil {
		logWarnf("inferno state: cannot create %s: %v", infernoStateDir, err)
		return
	}
	if newest == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(newest, "*.toml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			err = os.WriteFile(filepath.Join(infernoStateDir, filepath.Base(f)), data, 0o644)
		}
		if err != nil {
			logWarnf("inferno state: migrating %s: %v", f, err)
		}
	}
	logInfof("inferno state migrated from %s to %s", newest, infernoStateDir)
}

// channelSummary is the recordings table's one-line view of a take's
// channel names.
func channelSummary(chans []recChannel) string {
	names := make([]string, 0, 4)
	for i, c := range chans {
		if i == 3 {
			return strings.Join(names, ", ") + fmt.Sprintf(" +%d", len(chans)-3)
		}
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// handleAPIRecordingChannels reads (GET ?file=) or renames (POST file=,
// name_<n>=...) a take's channel names. JSON for API clients; the WebUI's
// htmx form gets the refreshed recordings table. Only the take's sidecar
// changes - never inferno.
func handleAPIRecordingChannels(w http.ResponseWriter, r *http.Request) {
	f, ok := resolveRecording(r.FormValue("file"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		mutex.Lock()
		active := isRecording && f == recordingFile
		mutex.Unlock()
		names := map[int]string{}
		for key, vals := range r.Form {
			if n, ok := strings.CutPrefix(key, "name_"); ok && len(vals) > 0 {
				num, err := strconv.Atoi(n)
				if err != nil {
					http.Error(w, "bad field "+key, http.StatusBadRequest)
					return
				}
				names[num] = vals[0]
			}
		}
		if active {
			http.Error(w, "that take is still being recorded", http.StatusConflict)
			return
		}
		if err := renameRecordingChannels(f, names); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logInfof("Channel names of %s changed via remote", filepath.Base(f))
		if r.Header.Get("HX-Request") != "" {
			writeRecordingsTable(w)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(recChannelsFile{Channels: recordingChannels(f)})
}
