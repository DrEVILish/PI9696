package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi9696/hardware"
)

func TestHyperdeckParse(t *testing.T) {
	for _, tc := range []struct {
		line   string
		verb   string
		params map[string]string
	}{
		{"play", "play", map[string]string{}},
		{"transport info", "transport info", map[string]string{}},
		{"PLAY", "play", map[string]string{}},
		{"play: speed: 100", "play", map[string]string{"speed": "100"}},
		{"notify: transport: true", "notify", map[string]string{"transport": "true"}},
		{"slot info: slot id: 2", "slot info", map[string]string{"slot id": "2"}},
		// Timecode values contain colons and must not split into pairs.
		{"goto: timecode: 00:01:02:03", "goto", map[string]string{"timecode": "00:01:02:03"}},
		{"record: name: Take One", "record", map[string]string{"name": "Take One"}},
	} {
		verb, params := hyperdeckParse(tc.line)
		if verb != tc.verb {
			t.Errorf("%q verb = %q, want %q", tc.line, verb, tc.verb)
		}
		if len(params) != len(tc.params) {
			t.Errorf("%q params = %v, want %v", tc.line, params, tc.params)
			continue
		}
		for k, want := range tc.params {
			if params[k] != want {
				t.Errorf("%q param %q = %q, want %q", tc.line, k, params[k], want)
			}
		}
	}
}

func TestHyperdeckTimecode(t *testing.T) {
	if got := hyperdeckTimecode(0); got != "00:00:00:00" {
		t.Errorf("zero = %q", got)
	}
	// 3723.04s = 1h02m03s + 1 frame @25fps.
	if got := hyperdeckTimecode(3723*time.Second + 40*time.Millisecond); got != "01:02:03:01" {
		t.Errorf("got %q", got)
	}
	if got := hyperdeckTimecode(-time.Second); got != "00:00:00:00" {
		t.Errorf("negative = %q", got)
	}
}

// hyperdeckDial starts an isolated server and returns a line scanner over
// the session (greeting already consumed) plus cleanup.
func hyperdeckDial(t *testing.T) (*bufio.Scanner, net.Conn, func()) {
	t.Helper()
	old := hyperdeckBindAddr
	hyperdeckBindAddr = "127.0.0.1:0"
	mutex.Lock()
	setHyperdeckEnabledLocked(true)
	mutex.Unlock()
	if !hyperdeckRunning() {
		hyperdeckBindAddr = old
		t.Fatal("server did not start")
	}
	hyperdeckMu.Lock()
	addr := hyperdeckListener.Addr().String()
	hyperdeckMu.Unlock()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 65536), 65536)
	// Consume the 500 greeting block through its terminating blank line.
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			break
		}
	}
	cleanup := func() {
		c.Close()
		mutex.Lock()
		setHyperdeckEnabledLocked(false)
		mutex.Unlock()
		hyperdeckBindAddr = old
	}
	return sc, c, cleanup
}

// hyperdeckCmd sends one line and reads the response: a single line for
// 1xx/200, or a block through the blank line for multi-line codes.
func hyperdeckCmd(t *testing.T, sc *bufio.Scanner, c net.Conn, cmd string) (string, []string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(cmd + "\n")); err != nil {
		t.Fatal(err)
	}
	if !sc.Scan() {
		t.Fatalf("%q: no response", cmd)
	}
	first := strings.TrimSpace(sc.Text())
	if !strings.HasSuffix(first, ":") {
		return first, nil // single-line 200/1xx
	}
	var lines []string
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(l) == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(l))
	}
	return first, lines
}

