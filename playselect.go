package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"pi9696/hardware"
)

// Choosing a take for playback.
//
// Play (panel key, WebUI, HyperDeck) used to always start the newest take.
// The WebUI recordings table now has a play button per row: it selects that
// take and starts it. The selection sticks, so the panel's Play key replays
// the chosen take too, until a take is deleted or another is chosen. With no
// selection (or a stale one) Play falls back to the newest take as before.

// selectedPlayback is the absolute path of the chosen take, "" for the
// newest. Guarded by the app mutex.
var selectedPlayback string

// playbackSourceLocked returns the take Play should start. A selection that
// has disappeared, or is the take being recorded, is dropped. Caller holds
// the app mutex.
func playbackSourceLocked() string {
	if selectedPlayback != "" {
		if _, err := os.Stat(selectedPlayback); err == nil && !(isRecording && selectedPlayback == recordingFile) {
			return selectedPlayback
		}
		selectedPlayback = ""
	}
	return latestRecording()
}

// resolveRecording maps a recordings-table key (path relative to
// RecordPath) to the take's absolute path, whitelisted against the real
// listing exactly like handleDownload.
func resolveRecording(rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	for _, f := range recordingFiles() {
		if r, err := filepath.Rel(RecordPath, f); err == nil && r == rel {
			return f, true
		}
	}
	return "", false
}

// handleAPIPlaybackSelect chooses a take (form "file", the row's RelPath)
// and, with play=1, starts it. A track already playing must be stopped
// first: replacing it silently would be a surprise on a live output.
// Answers with the re-rendered recordings table so the selected row shows.
func handleAPIPlaybackSelect(w http.ResponseWriter, r *http.Request) {
	f, ok := resolveRecording(r.FormValue("file"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	play := r.FormValue("play") == "1"
	mutex.Lock()
	busy := currentState == StatePlaying || currentState == StatePaused || isRecording
	if play && busy {
		showWebNotice("Stop the current recording or playback first")
		mutex.Unlock()
		w.WriteHeader(http.StatusConflict)
		writeRecordingsTable(w)
		return
	}
	if isRecording && f == recordingFile {
		mutex.Unlock()
		http.Error(w, "that take is still being recorded", http.StatusConflict)
		return
	}
	selectedPlayback = f
	mutex.Unlock()
	logInfof("Playback take selected via remote: %s", filepath.Base(f))
	if play {
		onButtonPress(hardware.PlayButton)
	}
	writeRecordingsTable(w)
}

func writeRecordingsTable(w http.ResponseWriter) {
	html, err := renderRecordingsHTML()
	if err != nil {
		http.Error(w, "recordings unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}
