package main

import (
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi9696/hardware"
	"pi9696/xlog"
)

// TestMain starts the single infernoWorker every test in this file shares.
// Each test used to spawn its own with "go infernoWorker()", which left
// multiple goroutines draining the same package-level infernoReqCh -
// harmless today only because doStartInferno/doStopInferno are idempotent
// and requests are short-lived, not because it's actually safe by design.
func TestMain(m *testing.M) {
	go infernoWorker()
	os.Exit(m.Run())
}

// fakeExecutable writes an executable shell script named `name` into a temp
// dir and prepends that dir to PATH for the duration of the test, so
// production code that shells out (doStartInferno's "cargo run", ffmpeg for
// playback) exec's a controlled stand-in instead of the real thing - no
// audio hardware or Inferno Rust project needed to test the Go state
// machine around them.
func fakeExecutable(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+":"+oldPath)
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
}

func initTestHardware(t *testing.T) {
	t.Helper()
	os.Setenv("PI9696_SIM", "1")
	if hwManager == nil {
		hm, err := hardware.NewHardwareManager()
		if err != nil {
			t.Fatalf("hardware init failed: %v", err)
		}
		hwManager = hm
	}
}

func withFakeInfernoProject(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll("inferno", 0755); err != nil {
		t.Fatalf("mkdir inferno: %v", err)
	}
	// Write a minimal Go stub instead of Cargo.toml - the production code
	// uses the prebuilt binary at InfernoBinary, not cargo run.
	stub := `package main
import ("flag"; "fmt"; "os"; "time")
func main() {
	channels := flag.Int("c", 2, "channels")
	output := flag.String("o", "", "output FIFO")
	flag.Parse()
	if *output == "" { fmt.Fprintln(os.Stderr, "Usage: inferno -c <channels> -o <output_fifo>"); os.Exit(1) }
	f, _ := os.OpenFile(*output, os.O_WRONLY, 0)
	defer f.Close()
	buf := make([]byte, *channels*4*1024)
	for { f.Write(buf); time.Sleep(10*time.Millisecond) }
}`
	if err := os.WriteFile("inferno/main.go", []byte(stub), 0644); err != nil {
		t.Fatalf("write inferno stub: %v", err)
	}
	if err := os.WriteFile("inferno/go.mod", []byte("module inferno\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write inferno go.mod: %v", err)
	}
	// Build the stub binary to the expected location
	if err := os.MkdirAll("inferno/target/release", 0755); err != nil {
		t.Fatalf("mkdir target/release: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", "inferno/target/release/inferno", "inferno/main.go")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build inferno stub: %v: %s", err, out)
	}
	t.Cleanup(func() { os.RemoveAll("inferno") })
}

// Traps SIGTERM and exits cleanly, standing in for both cargo (Inferno) and
// ffmpeg (playback) - both are just long-running children that must respond
// to SIGTERM for the worker/playback-goroutine cleanup paths under test.
const fakeChildScript = `#!/bin/sh
trap 'exit 0' TERM
sleep 300 &
wait $!
`

func TestInfernoWorkerConcurrency(t *testing.T) {
	initTestHardware(t)
	withFakeInfernoProject(t)
	fakeExecutable(t, "cargo", fakeChildScript)

	// Fire a burst of concurrent start/restart requests - the scenario that
	// used to race on infernoCmd/fifoPath/infernoState before Inferno
	// lifecycle was serialized behind a single worker goroutine.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				enqueueInferno(infernoCmdStart)
			} else {
				enqueueInferno(infernoCmdRestart)
			}
		}(i)
	}
	wg.Wait()

	time.Sleep(500 * time.Millisecond)

	stopInfernoAndWait()

	mutex.Lock()
	state := infernoState
	mutex.Unlock()
	if state != InfernoStopped {
		t.Fatalf("expected InfernoStopped after stopInfernoAndWait, got %v", state)
	}
}

func TestInfernoWorkerDoesNotKillActiveRecording(t *testing.T) {
	initTestHardware(t)
	withFakeInfernoProject(t)
	fakeExecutable(t, "cargo", fakeChildScript)

	done := make(chan struct{})
	infernoReqCh <- infernoRequest{cmd: infernoCmdStart, done: done}
	<-done

	mutex.Lock()
	if infernoState != InfernoRunning {
		mutex.Unlock()
		t.Fatalf("expected InfernoRunning after start, got %v", infernoState)
	}
	// Simulate an in-progress recording directly rather than going through
	// startRecording()/real ffmpeg+FIFO: the thing under test is
	// infernoWorker's recording guard, not the recording pipeline itself.
	isRecording = true
	currentState = StateRecording
	mutex.Unlock()

	enqueueInferno(infernoCmdRestart)
	time.Sleep(300 * time.Millisecond)

	mutex.Lock()
	stillRunning := infernoState == InfernoRunning
	isRecording = false
	currentState = StateIdle
	mutex.Unlock()

	if !stillRunning {
		t.Fatalf("infernoWorker restarted Inferno mid-recording instead of deferring")
	}

	// Now that isRecording is false, a fresh restart request should proceed.
	enqueueInferno(infernoCmdRestart)
	time.Sleep(1500 * time.Millisecond)

	stopInfernoAndWait()
}

