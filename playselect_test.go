package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi9696/hardware"
)

// stageTakes writes two 48 kHz/2ch takes, the second newer, and returns
// their RecordPath-relative keys (older, newer).
func stageTakes(t *testing.T) (string, string) {
	t.Helper()
	day := filepath.Join(RecordPath, "2026-01-01")
	if err := os.MkdirAll(day, 0755); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i, name := range []string{"recording_20260101_000000_ch2_48kHz.wav", "recording_20260101_010000_ch2_48kHz.wav"} {
		p := filepath.Join(day, name)
		if err := os.WriteFile(p, make([]byte, 44+48000*2*3*30), 0644); err != nil {
			t.Fatal(err)
		}
		// In the future, so they are the newest takes whatever earlier
		// tests left in RecordPath.
		mt := time.Now().Add(time.Duration(i+1) * time.Hour)
		os.Chtimes(p, mt, mt)
		t.Cleanup(func() { os.Remove(p) })
		keys = append(keys, filepath.Join("2026-01-01", name))
	}
	return keys[0], keys[1]
}

func postSelect(t *testing.T, mux http.Handler, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/playback/select", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// A row's play button selects that take and starts it; the selection then
// also drives the panel's Play key, and the row is marked in the table.
func TestPlaybackSelectFromTable(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)
	older, newer := stageTakes(t)
	mutex.Lock()
	currentState, isRecording = StateIdle, false
	sampleRateIdx, channelCount = 1, 2
	origSel := selectedPlayback
	selectedPlayback = ""
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		selectedPlayback = origSel
		mutex.Unlock()
	})
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	playing := func() string {
		mutex.Lock()
		defer mutex.Unlock()
		if currentState != StatePlaying {
			return ""
		}
		r, _ := filepath.Rel(RecordPath, playbackFile)
		return r
	}

	// Default: the newest take.
	onButtonPress(hardware.PlayButton)
	if got := playing(); got != newer {
		t.Fatalf("Play with no selection played %q, want the newest %q", got, newer)
	}
	// Choosing another take while one plays is refused, not a silent swap.
	if rr := postSelect(t, mux, cookie, url.Values{"file": {older}, "play": {"1"}}); rr.Code != http.StatusConflict {
		t.Fatalf("play-select while playing = %d, want 409", rr.Code)
	}
	if got := playing(); got != newer {
		t.Fatalf("refused select changed the playing take to %q", got)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)

	rr := postSelect(t, mux, cookie, url.Values{"file": {older}, "play": {"1"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("play-select = %d", rr.Code)
	}
	if got := playing(); got != older {
		t.Fatalf("row play started %q, want %q", got, older)
	}
	if !strings.Contains(rr.Body.String(), `class="is-selected"`) {
		t.Error("the selected take is not marked in the returned table")
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)

	// The panel's Play key now replays the chosen take.
	onButtonPress(hardware.PlayButton)
	if got := playing(); got != older {
		t.Fatalf("panel Play after a selection played %q, want %q", got, older)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)

	// A deleted selection falls back to the newest take.
	os.Remove(filepath.Join(RecordPath, older))
	onButtonPress(hardware.PlayButton)
	if got := playing(); got != newer {
		t.Fatalf("Play with a vanished selection played %q, want %q", got, newer)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}

// Only takes in the real listing can be chosen.
func TestPlaybackSelectRejectsUnknownFiles(t *testing.T) {
	initTestHardware(t)
	stageTakes(t)
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	for _, f := range []string{"", "../etc/passwd", "2026-01-01/nope.wav", "/etc/passwd"} {
		if rr := postSelect(t, mux, cookie, url.Values{"file": {f}}); rr.Code != http.StatusNotFound {
			t.Errorf("select %q = %d, want 404", f, rr.Code)
		}
	}
}

// Owner rule: the table's play button loads (queues) a take without
// starting it, even while another take plays; PLAY then starts it.
func TestPlaybackTableButtonLoadsOnly(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)
	older, newer := stageTakes(t)
	mutex.Lock()
	currentState, isRecording = StateIdle, false
	sampleRateIdx, channelCount = 1, 2
	origSel := selectedPlayback
	selectedPlayback = ""
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		selectedPlayback = origSel
		mutex.Unlock()
	})
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	state := func() AppState {
		mutex.Lock()
		defer mutex.Unlock()
		return currentState
	}

	rr := postSelect(t, mux, cookie, url.Values{"file": {older}})
	if rr.Code != http.StatusOK {
		t.Fatalf("load = %d", rr.Code)
	}
	if s := state(); s == StatePlaying {
		t.Fatal("loading a take started playback")
	}
	if !strings.Contains(rr.Body.String(), `>loaded</span>`) {
		t.Error("the loaded take is not marked in the returned table")
	}
	if strings.Contains(rr.Body.String(), `name="play"`) {
		t.Error("the table button still asks to start playback")
	}

	// While a take plays, loading another queues it without a swap.
	onButtonPress(hardware.PlayButton)
	if rr := postSelect(t, mux, cookie, url.Values{"file": {newer}}); rr.Code != http.StatusOK {
		t.Fatalf("load while playing = %d", rr.Code)
	}
	mutex.Lock()
	cur, _ := filepath.Rel(RecordPath, playbackFile)
	mutex.Unlock()
	if cur != older {
		t.Fatalf("loading while playing swapped the take to %q", cur)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
	onButtonPress(hardware.PlayButton)
	mutex.Lock()
	cur, _ = filepath.Rel(RecordPath, playbackFile)
	mutex.Unlock()
	if cur != newer {
		t.Fatalf("PLAY after a queued load played %q, want %q", cur, newer)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}
