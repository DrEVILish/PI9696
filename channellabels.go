package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// channelLabels are the unit's own names for its input channels (1-based
// channel number -> label), set by double-clicking a meter's name in the
// WebUI. Owner rule: renaming happens in the WebUI/API only and never
// changes inferno's channel names, so a label overrides the inferno name
// for display (meters) and for new takes' channel names; clearing it shows
// the inferno name again. Persisted in the config. Guarded by the app mutex.
var channelLabels = map[int]string{}

// validChannelLabels keeps the well-formed entries of a loaded map.
func validChannelLabels(in map[int]string) map[int]string {
	out := map[int]string{}
	for ch, name := range in {
		if ch >= 1 && ch <= MaxChannelCount && validChannelName(name) {
			out[ch] = name
		}
	}
	return out
}

func copyChannelLabels(in map[int]string) map[int]string {
	out := make(map[int]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// applyChannelLabels overlays labels on names (index i is channel i+1).
func applyChannelLabels(names []string, labels map[int]string) []string {
	for i := range names {
		if l, ok := labels[i+1]; ok {
			names[i] = l
		}
	}
	return names
}

// displayChannelNames is what the meters show for n channels: the label
// where one is set, else inferno's RX channel name. Takes the app mutex.
func displayChannelNames(n int) []string {
	names := liveRxChannelNames(n)
	mutex.Lock()
	labels := copyChannelLabels(channelLabels)
	mutex.Unlock()
	return applyChannelLabels(names, labels)
}

// handleAPIChannelLabel sets (form channel=<n>, name=<label>) or clears
// (empty name) one channel's label. Answers JSON {channel, name, label}:
// name is what the meter now shows, label the stored label ("" if none).
func handleAPIChannelLabel(w http.ResponseWriter, r *http.Request) {
	ch, err := strconv.Atoi(r.FormValue("channel"))
	if err != nil || ch < 1 || ch > MaxChannelCount {
		http.Error(w, "channel must be 1-128", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name != "" && !validChannelName(name) {
		http.Error(w, "name must be 1-32 printable characters", http.StatusBadRequest)
		return
	}
	mutex.Lock()
	if name == "" {
		delete(channelLabels, ch)
	} else {
		channelLabels[ch] = name
	}
	settingChanged()
	mutex.Unlock()
	if name == "" {
		logInfof("Channel %d label cleared via remote", ch)
	} else {
		logInfof("Channel %d labelled %q via remote", ch, name)
	}
	shown := displayChannelNames(ch)[ch-1]
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"channel": ch, "name": shown, "label": name})
}