func TestMeterReaderParsesAstatsOutput(t *testing.T) {
	origPeak, origRMS := meterPeakDB, meterRMSDB
	t.Cleanup(func() { meterPeakDB, meterRMSDB = origPeak, origRMS })

	input := "frame:0    pts:0       pts_time:0\n" +
		"lavfi.astats.Overall.Peak_level=-18.063656\n" +
		"lavfi.astats.Overall.RMS_level=-21.091526\n" +
		"lavfi.astats.Overall.DC_offset=0.001483\n"

	meterReader(strings.NewReader(input))

	mutex.Lock()
	peak, rms := meterPeakDB, meterRMSDB
	mutex.Unlock()

	if peak != -18.063656 {
		t.Fatalf("expected peak -18.063656, got %v", peak)
	}
	if rms != -21.091526 {
		t.Fatalf("expected rms -21.091526, got %v", rms)
	}
}

// fakeFfmpegDelayedExitScript acknowledges SIGTERM but takes a noticeable
// moment to actually exit, standing in for ffmpeg finalizing a file - long
// enough to prove stopRecording() returns before that finishes rather than
// blocking on it (see startRecording's comment on why cmd.Wait() moved off
// the caller and into its own goroutine).
const fakeFfmpegDelayedExitScript = `#!/bin/sh
trap 'sleep 0.3; exit 0' TERM
sleep 300 &
wait $!
`

