package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useChannelLabels starts a test with the given labels, restoring the
// unit's afterwards.
func useChannelLabels(t *testing.T, labels map[int]string) {
	t.Helper()
	mutex.Lock()
	orig := channelLabels
	channelLabels = map[int]string{}
	maps.Copy(channelLabels, labels)
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		channelLabels = orig
		mutex.Unlock()
	})
}

func postChannelLabel(t *testing.T, mux http.Handler, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/channels/label", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// A label overrides inferno's name on the meters; clearing it brings the
// inferno name back; inferno's own state is never written.
func TestChannelLabelSetAndClear(t *testing.T) {
	dir := useInfernoState(t, map[string]string{"rx_channels.toml": testRxChannelsTOML})
	useChannelLabels(t, nil)
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	rr := postChannelLabel(t, mux, cookie, url.Values{"channel": {"2"}, "name": {" Snare btm "}})
	if rr.Code != http.StatusOK {
		t.Fatalf("set label = %d %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got["name"] != "Snare btm" {
		t.Fatalf("set label answered %s", rr.Body.String())
	}
	if names := displayChannelNames(3); names[0] != "Kick" || names[1] != "Snare btm" || names[2] != "RX 3" {
		t.Errorf("meter names = %q", names)
	}
	mutex.Lock()
	saved := currentConfig().ChannelLabels
	mutex.Unlock()
	if saved[2] != "Snare btm" {
		t.Errorf("label not in the persisted config: %v", saved)
	}

	rr = postChannelLabel(t, mux, cookie, url.Values{"channel": {"2"}, "name": {""}})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear label = %d", rr.Code)
	}
	if names := displayChannelNames(2); names[1] != `Snare "top"` {
		t.Errorf("cleared label shows %q, want the inferno name", names[1])
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "rx_channels.toml")); string(data) != testRxChannelsTOML {
		t.Error("labelling a channel changed inferno's channel names")
	}
}

func TestChannelLabelRejectsBadInput(t *testing.T) {
	useChannelLabels(t, nil)
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	for _, form := range []url.Values{
		{"channel": {"0"}, "name": {"x"}},
		{"channel": {"129"}, "name": {"x"}},
		{"channel": {"abc"}, "name": {"x"}},
		{"channel": {"1"}, "name": {strings.Repeat("x", 33)}},
		{"channel": {"1"}, "name": {"bad\x01name"}},
	} {
		if rr := postChannelLabel(t, mux, cookie, form); rr.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", form, rr.Code)
		}
	}
	mutex.Lock()
	n := len(channelLabels)
	mutex.Unlock()
	if n != 0 {
		t.Errorf("rejected requests stored labels: %v", channelLabels)
	}
}

// Loaded labels are validated; new takes name their channels by label and
// keep inferno's name as the device name.
func TestChannelLabelsLoadAndNameNewTakes(t *testing.T) {
	useInfernoState(t, map[string]string{"rx_channels.toml": testRxChannelsTOML})
	useChannelLabels(t, nil)
	mutex.Lock()
	// From the current config, so only the labels change (a bare config
	// would reset the sample rate and other settings for later tests).
	cfg := currentConfig()
	cfg.ChannelLabels = map[int]string{1: "Kick in", 0: "bad", 200: "bad", 3: ""}
	applyConfigSettings(&cfg)
	loaded := maps.Clone(channelLabels)
	mutex.Unlock()
	if len(loaded) != 1 || loaded[1] != "Kick in" {
		t.Fatalf("loaded labels = %v, want only channel 1", loaded)
	}

	wav := filepath.Join(t.TempDir(), "take.wav")
	mutex.Lock()
	snapshotRecordingChannels(wav, 2)
	mutex.Unlock()
	chans := recordingChannels(wav)
	if chans[0].Name != "Kick in" || chans[0].Device != "Kick" {
		t.Errorf("channel 1 = %+v, want the label as name and inferno's as device", chans[0])
	}
	if chans[1].Name != `Snare "top"` {
		t.Errorf("unlabelled channel 2 = %+v", chans[1])
	}
}
