package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRxChannelsTOML = `[[channels]]
id = 1
friendly_name = "Kick"

[[channels]]
id = 2
friendly_name = "Snare \"top\""
`

const testRxSubsTOML = `[[channels]]
local_channel_id = 1
local_channel_name = "Kick"
tx_channel_name = "Left"
tx_hostname = "AVIO-USB"
`

// useInfernoState points the app at a temp inferno state dir holding the
// given files.
func useInfernoState(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := infernoStateDir
	infernoStateDir = dir
	t.Cleanup(func() { infernoStateDir = orig })
	return dir
}

func TestInfernoRxChannelsReadsNamesAndSources(t *testing.T) {
	useInfernoState(t, map[string]string{"rx_channels.toml": testRxChannelsTOML, "rx_subscriptions.toml": testRxSubsTOML})
	got := infernoRxChannels(3)
	want := []recChannel{
		{Number: 1, Name: "Kick", Source: "Left@AVIO-USB", Device: "Kick"},
		{Number: 2, Name: `Snare "top"`, Device: `Snare "top"`},
		{Number: 3, Name: "RX 3", Device: "RX 3"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("channel %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
	// No state at all: numbered defaults, no error.
	useInfernoState(t, nil)
	if got := infernoRxChannels(2); got[1].Name != "RX 2" {
		t.Errorf("without state: %+v", got)
	}
}

// A take snapshots inferno's channel names when it starts; renaming the
// take's channels (WebUI/API) changes only its sidecar - inferno's state is
// never written.
func TestRecordingChannelNamesSnapshotAndRename(t *testing.T) {
	stateDir := useInfernoState(t, map[string]string{"rx_channels.toml": testRxChannelsTOML, "rx_subscriptions.toml": testRxSubsTOML})
	before, _ := os.ReadFile(filepath.Join(stateDir, "rx_channels.toml"))
	startFakeTake(t)
	mutex.Lock()
	take := recordingFile
	mutex.Unlock()
	// The fake recorder writes nothing; stand in for its growing WAV.
	if err := os.WriteFile(take, make([]byte, 44+6*48000), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(channelsSidecar(take))
	if err != nil {
		t.Fatalf("no channel names were saved with the take: %v", err)
	}
	if !strings.Contains(string(data), `"Kick"`) || !strings.Contains(string(data), "Left@AVIO-USB") {
		t.Fatalf("snapshot lacks inferno's names/sources: %s", data)
	}

	// The take is still recording: renames wait until it is finished.
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	rel, _ := filepath.Rel(RecordPath, take)
	post := func(form url.Values, htmx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/recordings/channels", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	if rr := post(url.Values{"file": {rel}, "name_1": {"Bass drum"}}, false); rr.Code != http.StatusConflict {
		t.Fatalf("rename during the take = %d, want 409", rr.Code)
	}
	mutex.Lock()
	stopRecording()
	done := recordingDone
	mutex.Unlock()
	if done != nil {
		<-done
	}

	if rr := post(url.Values{"file": {rel}, "name_1": {"Bass drum"}}, false); rr.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rr.Code, rr.Body.String())
	} else {
		var f recChannelsFile
		json.Unmarshal(rr.Body.Bytes(), &f)
		if f.Channels[0].Name != "Bass drum" || f.Channels[0].Device != "Kick" || f.Channels[0].Source == "" {
			t.Fatalf("after rename: %+v", f.Channels)
		}
	}
	if rr := post(url.Values{"file": {rel}, "name_2": {"Snare"}}, true); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Bass drum, Snare") {
		t.Fatalf("htmx rename should return the table with the new names (%d): %.300s", rr.Code, rr.Body.String())
	}
	for _, bad := range []url.Values{{"file": {rel}, "name_9": {"x"}}, {"file": {rel}, "name_1": {""}}, {"file": {rel}, "name_1": {strings.Repeat("x", 33)}}, {"file": {rel}, "name_one": {"x"}}} {
		if rr := post(bad, false); rr.Code != http.StatusBadRequest {
			t.Errorf("bad rename %v = %d, want 400", bad, rr.Code)
		}
	}
	if rr := post(url.Values{"file": {"../etc/passwd"}, "name_1": {"x"}}, false); rr.Code != http.StatusNotFound {
		t.Errorf("unknown take = %d, want 404", rr.Code)
	}
	after, _ := os.ReadFile(filepath.Join(stateDir, "rx_channels.toml"))
	if !bytes.Equal(before, after) {
		t.Fatal("renaming a take's channels wrote to inferno's channel names")
	}

	// The names travel with the take in the download-all bundle.
	var buf bytes.Buffer
	if err := writeRecordingZip(&buf, RecordPath, []string{take}); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var sawSidecar bool
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, ".channels.json") {
			sawSidecar = true
		}
		if zf.Name == "manifest.txt" {
			rc, _ := zf.Open()
			m, _ := io.ReadAll(rc)
			rc.Close()
			if !strings.Contains(string(m), "Bass drum  <- Left@AVIO-USB") {
				t.Errorf("manifest lacks the channel names:\n%s", m)
			}
		}
	}
	if !sawSidecar {
		t.Error("the bundle lacks the take's channel names")
	}

	// Deleting the takes deletes their names too.
	deleteAllRecordings()
	if _, err := os.Stat(channelsSidecar(take)); !os.IsNotExist(err) {
		t.Error("delete-all left the take's channel names behind")
	}
}