func TestStopRecordingDoesNotBlockMutex(t *testing.T) {
	initTestHardware(t)
	withFakeInfernoProject(t)
	fakeExecutable(t, "cargo", fakeChildScript)
	fakeExecutable(t, "ffmpeg", fakeFfmpegDelayedExitScript)

	startDone := make(chan struct{})
	infernoReqCh <- infernoRequest{cmd: infernoCmdStart, done: startDone}
	<-startDone

	mutex.Lock()
	if infernoState != InfernoRunning {
		mutex.Unlock()
		t.Fatalf("expected InfernoRunning after start")
	}
	startRecording()
	recording := isRecording
	mutex.Unlock()
	if !recording {
		t.Fatalf("expected isRecording true after startRecording")
	}

	mutex.Lock()
	stopStart := time.Now()
	stopRecording()
	stopElapsed := time.Since(stopStart)
	mutex.Unlock()

	if stopElapsed > 50*time.Millisecond {
		t.Fatalf("stopRecording blocked for %v - expected a near-instant signal-only return", stopElapsed)
	}

	// The fake ffmpeg takes ~300ms to actually exit after SIGTERM; the
	// owning goroutine started by startRecording should reap it and reset
	// state (including the meter, back to meterSilence) without anyone
	// having called stopRecording again.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mutex.Lock()
		clean := !isRecording && meterPeakDB == meterSilence && currentState == StateIdle
		mutex.Unlock()
		if clean {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording state did not clean up after stop")
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopInfernoAndWait()
}

// TestStartRecordingGuarded covers the shared gate used by both the physical
// Record button and the WebUI /api/record/start endpoint. It must refuse to
// start from a non-idle state or on top of an existing take, and allow a
// clean idle start (the test env has no real disk, so lowDisk() is false -
// zero free space is deliberately not treated as low).
func TestStartRecordingGuarded(t *testing.T) {
	origState, origRec := currentState, isRecording
	origCfg := os.Getenv("PI9696_CONFIG")
	t.Cleanup(func() { currentState, isRecording = origState, origRec; os.Setenv("PI9696_CONFIG", origCfg) })

	initTestHardware(t)
	tmpCfg := filepath.Join(t.TempDir(), "config.json")
	os.Setenv("PI9696_CONFIG", tmpCfg)

	// Refused: already recording.
	mutex.Lock()
	currentState = StateIdle
	isRecording = true
	mutex.Unlock()
	if startRecordingGuarded() {
		mutex.Lock()
		got := isRecording
		mutex.Unlock()
		t.Fatalf("expected guarded start to refuse when already recording (still %v)", got)
	}

	// Refused: not in an idle state.
	mutex.Lock()
	currentState = StateSettings
	isRecording = false
	mutex.Unlock()
	if startRecordingGuarded() {
		t.Fatalf("expected guarded start to refuse from StateSettings")
	}

	// Allowed: idle, not recording, and (in test) not low on disk.
	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()
	if !startRecordingGuarded() {
		t.Fatalf("expected guarded start to succeed from idle")
	}

	// Clean up the started take (no real ffmpeg runs - startRecording spins
	// a real exec.Command; just reset the flag for the rest of the suite).
	mutex.Lock()
	stopRecording()
	isRecording = false
	currentState = StateIdle
	mutex.Unlock()
}

// TestRotationNavigatesUntilRowIsClicked is the regression test for the bug
// where rotation directly adjusted whichever parameter row happened to be
// selected, meaning the cursor could never advance past row 0: every
// rotation changed Sample Rate's value instead of moving to Channel Count.
// Rotation must navigate (move selectedMenu) until the row is explicitly
// entered via a click, and only then adjust its value. Since the settings
// tree was split into sub-menus, the parameter rows now live in the Audio
// sub-menu (StateAudio) behind a StateSettings row; the navigate-until-clicked
// rule is exercised there.
func TestRotationNavigatesUntilRowIsClicked(t *testing.T) {
	origSelected, origEditing, origSampleIdx := selectedMenu, editingParameter, sampleRateIdx
	t.Cleanup(func() {
		selectedMenu, editingParameter, sampleRateIdx = origSelected, origEditing, origSampleIdx
	})

	mutex.Lock()
	currentState = StateSettings
	selectedMenu = 0
	editingParameter = false
	sampleRateIdx = 1
	mutex.Unlock()

	// Rotating on the top-level Settings screen navigates the cursor (row 0
	// -> row 1) without touching any value.
	onEncoderRotate(1)
	mutex.Lock()
	topMovedNotAdjusted := selectedMenu == 1 && sampleRateIdx == 1
	mutex.Unlock()
	if !topMovedNotAdjusted {
		t.Fatalf("expected rotation on Settings to navigate to row 1 without touching sampleRateIdx, got selectedMenu=%d sampleRateIdx=%d", selectedMenu, sampleRateIdx)
	}

	// Clicking row 0 ("Audio") opens the Audio sub-menu, not edit mode.
	mutex.Lock()
	selectedMenu = 0
	mutex.Unlock()
	onEncoderClick()
	mutex.Lock()
	enteredAudio := currentState == StateAudio && !editingParameter
	mutex.Unlock()
	if !enteredAudio {
		t.Fatalf("expected click on Audio row to open the Audio sub-menu, got state=%d editing=%v", currentState, editingParameter)
	}

	// Inside the Audio sub-menu, rotating while not editing must move the
	// cursor, not change Sample Rate's value - the original bug.
	onEncoderRotate(1)
	mutex.Lock()
	movedNotAdjusted := selectedMenu == 1 && sampleRateIdx == 1
	selectedMenu = 0
	mutex.Unlock()
	if !movedNotAdjusted {
		t.Fatalf("expected rotation to navigate to row 1 without touching sampleRateIdx, got selectedMenu=%d sampleRateIdx=%d", selectedMenu, sampleRateIdx)
	}

	// Click on row 0 (Sample Rate) enters edit mode.
	onEncoderClick()
	mutex.Lock()
	entered := editingParameter
	mutex.Unlock()
	if !entered {
		t.Fatalf("expected click on Sample Rate row to enter edit mode")
	}

	// Now rotation must adjust the value, not the cursor, still inside the
	// Audio sub-menu.
	onEncoderRotate(1)
	mutex.Lock()
	adjustedNotMoved := selectedMenu == 0 && sampleRateIdx == 2
	mutex.Unlock()
	if !adjustedNotMoved {
		t.Fatalf("expected rotation in edit mode to adjust sampleRateIdx without moving selectedMenu, got selectedMenu=%d sampleRateIdx=%d", selectedMenu, sampleRateIdx)
	}

	// A further click exits edit mode back to Audio navigation, without also
	// acting on the row underneath.
	onEncoderClick()
	mutex.Lock()
	exited := !editingParameter && currentState == StateAudio
	currentState = StateIdle
	mutex.Unlock()
	if !exited {
		t.Fatalf("expected click to exit edit mode back to Audio navigation")
	}
}

func TestRemoteAuthRedirectsWithoutCookie(t *testing.T) {
	origToken := remoteToken
	remoteToken = "TESTTOKEN1"
	t.Cleanup(func() { remoteToken = origToken })

	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect to /login without a session cookie, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestRemoteLoginWrongTokenThenCorrectToken(t *testing.T) {
	origToken, origLimiter := remoteToken, loginLimit
	remoteToken = "TESTTOKEN2"
	loginLimit = newLoginLimiter() // isolate rate-limit state from other tests
	t.Cleanup(func() { remoteToken, loginLimit = origToken, origLimiter })

	mux := newRemoteMux()

	// Wrong token: rejected, no session cookie issued.
	form := "token=WRONGTOKEN"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			t.Fatalf("session cookie set despite wrong token")
		}
	}

	// Correct token: session cookie issued, and it authorizes a subsequent request.
	form = "token=" + remoteToken
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after correct token, got %d", rec.Code)
	}
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected session cookie after correct token")
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid session cookie, got %d", rec.Code)
	}
}

func TestRemoteLoginRateLimitsRepeatedFailures(t *testing.T) {
	origToken, origLimiter := remoteToken, loginLimit
	remoteToken = "TESTTOKEN3"
	loginLimit = newLoginLimiter()
	t.Cleanup(func() { remoteToken, loginLimit = origToken, origLimiter })

	mux := newRemoteMux()
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/login", strings.NewReader("token=WRONG"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "192.0.2.2:12345"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
	}

	// The 6th attempt, even with the correct token, should now be locked out.
	req := httptest.NewRequest("POST", "/login", strings.NewReader("token="+remoteToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.2:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after 5 failed attempts, got %d", rec.Code)
	}
}

func TestFormatTokenAndNormalizeToken(t *testing.T) {
	if got := formatToken("K7M2QX9F"); got != "K7M2 QX9F" {
		t.Fatalf("formatToken: expected 'K7M2 QX9F', got %q", got)
	}
	for _, input := range []string{"K7M2QX9F", "K7M2 QX9F", "K7M2-QX9F"} {
		if got := normalizeToken(input); got != "K7M2QX9F" {
			t.Fatalf("normalizeToken(%q) = %q, want K7M2QX9F", input, got)
		}
	}
}