func hyperdeckHas(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestHyperdeckProtocolSmoke(t *testing.T) {
	sc, c, cleanup := hyperdeckDial(t)
	defer cleanup()

	if first, _ := hyperdeckCmd(t, sc, c, "ping"); first != "200 ok" {
		t.Errorf("ping = %q", first)
	}
	if first, lines := hyperdeckCmd(t, sc, c, "device info"); first != "204 device info:" {
		t.Errorf("device info = %q", first)
	} else if !hyperdeckHas(lines, "model: PI9696") {
		t.Errorf("device info missing model: %v", lines)
	}
	if first, lines := hyperdeckCmd(t, sc, c, "transport info"); first != "208 transport info:" {
		t.Errorf("transport info = %q", first)
	} else if !hyperdeckHas(lines, "status: stopped") {
		t.Errorf("idle status = %v", lines)
	}
	if first, lines := hyperdeckCmd(t, sc, c, "clips count"); first != "214 clips count:" {
		t.Errorf("clips count = %q", first)
	} else {
		found := false
		for _, l := range lines {
			found = found || strings.HasPrefix(l, "count: ")
		}
		if !found {
			t.Errorf("clips count missing count: %v", lines)
		}
	}
	if first, lines := hyperdeckCmd(t, sc, c, "notify: transport: true"); first != "209 notify:" {
		t.Errorf("notify = %q", first)
	} else if !hyperdeckHas(lines, "transport: true") {
		t.Errorf("notify echo = %v", lines)
	}
	// Unknown verbs are syntax errors; real-but-unimplemented verbs are
	// "unsupported" - controllers tell the difference.
	if first, _ := hyperdeckCmd(t, sc, c, "frobnicate"); !strings.HasPrefix(first, "100") {
		t.Errorf("garbage = %q, want 100", first)
	}
	if first, _ := hyperdeckCmd(t, sc, c, "preview: enable: true"); !strings.HasPrefix(first, "103") {
		t.Errorf("preview = %q, want 103", first)
	}
	if first, _ := hyperdeckCmd(t, sc, c, "slot info: slot id: 2"); !strings.HasPrefix(first, "102") {
		t.Errorf("bad slot = %q, want 102", first)
	}
	// Invalid take names are rejected before touching the transport.
	if first, _ := hyperdeckCmd(t, sc, c, "record: name: bad/name"); !strings.HasPrefix(first, "102") {
		t.Errorf("bad name = %q, want 102", first)
	}
	// Rewind-to-top with nothing playing reports not-playing, not
	// unsupported; other goto targets are unsupported.
	if first, _ := hyperdeckCmd(t, sc, c, "goto: timeline: 0"); !strings.HasPrefix(first, "103") {
		t.Errorf("idle goto-top = %q, want 103", first)
	}
	if first, _ := hyperdeckCmd(t, sc, c, "goto: clip id: 3"); !strings.HasPrefix(first, "103") {
		t.Errorf("goto clip = %q, want 103", first)
	}
	// Subscription values are case-insensitive.
	if _, lines := hyperdeckCmd(t, sc, c, "notify: transport: TRUE"); !hyperdeckHas(lines, "transport: true") {
		t.Errorf("notify TRUE echo = %v", lines)
	}
	if first, _ := hyperdeckCmd(t, sc, c, "stop"); first != "200 ok" {
		t.Errorf("idle stop = %q", first)
	}
	if first, _ := hyperdeckCmd(t, sc, c, "quit"); first != "200 ok" {
		t.Errorf("quit = %q", first)
	}
}

func TestHyperdeckToggleRoundTrip(t *testing.T) {
	old := hyperdeckBindAddr
	hyperdeckBindAddr = "127.0.0.1:0"
	defer func() { hyperdeckBindAddr = old }()

	mutex.Lock()
	setHyperdeckEnabledLocked(true)
	on := hyperdeckEnabled && hyperdeckRunning()
	setHyperdeckEnabledLocked(false)
	off := !hyperdeckEnabled && !hyperdeckRunning()
	mutex.Unlock()
	if !on || !off {
		t.Errorf("toggle round-trip on=%v off=%v", on, off)
	}
}

func TestHyperdeckNotifyConcurrent(t *testing.T) {
	sc, _, cleanup := hyperdeckDial(t)
	defer cleanup()

	// Subscribe, then hammer subscription state and notify pushes from
	// many goroutines: the ticker and the command loop touch both
	// concurrently in production (see smu).
	h := &hyperdeckConn{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h.setTransportSubscribed(i%2 == 0)
				_ = h.transportSubscribed()
				h.snapshotSig()
				h.pushTransportNotify()
			}
		}(g)
	}
	wg.Wait()
	_ = sc
}

// TestHyperdeckPlayIdempotent drives real (fake-ffmpeg) playback and proves
// the protocol verb doesn't inherit the physical PLAY key's toggle: re-sent
// play is a no-op 200 while playing, speed 0 pauses, and play resumes.
func TestHyperdeckPlayIdempotent(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	if err := os.MkdirAll(RecordPath, 0755); err != nil {
		t.Fatal(err)
	}
	recFile := filepath.Join(RecordPath, "recording_20260101_000002_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()
	onButtonPress(hardware.PlayButton)
	mutex.Lock()
	if currentState != StatePlaying {
		mutex.Unlock()
		t.Fatal("setup: expected playback to start")
	}
	mutex.Unlock()
	t.Cleanup(func() {
		onButtonPress(hardware.StopButton)
		waitForPlaybackIdle(t)
	})

	sc, c, cleanup := hyperdeckDial(t)
	defer cleanup()

	state := func() AppState {
		mutex.Lock()
		defer mutex.Unlock()
		return currentState
	}
	if first, _ := hyperdeckCmd(t, sc, c, "play"); first != "200 ok" {
		t.Fatalf("play while playing = %q, want 200 ok", first)
	}
	if state() != StatePlaying {
		t.Fatal("re-sent play paused the deck; the verb must be idempotent")
	}
	if first, _ := hyperdeckCmd(t, sc, c, "play: speed: 0"); first != "200 ok" {
		t.Fatalf("play speed 0 = %q, want 200 ok", first)
	}
	if state() != StatePaused {
		t.Fatal("play speed 0 should hold the track paused")
	}
	if first, _ := hyperdeckCmd(t, sc, c, "play"); first != "200 ok" {
		t.Fatalf("play while paused = %q, want 200 ok", first)
	}
	if state() != StatePlaying {
		t.Fatal("play while paused should resume")
	}
}