// A take without a sidecar (older takes) shows numbered names from its
// filename's channel count.
func TestRecordingChannelsDefaultsWithoutSidecar(t *testing.T) {
	wav := filepath.Join(t.TempDir(), "recording_20260101_000000_ch3_48kHz.wav")
	got := recordingChannels(wav)
	if len(got) != 3 || got[2].Name != "Ch 3" {
		t.Fatalf("defaults = %+v", got)
	}
	if s := channelSummary(append(got, recChannel{Number: 4, Name: "Ch 4"})); s != "Ch 1, Ch 2, Ch 3 +1" {
		t.Errorf("summary = %q", s)
	}
}

// The first start with a pinned state dir carries over the plugin's old
// per-device-id state (the most recent one).
func TestMigrateInfernoState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	old := filepath.Join(home, "inferno_aoip", "0000c00002450000")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "rx_subscriptions.toml"), []byte(testRxSubsTOML), 0o644)
	os.WriteFile(filepath.Join(old, "rx_channels.toml"), []byte(testRxChannelsTOML), 0o644)
	orig := infernoStateDir
	infernoStateDir = filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { infernoStateDir = orig })

	migrateInfernoState()
	if got, _ := os.ReadFile(filepath.Join(infernoStateDir, "rx_channels.toml")); string(got) != testRxChannelsTOML {
		t.Fatalf("state not migrated: %q", got)
	}
	// Never again once the pinned dir has state.
	os.WriteFile(filepath.Join(old, "rx_channels.toml"), []byte("changed"), 0o644)
	migrateInfernoState()
	if got, _ := os.ReadFile(filepath.Join(infernoStateDir, "rx_channels.toml")); string(got) != testRxChannelsTOML {
		t.Fatal("migration ran again over existing state")
	}
}

// The unit's own state directory wins over a more recently written one
// from a loopback test instance or an old address.
func TestPickInfernoStateDirPrefersTheUnitsOwn(t *testing.T) {
	root := t.TempDir()
	mk := func(id string) string {
		d := filepath.Join(root, id)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "rx_subscriptions.toml"), []byte(testRxSubsTOML), 0o644)
		return d
	}
	own := mk("0000c00002450000") // 192.0.2.69
	mk("00007f0000010002")        // loopback test instance, written later
	old := mk("0000c000020a0000") // an old address
	ip := net.ParseIP("192.0.2.69")
	if got := pickInfernoStateDir(root, []net.IP{net.ParseIP("127.0.0.1"), ip}); got != own {
		t.Fatalf("picked %s, want the unit's own %s", got, own)
	}
	// Without a match: the newest non-loopback directory.
	if got := pickInfernoStateDir(root, []net.IP{net.ParseIP("10.0.0.5")}); got != old && got != own {
		t.Fatalf("fallback picked %s (loopback must never win)", got)
	}
	os.RemoveAll(own)
	os.RemoveAll(old)
	if got := pickInfernoStateDir(root, nil); got != "" {
		t.Fatalf("only a loopback directory left, picked %s", got)
	}
}