// slowWriter blocks every Write() until release is closed, standing in for
// a stalled network client (bad wifi, a backgrounded tab that stopped
// reading) so tests can prove a handler released mutex before writing to
// the client rather than across it.
type slowWriter struct {
	http.ResponseWriter
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newSlowWriter(w http.ResponseWriter) *slowWriter {
	return &slowWriter{ResponseWriter: w, started: make(chan struct{}), release: make(chan struct{})}
}

func (s *slowWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.ResponseWriter.Write(p)
}

// TestDisplayPNGDoesNotBlockMutex guards against handleDisplayPNG
// regressing to encoding straight into the response (which would hold
// mutex for as long as a slow client's network write took, freezing
// render() and every button/encoder callback - the exact bug this endpoint
// shipped with and was fixed before release).
func TestDisplayPNGDoesNotBlockMutex(t *testing.T) {
	initTestHardware(t)
	origToken := remoteToken
	remoteToken = "TESTTOKEN8"
	t.Cleanup(func() { remoteToken = origToken })

	rec := httptest.NewRecorder()
	sw := newSlowWriter(rec)

	req := httptest.NewRequest("GET", "/api/display.png", nil)
	req.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: remoteToken})

	done := make(chan struct{})
	go func() {
		newRemoteMux().ServeHTTP(sw, req)
		close(done)
	}()

	select {
	case <-sw.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler never started writing to the client")
	}

	// The client write is now stuck. If handleDisplayPNG held mutex across
	// it, this Lock would also stall.
	lockAcquired := make(chan struct{})
	go func() {
		mutex.Lock()
		mutex.Unlock()
		close(lockAcquired)
	}()
	select {
	case <-lockAcquired:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("mutex still held while the client write was blocked - handleDisplayPNG is holding the lock across the network write")
	}

	close(sw.release)
	<-done
}

func TestRemoteDisplayPNGEndpoint(t *testing.T) {
	initTestHardware(t)
	origToken := remoteToken
	remoteToken = "TESTTOKEN5"
	t.Cleanup(func() { remoteToken = origToken })

	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/api/display.png", nil)
	req.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: remoteToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("expected image/png content type, got %q", ct)
	}
	if _, err := png.Decode(rec.Body); err != nil {
		t.Fatalf("response body is not a valid PNG: %v", err)
	}
}

// TestRemoteInputEndpointsDriveStateMachine confirms the web input routes
// go through the real onEncoderClick/onEncoderHold functions - the same
// ones physical hardware calls - rather than a parallel implementation, by
// checking they produce the exact state transitions those functions are
// already known to produce.
func TestRemoteInputEndpointsDriveStateMachine(t *testing.T) {
	initTestHardware(t)
	origToken := remoteToken
	remoteToken = "TESTTOKEN6"
	t.Cleanup(func() { remoteToken = origToken })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()

	mux := newRemoteMux()
	sessionCookie := &http.Cookie{Name: remoteSessionCookie, Value: remoteToken}

	post := func(path string) int {
		req := httptest.NewRequest("POST", path, nil)
		req.AddCookie(sessionCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("/api/input/encoder/click"); code != http.StatusNoContent {
		t.Fatalf("expected 204 from encoder click, got %d", code)
	}
	mutex.Lock()
	state := currentState
	mutex.Unlock()
	if state != StateSettings {
		t.Fatalf("expected encoder click from idle to enter Settings, got %v", state)
	}

	if code := post("/api/input/encoder/hold"); code != http.StatusNoContent {
		t.Fatalf("expected 204 from encoder hold, got %d", code)
	}
	mutex.Lock()
	state = currentState
	mutex.Unlock()
	if state != StateIdle {
		t.Fatalf("expected encoder hold to return to idle, got %v", state)
	}
}

func TestRemoteDownloadWhitelistsAgainstRealFiles(t *testing.T) {
	origToken := remoteToken
	remoteToken = "TESTTOKEN4"
	t.Cleanup(func() { remoteToken = origToken })

	os.MkdirAll(RecordPath, 0755)
	realFile := filepath.Join(RecordPath, "recording_20260101_000002_ch2_48kHz.wav")
	os.WriteFile(realFile, []byte("fake wav data"), 0644)
	t.Cleanup(func() { os.Remove(realFile) })

	mux := newRemoteMux()
	sessionCookie := &http.Cookie{Name: remoteSessionCookie, Value: remoteToken}

	// A file that exists on disk but wasn't returned by recordingFiles()
	// (wrong extension) must be rejected, same as an outright bogus name -
	// the whitelist is "what recordingFiles() lists right now", not "what
	// filepath.Base() can be made to look safe".
	decoyFile := filepath.Join(RecordPath, "not-a-recording.txt")
	os.WriteFile(decoyFile, []byte("decoy"), 0644)
	t.Cleanup(func() { os.Remove(decoyFile) })

	for _, name := range []string{"not-a-recording.txt", "nonexistent.wav"} {
		req := httptest.NewRequest("GET", "/download/"+name, nil)
		req.AddCookie(sessionCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for non-whitelisted file %q, got %d", name, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/download/recording_20260101_000002_ch2_48kHz.wav", nil)
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a real whitelisted recording, got %d", rec.Code)
	}
	if rec.Body.String() != "fake wav data" {
		t.Fatalf("unexpected download body: %q", rec.Body.String())
	}

	// A recording in a per-day subfolder is downloadable by its relative path
	// (the RelPath key), and that path is unique even though the basename is
	// shared - the download is keyed on the folder-relative path, not the name.
	subFile := filepath.Join(RecordPath, "2026-01-01", "recording_20260101_000003_ch2_48kHz.wav")
	os.MkdirAll(filepath.Dir(subFile), 0755)
	os.WriteFile(subFile, []byte("subfolder data"), 0644)
	t.Cleanup(func() { os.Remove(subFile) })

	req = httptest.NewRequest("GET", "/download/2026-01-01/recording_20260101_000003_ch2_48kHz.wav", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a subfolder recording by rel path, got %d", rec.Code)
	}
	if rec.Body.String() != "subfolder data" {
		t.Fatalf("unexpected subfolder download body: %q", rec.Body.String())
	}

	// Path traversal in the request is rejected outright, not sanitized. Go's
	// ServeMux cleans the path (redirecting `..` segments), and handleDownload
	// independently rejects any `..` in the value - either way it must never
	// serve the file.
	for _, evil := range []string{"../etc/passwd", "..", "2026-01-01/../../etc/passwd"} {
		req = httptest.NewRequest("GET", "/download/"+evil, nil)
		req.AddCookie(sessionCookie)
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("expected traversal %q to be rejected (non-200), got 200", evil)
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("expected traversal %q to never serve file content, got %q", evil, rec.Body.String())
		}
	}
}

func waitForPlaybackIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mutex.Lock()
		idle := currentState == StateIdle && playbackCmd == nil
		mutex.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("playback did not return to idle after stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPlaybackLifecycle(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	if err := os.MkdirAll(RecordPath, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", RecordPath, err)
	}
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatalf("write fake recording: %v", err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)

	mutex.Lock()
	state := currentState
	hasCmd := playbackCmd != nil
	mutex.Unlock()
	if state != StatePlaying || !hasCmd {
		t.Fatalf("expected StatePlaying with a running playbackCmd, got state=%v hasCmd=%v", state, hasCmd)
	}

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}

// Covers the mutual-exclusion guard the advisor flagged: no playback while
// recording, no recording while playing.
func TestPlaybackAndRecordingAreMutuallyExclusive(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	recFile := filepath.Join(RecordPath, "recording_20260101_000001_ch2_48kHz.wav")
	os.WriteFile(recFile, []byte("fake"), 0644)
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)
	mutex.Lock()
	playing := currentState == StatePlaying
	mutex.Unlock()
	if !playing {
		t.Fatalf("setup: expected playback to start")
	}

	// Record button must be a no-op while playing.
	onButtonPress(hardware.RecordButton)
	mutex.Lock()
	stillPlayingNotRecording := currentState == StatePlaying && !isRecording
	mutex.Unlock()
	if !stillPlayingNotRecording {
		t.Fatalf("record button started recording while playback was active")
	}

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)

	// Play button must be a no-op while recording (simulated directly, same
	// rationale as TestInfernoWorkerDoesNotKillActiveRecording).
	mutex.Lock()
	isRecording = true
	currentState = StateRecording
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)

	mutex.Lock()
	notPlaying := currentState == StateRecording && playbackCmd == nil
	isRecording = false
	currentState = StateIdle
	mutex.Unlock()
	if !notPlaying {
		t.Fatalf("play button started playback while recording was active")
	}
}

func TestIdleBrowseNavigatesPagesAndReturnsToIdle(t *testing.T) {
	origState, origChannels, origPage, origOwned := currentState, channelCount, idleBrowsePage, idleBrowseMonitorOwned
	t.Cleanup(func() {
		mutex.Lock()
		currentState, channelCount, idleBrowsePage, idleBrowseMonitorOwned = origState, origChannels, origPage, origOwned
		mutex.Unlock()
	})

	mutex.Lock()
	currentState = StateIdle
	channelCount = 8 // idleVUChannelsPerPage=6 -> 2 VU pages + 1 waveform page + 1 info page = 4 stops
	mutex.Unlock()

	// Any rotation from idle enters browse mode at page 0.
	onEncoderRotate(1)
	mutex.Lock()
	enteredRight := currentState == StateIdleBrowse && idleBrowsePage == 0
	mutex.Unlock()
	if !enteredRight {
		t.Fatalf("expected rotating from idle to enter StateIdleBrowse at page 0, got state=%v page=%d", currentState, idleBrowsePage)
	}

	// Rotating forward pages through the 2 VU pages, then the waveform
	// page, then the info page, then wraps back to page 0.
	for i, want := range []int{1, 2, 3, 0} {
		onEncoderRotate(1)
		mutex.Lock()
		got := idleBrowsePage
		mutex.Unlock()
		if got != want {
			t.Fatalf("rotation %d: expected page %d, got %d", i+1, want, got)
		}
	}

	// Rotating backward from page 0 wraps to the last (info) page.
	onEncoderRotate(-1)
	mutex.Lock()
	got := idleBrowsePage
	mutex.Unlock()
	if got != 3 {
		t.Fatalf("expected backward rotation from page 0 to wrap to page 3, got %d", got)
	}

	// A click exits back to idle.
	onEncoderClick()
	mutex.Lock()
	exited := currentState == StateIdle
	mutex.Unlock()
	if !exited {
		t.Fatalf("expected click to return to StateIdle from idle-browse")
	}
}

func TestIdleVUPct(t *testing.T) {
	cases := []struct {
		db   float64
		want float64
	}{
		{0, 100}, {-90, 0}, {-18, 58}, {-200, 0}, {10, 100},
	}
	for _, c := range cases {
		if got := idleVUPct(c.db); got != c.want {
			t.Errorf("idleVUPct(%v) = %v, want %v", c.db, got, c.want)
		}
	}
}

func TestAdjustVURangeAndPeakHoldWrap(t *testing.T) {
	origRange, origHold := vuRangeIdx, peakHoldIdx
	t.Cleanup(func() { vuRangeIdx, peakHoldIdx = origRange, origHold })

	vuRangeIdx = len(vuRangeOptions) - 1
	adjustVURange(1)
	if vuRangeIdx != 0 {
		t.Fatalf("expected vuRangeIdx to wrap to 0, got %d", vuRangeIdx)
	}
	adjustVURange(-1)
	if vuRangeIdx != len(vuRangeOptions)-1 {
		t.Fatalf("expected vuRangeIdx to wrap backward to %d, got %d", len(vuRangeOptions)-1, vuRangeIdx)
	}

	peakHoldIdx = len(peakHoldOptions) - 1
	adjustPeakHold(1)
	if peakHoldIdx != 0 {
		t.Fatalf("expected peakHoldIdx to wrap to 0, got %d", peakHoldIdx)
	}
}

func TestDecayPeakHoldHoldsThenFalls(t *testing.T) {
	origPeak, origHeld, origSetAt, origHoldIdx := meterChannelPeak, meterChannelPeakHeld, peakHeldSetAt, peakHoldIdx
	t.Cleanup(func() {
		mutex.Lock()
		meterChannelPeak, meterChannelPeakHeld, peakHeldSetAt, peakHoldIdx = origPeak, origHeld, origSetAt, origHoldIdx
		mutex.Unlock()
	})

	mutex.Lock()
	defer mutex.Unlock()

	peakHoldIdx = 1 // 500ms hold
	meterChannelPeak = []float64{-6}
	meterChannelPeakHeld = nil
	decayPeakHold() // first call seeds held from instantaneous
	if meterChannelPeakHeld[0] != -6 {
		t.Fatalf("expected initial held peak to seed at -6, got %v", meterChannelPeakHeld[0])
	}

	// Signal drops, but within the hold window the displayed peak must not
	// move yet - that's the whole point of a peak hold.
	meterChannelPeak[0] = -40
	decayPeakHold()
	if meterChannelPeakHeld[0] != -6 {
		t.Fatalf("expected held peak to stay at -6 within the hold window, got %v", meterChannelPeakHeld[0])
	}

	// Once the hold window has elapsed, it should be falling toward the
	// (lower) instantaneous value rather than staying pinned or jumping
	// straight down to it.
	peakHeldSetAt[0] = time.Now().Add(-time.Second)
	decayPeakHold()
	if meterChannelPeakHeld[0] >= -6 || meterChannelPeakHeld[0] < -40 {
		t.Fatalf("expected held peak to have decayed between -40 and -6, got %v", meterChannelPeakHeld[0])
	}
}

func TestLogLevelSetAndPersist(t *testing.T) {
	origLevel, origState, origMenu := xlog.GetLevel(), currentState, selectedMenu
	t.Cleanup(func() {
		xlog.SetLevel(origLevel)
		currentState, selectedMenu = origState, origMenu
	})

	mutex.Lock()
	applyLogLevel(LogDebug)
	mutex.Unlock()
	if xlog.GetLevel() != LogDebug {
		t.Fatalf("expected applyLogLevel to set Debug, got %d", xlog.GetLevel())
	}

	mutex.Lock()
	currentState = StateLogging
	selectedMenu = 2 // Info
	mutex.Unlock()

	onEncoderClick()
	if xlog.GetLevel() != LogInfo {
		t.Fatalf("expected click on Info row to set LogInfo, got %d", xlog.GetLevel())
	}

	mutex.Lock()
	enteredSettingsBack := currentState == StateLogging
	mutex.Unlock()
	if !enteredSettingsBack {
		t.Fatalf("expected level-selection click to stay on the Logging submenu, got state=%d", currentState)
	}

	// The Back row returns to Settings without changing the level.
	mutex.Lock()
	selectedMenu = 4
	mutex.Unlock()
	onEncoderClick()
	mutex.Lock()
	backToSettings := currentState == StateSettings && xlog.GetLevel() == LogInfo
	mutex.Unlock()
	if !backToSettings {
		t.Fatalf("expected Back to return to Settings keeping Info, got state=%d level=%d", currentState, xlog.GetLevel())
	}

	// A raised level round-trips through the persisted config.
	origCfg := os.Getenv("PI9696_CONFIG")
	tmpCfg := filepath.Join(t.TempDir(), "config.json")
	os.Setenv("PI9696_CONFIG", tmpCfg)
	t.Cleanup(func() { os.Setenv("PI9696_CONFIG", origCfg) })

	persistConfig()
	loadPersistedConfig()
	if xlog.GetLevel() != LogInfo {
		t.Fatalf("expected persisted log level Info to reload, got %d", xlog.GetLevel())
	}
}

func TestLogLevelSubmenuBackTarget(t *testing.T) {
	origState, origMenu := currentState, selectedMenu
	t.Cleanup(func() { currentState, selectedMenu = origState, origMenu })

	mutex.Lock()
	currentState = StateLogging
	selectedMenu = 4 // Back
	mutex.Unlock()
	onEncoderClick()

	mutex.Lock()
	got := currentState == StateSettings
	mutex.Unlock()
	if !got {
		t.Fatalf("expected Logging Back to go to Settings, got state=%d", currentState)
	}
}

func TestOledBrightnessAdjustAndPersist(t *testing.T) {
	origB, origAuto := oledBrightnessPct, autoDimEnabled
	origCfg := os.Getenv("PI9696_CONFIG")
	t.Cleanup(func() {
		oledBrightnessPct, autoDimEnabled = origB, origAuto
		os.Setenv("PI9696_CONFIG", origCfg)
	})

	// Clamping: going negative stops at 0, and going high stops at 100.
	oledBrightnessPct = 2
	adjustOledBrightness(-5)
	if oledBrightnessPct != 0 {
		t.Fatalf("expected brightness to clamp at 0, got %d", oledBrightnessPct)
	}
	adjustOledBrightness(500)
	if oledBrightnessPct != 100 {
		t.Fatalf("expected brightness to clamp at 100, got %d", oledBrightnessPct)
	}

	// A mid-scale value round-trips through the persisted config.
	oledBrightnessPct = 37
	tmpCfg := filepath.Join(t.TempDir(), "config.json")
	os.Setenv("PI9696_CONFIG", tmpCfg)

	persistConfig()
	oledBrightnessPct = 0
	loadPersistedConfig()
	if oledBrightnessPct != 37 {
		t.Fatalf("expected persisted brightness 37 to reload, got %d", oledBrightnessPct)
	}
}

func TestDisplaySubmenuBackTarget(t *testing.T) {
	origState, origMenu := currentState, selectedMenu
	t.Cleanup(func() { currentState, selectedMenu = origState, origMenu })

	mutex.Lock()
	currentState = StateDisplay
	selectedMenu = 2 // Back
	mutex.Unlock()
	onEncoderClick()

	mutex.Lock()
	got := currentState == StateSettings
	mutex.Unlock()
	if !got {
		t.Fatalf("expected Display Back to go to Settings, got state=%d", currentState)
	}
}

func TestAutoDimStateTransitions(t *testing.T) {
	origB, origAuto, origLast, origDim := oledBrightnessPct, autoDimEnabled, lastInputTime, displayDimState
	origRec, origPlay := isRecording, playbackCmd
	t.Cleanup(func() {
		oledBrightnessPct, autoDimEnabled, lastInputTime, displayDimState = origB, origAuto, origLast, origDim
		isRecording, playbackCmd = origRec, origPlay
	})

	mutex.Lock()
	oledBrightnessPct = 80
	autoDimEnabled = true
	displayDimState = -1                             // force an apply on the first call
	lastInputTime = time.Now().Add(-5 * time.Minute) // long idle
	mutex.Unlock()

	// Long-idle clocks through dim (20%) then off (0) as time advances.
	applyAutoDimLocked(time.Now())
	if displayDimState != 2 || oledBrightnessPct != 80 {
		t.Fatalf("expected long-idle to go fully off (state 2), got state=%d b=%d", displayDimState, oledBrightnessPct)
	}
	// Note: applyAutoDimLocked changes only the display value via the manager,
	// which is nil in tests, so the user brightness (oledBrightnessPct) is a
	// separate readiness check above and the actual transitions are asserted
	// through displayDimState below.

	mutex.Lock()
	displayDimState = -1
	lastInputTime = time.Now().Add(-dimTimeout - 10*time.Second) // past dim, before off
	mutex.Unlock()
	applyAutoDimLocked(time.Now())
	if displayDimState != 1 {
		t.Fatalf("expected dim-threshold idle to dim (state 1), got state=%d", displayDimState)
	}

	// Just-active: full brightness, and auto-dim disabled stays put too.
	mutex.Lock()
	displayDimState = -1
	lastInputTime = time.Now()
	autoDimEnabled = false
	mutex.Unlock()
	applyAutoDimLocked(time.Now())
	if displayDimState != 0 {
		t.Fatalf("expected active/disabled to be full brightness (state 0), got state=%d", displayDimState)
	}

	// Auto-dim disabled with a stale idle still must not dim or off.
	mutex.Lock()
	displayDimState = -1
	lastInputTime = time.Now().Add(-10 * time.Minute)
	mutex.Unlock()
	applyAutoDimLocked(time.Now())
	if displayDimState != 0 {
		t.Fatalf("expected auto-dim disabled to ignore idle (state 0), got state=%d", displayDimState)
	}

	// An active take or playback must hold the panel at full brightness even
	// after a long idle - the OLED is the operator's live status surface.
	mutex.Lock()
	displayDimState = -1
	autoDimEnabled = true
	lastInputTime = time.Now().Add(-10 * time.Minute)
	isRecording = true
	playbackCmd = nil
	mutex.Unlock()
	applyAutoDimLocked(time.Now())
	if displayDimState != 0 {
		t.Fatalf("expected active recording to stay at full brightness (state 0), got state=%d", displayDimState)
	}

	mutex.Lock()
	isRecording = false
	playbackCmd = &exec.Cmd{} // non-nil = playback running
	displayDimState = -1
	mutex.Unlock()
	applyAutoDimLocked(time.Now())
	if displayDimState != 0 {
		t.Fatalf("expected active playback to stay at full brightness (state 0), got state=%d", displayDimState)
	}
}

func TestIsValidFilePrefix(t *testing.T) {
	valid := []string{"Live", "My Show", "ABC-1", "a", "recording"}
	for _, s := range valid {
		if !isValidFilePrefix(s) {
			t.Errorf("expected %q to be a valid prefix", s)
		}
	}
	invalid := []string{"", "has_underscore", "toolong01234567890123456789012345", "bad/char", "spaces ", " leading"}
	for _, s := range invalid {
		if isValidFilePrefix(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestAdjustRecordPrefixCycles(t *testing.T) {
	orig := filePrefix
	t.Cleanup(func() { filePrefix = orig })

	// Empty -> step up -> first non-empty preset.
	filePrefix = ""
	adjustRecordPrefix(1)
	if filePrefix != "Live" {
		t.Fatalf("expected first preset after stepping up from default, got %q", filePrefix)
	}

	// Stepping up again moves to the next preset.
	adjustRecordPrefix(1)
	if filePrefix != "Rehearsal" {
		t.Fatalf("expected Rehearsal after another step, got %q", filePrefix)
	}

	// Stepping down wraps back.
	adjustRecordPrefix(-1)
	if filePrefix != "Live" {
		t.Fatalf("expected wrap back to Live, got %q", filePrefix)
	}
}

func TestEffectiveFilePrefix(t *testing.T) {
	orig := filePrefix
	t.Cleanup(func() { filePrefix = orig })

	filePrefix = ""
	if got := effectiveFilePrefix(); got != defaultFilePrefix {
		t.Fatalf("expected empty prefix to use default %q, got %q", defaultFilePrefix, got)
	}
	filePrefix = "StudioA"
	if got := effectiveFilePrefix(); got != "StudioA" {
		t.Fatalf("expected custom prefix, got %q", got)
	}
}

func TestFilePrefixPersists(t *testing.T) {
	origPrefix := filePrefix
	origCfg := os.Getenv("PI9696_CONFIG")
	t.Cleanup(func() { filePrefix = origPrefix; os.Setenv("PI9696_CONFIG", origCfg) })

	tmpCfg := filepath.Join(t.TempDir(), "config.json")
	os.Setenv("PI9696_CONFIG", tmpCfg)

	filePrefix = "VenueB"
	persistConfig()
	filePrefix = ""
	loadPersistedConfig()
	if filePrefix != "VenueB" {
		t.Fatalf("expected persisted prefix VenueB to reload, got %q", filePrefix)
	}
}

func TestRecordingFilenameMatchesRegexWithPrefix(t *testing.T) {
	// The name parser must read a recording back whether it used the default
	// prefix or a custom one.
	cases := []struct {
		name       string
		wantCh     int
		wantRate   int
		wantFormat string
	}{
		{"recording_20240131_143022_ch2_48kHz.wav", 2, 48, "WAV"},
		{"Live_20240131_143022_ch8_96kHz.wav", 8, 96, "WAV"},
		{"My Show_20240131_143022_ch1_192kHz.wav", 1, 192, "WAV"},
	}
	for _, c := range cases {
		row := buildRecordingRow(c.name)
		if row.Channels != c.wantCh || row.SampleRate != c.wantRate || row.Format != c.wantFormat {
			t.Errorf("buildRecordingRow(%q) got ch=%d rate=%d fmt=%s, want ch=%d rate=%d fmt=%s",
				c.name, row.Channels, row.SampleRate, row.Format, c.wantCh, c.wantRate, c.wantFormat)
		}
	}

	// A non-matching file (e.g. from a third-party recorder) must not crash
	// and should produce a bare row.
	row := buildRecordingRow("other.wav")
	if row.Name != "other.wav" || row.Format != "" || row.Channels != 0 {
		t.Errorf("expected a bare row for non-matching name, got %+v", row)
	}
}

func TestRecordingDurationUsesHzNotKHz(t *testing.T) {
	// Regression: recordingDuration must be fed the actual sample rate in Hz.
	// A 1-second 48kHz stereo 24-bit WAV is 44-byte header + 48000*2*3 data
	// bytes; if the kHz value (48) were used as the rate the reported
	// duration would be 1000x too long.
	dir := t.TempDir()
	p := filepath.Join(dir, "recording_20240131_143022_ch2_48kHz.wav")
	dataBytes := 48000 * 2 * 3
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 44+dataBytes)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := recordingDuration(p, 2, 48000)
	if got < 950*time.Millisecond || got > 1050*time.Millisecond {
		t.Fatalf("expected ~1s duration, got %v", got)
	}

	// The kHz value (48) must yield ~1000x the true duration - the bug that
	// used to be shipped - so the fix is pinned to the correct unit.
	if big := recordingDuration(p, 2, 48); big < 15*time.Minute {
		t.Fatalf("kHz value should give a 1000x-inflated duration for the test to be meaningful, got %v", big)
	}
}
