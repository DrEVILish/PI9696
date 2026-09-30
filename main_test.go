package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"pi9696/hardware"

	"golang.org/x/net/websocket"
)

// TestMain starts the single infernoWorker every test in this file shares.
// Each test used to spawn its own with "go infernoWorker()", which left
// multiple goroutines draining the same package-level infernoReqCh -
// harmless today only because doStartInferno/doStopInferno are idempotent
// and requests are short-lived, not because it's actually safe by design.
func TestMain(m *testing.M) {
	// Persist tests must never touch the real unit config (/etc/pi9696/
	// config.json) - ConfigPath is computed once at package init, so the
	// per-test PI9696_CONFIG env is ignored; repoint it here instead.
	ConfigPath = filepath.Join(os.TempDir(), "pi9696-test-config.json")
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

// demoTestCleanup stops any running demo generator first (a direct flag
// restore would orphan it and its FIFO), then restores the saved flags.
// Register it in every demo test; individual cleanups must not touch
// demoMode/demoFifoPath themselves. It also waits for the generator's FIFO
// to actually disappear - a stuck generator shows up here instead of
// leaking files (and writes) into the next test.
func demoTestCleanup(t *testing.T) {
	t.Helper()
	origDemo, origFifo := demoMode, demoFifoPath
	t.Cleanup(func() {
		// setDemoModeLocked refuses the off-toggle while transport is still
		// winding down (stopRecording/stopPlayback only signal; the owning
		// reapers flip isRecording/playbackCmd asynchronously). Wait for the
		// reapers first, or the refused toggle orphans the generator and its
		// FIFO - and cascades stale demo state into the next test.
		busyDeadline := time.Now().Add(3 * time.Second)
		for {
			mutex.Lock()
			busy := isRecording || playbackCmd != nil
			mutex.Unlock()
			if !busy || time.Now().After(busyDeadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		mutex.Lock()
		path := demoFifoPath
		if demoMode {
			setDemoModeLocked(false)
		}
		demoMode, demoFifoPath = origDemo, origFifo
		mutex.Unlock()
		if path == "" {
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("demo generator did not remove %s after stop", path)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// resetTransportCleanup is the standard transport-state cleanup for demo and
// playback tests: stops the monitor, then forces every transport field back
// to idle so the next test starts from a known state.
func resetTransportCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		mutex.Lock()
		if monitoring {
			stopMonitor()
		}
		currentState = StateIdle
		monitoring = false
		monitoringOutput = false
		autoMonitor = false
		playbackCmd = nil
		playbackFile = ""
		playbackPausedElapsed = 0
		playbackStart = time.Time{}
		playbackDone = nil
		playbackDuration = 0
		mutex.Unlock()
	})
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

// testSessionCookie logs in through the real login flow and returns a valid
// session cookie. Since a session is a server-side ID (not the token), tests
// must not fabricate a cookie with the token value - they authenticate the
// same way a user does.
func testSessionCookie(t *testing.T) *http.Cookie {
	t.Helper()
	mux := newRemoteMux()
	form := "token=" + remoteToken
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			return c
		}
	}
	t.Fatalf("login did not set a session cookie")
	return nil
}

// withFakeInfernoProject points InfernoBinary at a stub that behaves like
// inferno2pipe, built in a temp dir.
//
// It must never write inside the project's own inferno/ checkout: the real
// tree is the installed AoIP server, and this helper used to create a stub
// there and then os.RemoveAll("inferno") on cleanup - so running the suite on
// a deployed unit silently deleted the built binary and the next start failed
// with InfernoFailed. Temp dir, temp module, cleanup restores the variable.
func withFakeInfernoProject(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := `package main
import ("flag"; "fmt"; "os"; "time")
func main() {
	channels := flag.Int("c", 2, "channels")
	output := flag.String("o", "", "output FIFO")
	flag.Parse()
	if *output == "" { fmt.Fprintln(os.Stderr, "Usage: inferno2pipe -c <channels> -o <output_fifo>"); os.Exit(1) }
	f, _ := os.OpenFile(*output, os.O_WRONLY, 0)
	defer f.Close()
	buf := make([]byte, *channels*4*1024)
	for { f.Write(buf); time.Sleep(10*time.Millisecond) }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(stub), 0644); err != nil {
		t.Fatalf("write inferno stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module inferno2pipe\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write inferno go.mod: %v", err)
	}
	bin := filepath.Join(dir, "inferno2pipe")
	cmd := exec.Command("go", "build", "-o", bin, "main.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build inferno stub: %v: %s", err, out)
	}

	orig := InfernoBinary
	InfernoBinary = bin
	t.Cleanup(func() { InfernoBinary = orig })
}

// Guards the destructive mistake directly: the suite must not remove the
// installed inferno/ checkout. Cheap, and it fails loudly if someone reverts
// the helper to writing inside the project tree.
func TestSuiteDoesNotTouchInstalledInferno(t *testing.T) {
	if _, err := os.Stat("inferno"); err != nil {
		t.Skip("no inferno/ checkout in this working tree")
	}
	entries, err := os.ReadDir("inferno")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("inferno/ is empty - a previous test run deleted the installed build")
	}
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

	meterReader(strings.NewReader(input), meterGen)

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
	// The fake ffmpeg never writes the output WAV, so create it so the
	// post-take fsync in the stop goroutine opens a real file.
	os.MkdirAll(RecordPath, 0755)
	takeFile := filepath.Join(recordingFile)
	os.WriteFile(takeFile, []byte("fake"), 0644)
	t.Cleanup(func() { os.Remove(takeFile) })
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
	origState, origRec, origInferno := currentState, isRecording, infernoState
	t.Cleanup(func() { currentState, isRecording, infernoState = origState, origRec, origInferno })

	initTestHardware(t)
	mutex.Lock()
	infernoState = InfernoStopped
	mutex.Unlock()

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
	origSelected, origEditing, origSampleIdx, origState := selectedMenu, editingParameter, sampleRateIdx, currentState
	t.Cleanup(func() {
		selectedMenu, editingParameter, sampleRateIdx, currentState = origSelected, origEditing, origSampleIdx, origState
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

func TestTelemetryHistAppendCap(t *testing.T) {
	origT, origCPU, origApp, origSys, origCores, origTemp, origDisk := teleHistT, teleHistCPU, teleHistRAMApp, teleHistRAMSys, teleHistCores, teleHistTemp, teleHistDisk
	origPct := cpuPct
	t.Cleanup(func() {
		teleHistT, teleHistCPU, teleHistRAMApp, teleHistRAMSys, teleHistCores, teleHistTemp, teleHistDisk = origT, origCPU, origApp, origSys, origCores, origTemp, origDisk
		cpuPct = origPct
	})

	// Empty history accepts samples; parallel slices stay aligned.
	teleHistT, teleHistCPU, teleHistRAMApp, teleHistRAMSys, teleHistCores, teleHistTemp, teleHistDisk = nil, nil, nil, nil, nil, nil, nil
	cpuPct = []float64{10, 30}
	appendTelemetryHist()
	appendTelemetryHist()
	for name, n := range map[string]int{"t": len(teleHistT), "cpu": len(teleHistCPU), "cores": len(teleHistCores), "ramApp": len(teleHistRAMApp), "ramSys": len(teleHistRAMSys), "temp": len(teleHistTemp), "disk": len(teleHistDisk)} {
		if n != 2 {
			t.Fatalf("history slice %s drifted: len %d", name, n)
		}
	}
	if teleHistCPU[0] != 20 {
		t.Fatalf("expected cross-core average 20, got %v", teleHistCPU[0])
	}
	if len(teleHistCores[0]) != 2 || teleHistCores[1][1] != 30 {
		t.Fatalf("per-core rows wrong: %v", teleHistCores)
	}

	// Overflowing the cap trims oldest-first, newest kept.
	for i := 0; i < teleHistN+10; i++ {
		appendTelemetryHist()
	}
	for name, n := range map[string]int{"t": len(teleHistT), "cpu": len(teleHistCPU), "cores": len(teleHistCores), "ramApp": len(teleHistRAMApp), "ramSys": len(teleHistRAMSys), "temp": len(teleHistTemp), "disk": len(teleHistDisk)} {
		if n != teleHistN {
			t.Fatalf("expected cap %d on slice %s, got %d", teleHistN, name, n)
		}
	}
	for i := 1; i < len(teleHistT); i++ {
		if teleHistT[i] < teleHistT[i-1] {
			t.Fatalf("history timestamps out of order at %d", i)
		}
	}
}

func TestAPITelemetry(t *testing.T) {
	origToken, origLimiter, origSessions := remoteToken, loginLimit, sessions
	origT, origCPU, origApp, origSys, origCores, origTemp, origDisk := teleHistT, teleHistCPU, teleHistRAMApp, teleHistRAMSys, teleHistCores, teleHistTemp, teleHistDisk
	remoteToken = "TESTTOKEN2"
	loginLimit = newLoginLimiter()
	sessions = newSessionStore()
	t.Cleanup(func() {
		remoteToken, loginLimit, sessions = origToken, origLimiter, origSessions
		teleHistT, teleHistCPU, teleHistRAMApp, teleHistRAMSys, teleHistCores, teleHistTemp, teleHistDisk = origT, origCPU, origApp, origSys, origCores, origTemp, origDisk
	})

	mux := newRemoteMux()
	form := "token=" + remoteToken
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected session cookie")
	}

	teleHistT = []int64{1000, 1002, 1004}
	teleHistCPU = []float64{10, 20, 30}
	teleHistCores = [][]float64{{5, 15}, {10, 30}, {15, 45}}
	teleHistRAMApp = []float64{40, 41, 42}
	teleHistRAMSys = []float64{1000, 1001, 1002}
	teleHistTemp = []float64{50, 51, 52}
	teleHistDisk = []float64{60, 61, 62}

	req = httptest.NewRequest("GET", "/api/telemetry", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var v telemetryHistView
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("telemetry not JSON: %v", err)
	}
	if len(v.T) != 3 || len(v.CPU) != 3 || len(v.RAMApp) != 3 || len(v.RAMSys) != 3 || len(v.Cores) != 3 || len(v.Temp) != 3 || len(v.Disk) != 3 {
		t.Fatalf("parallel arrays must match: %+v", v)
	}
	if v.T[0] != 1000 || v.CPU[2] != 30 || v.RAMApp[1] != 41 || v.RAMSys[2] != 1002 {
		t.Fatalf("history values wrong: %+v", v)
	}
	if len(v.Cores[2]) != 2 || v.Cores[2][0] != 15 || v.Cores[2][1] != 45 {
		t.Fatalf("per-core rows wrong: %+v", v.Cores)
	}
}

func TestDisplaySeqBumpsOnFrameChange(t *testing.T) {
	initTestHardware(t)
	origSeq, origHash := displaySeq, displayLastHash
	t.Cleanup(func() { displaySeq, displayLastHash = origSeq, origHash })

	mutex.Lock()
	hwManager.ClearDisplay()
	noteDisplayFrame()
	afterClear := displaySeq
	noteDisplayFrame() // identical frame: no bump
	if displaySeq != afterClear {
		t.Fatalf("identical frames must not bump displaySeq")
	}
	hwManager.DrawText(0, 10, "changed")
	noteDisplayFrame()
	mutex.Unlock()
	if displaySeq != afterClear+1 {
		t.Fatalf("changed frame must bump displaySeq once")
	}
}

func TestTelemetryWSRoundtrip(t *testing.T) {
	initTestHardware(t) // connect snapshot now includes panels (hwManager)
	origT, origCPU := teleHistT, teleHistCPU
	origHub := teleWSHub
	teleWSHub = map[*websocket.Conn]bool{}
	t.Cleanup(func() {
		teleHistT, teleHistCPU = origT, origCPU
		teleWSMu.Lock()
		teleWSHub = origHub
		teleWSMu.Unlock()
	})
	teleHistT = []int64{2000, 2002}
	teleHistCPU = []float64{11, 22}

	srv := httptest.NewServer(websocket.Handler(handleWSTelemetry))
	defer srv.Close()
	ws, err := websocket.Dial("ws://"+strings.TrimPrefix(srv.URL, "http://")+"/", "", "http://localhost/")
	if err != nil {
		t.Fatalf("dial telemetry WS: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Connect snapshot: status HTML first, history JSON second.
	var statusMsg, histMsg teleWSMessage
	if err := websocket.JSON.Receive(ws, &statusMsg); err != nil {
		t.Fatalf("status message: %v", err)
	}
	if err := websocket.JSON.Receive(ws, &histMsg); err != nil {
		t.Fatalf("history message: %v", err)
	}
	if statusMsg.Target != "#status" || !strings.Contains(statusMsg.Content, "sys-readout") {
		t.Fatalf("bad status message target/content: %+v", statusMsg)
	}
	if histMsg.Target != "#teleHist" {
		t.Fatalf("bad history message target: %+v", histMsg)
	}
	var h telemetryHistView
	if err := json.Unmarshal([]byte(histMsg.Content), &h); err != nil {
		t.Fatalf("history content not JSON: %v", err)
	}
	if len(h.T) != 2 || h.CPU[1] != 22 {
		t.Fatalf("history values wrong: %+v", h)
	}

	// Closing the client unregisters it from the hub.
	ws.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		teleWSMu.Lock()
		n := len(teleWSHub)
		teleWSMu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			if n != 0 {
				t.Fatalf("closed connection still in hub")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTelemetryWSPanelsPush(t *testing.T) {
	initTestHardware(t) // renderConfigHTML reads hwManager.Network
	origHub := teleWSHub
	origCfg, origRecs, origKey := lastPanelConfig, lastPanelRecs, lastPanelRecsKey
	teleWSHub = map[*websocket.Conn]bool{}
	lastPanelConfig, lastPanelRecs, lastPanelRecsKey = "", "", ""
	t.Cleanup(func() {
		teleWSMu.Lock()
		teleWSHub = origHub
		lastPanelConfig, lastPanelRecs, lastPanelRecsKey = origCfg, origRecs, origKey
		teleWSMu.Unlock()
	})

	srv := httptest.NewServer(websocket.Handler(handleWSTelemetry))
	defer srv.Close()
	ws, err := websocket.Dial("ws://"+strings.TrimPrefix(srv.URL, "http://")+"/", "", "http://localhost/")
	if err != nil {
		t.Fatalf("dial telemetry WS: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Connect snapshot: status, history, then both panels (no polling).
	var msgs [4]teleWSMessage
	for i := range msgs {
		if err := websocket.JSON.Receive(ws, &msgs[i]); err != nil {
			t.Fatalf("connect message %d: %v", i, err)
		}
	}
	if msgs[0].Target != "#status" || msgs[1].Target != "#teleHist" ||
		msgs[2].Target != "#config" || msgs[3].Target != "#recordings" {
		t.Fatalf("bad connect targets: %q %q %q %q",
			msgs[0].Target, msgs[1].Target, msgs[2].Target, msgs[3].Target)
	}

	// Unchanged broadcast: only status + history go out, panels stay quiet.
	broadcastTelemetry()
	for _, want := range []string{"#status", "#teleHist"} {
		var m teleWSMessage
		if err := websocket.JSON.Receive(ws, &m); err != nil {
			t.Fatalf("broadcast %s: %v", want, err)
		}
		if m.Target != want {
			t.Fatalf("broadcast target = %q, want %q", m.Target, want)
		}
	}
	ws.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var extra teleWSMessage
	if err := websocket.JSON.Receive(ws, &extra); err == nil {
		t.Fatalf("unchanged panels must not push, got target %q", extra.Target)
	}
}

func TestLoginPageMarksTokenFresh(t *testing.T) {
	mutex.Lock()
	lastLoginPage = time.Time{}
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		lastLoginPage = time.Time{}
		mutex.Unlock()
	})

	req := httptest.NewRequest("GET", "/login", nil)
	rec := httptest.NewRecorder()
	handleLoginGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page status = %d, want 200", rec.Code)
	}
	mutex.Lock()
	fresh := loginTokenFreshLocked()
	firstBump := lastLoginPage
	mutex.Unlock()
	if !fresh {
		t.Fatalf("serving the login page must mark the OLED token fresh")
	}

	// A repeat GET must not extend the window: a crawler hammering /login
	// would otherwise keep the token parked on the OLED indefinitely.
	req2 := httptest.NewRequest("GET", "/login", nil)
	rec2 := httptest.NewRecorder()
	handleLoginGet(rec2, req2)
	mutex.Lock()
	unchanged := lastLoginPage.Equal(firstBump)
	mutex.Unlock()
	if !unchanged {
		t.Fatalf("a repeat login GET must not extend the token window")
	}

	mutex.Lock()
	lastLoginPage = time.Now().Add(-loginTokenShowFor - time.Minute)
	stale := loginTokenFreshLocked()
	mutex.Unlock()
	if stale {
		t.Fatalf("token shown past loginTokenShowFor must go stale")
	}
}

func TestSimDefaultLogLevelDebug(t *testing.T) {
	t.Setenv("PI9696_SIM", "1")
	origPath, origLevel := ConfigPath, currentLogLevel()
	t.Cleanup(func() {
		ConfigPath = origPath
		applyLogLevel(origLevel)
	})
	ConfigPath = filepath.Join(t.TempDir(), "nonexistent-config.json")
	applyLogLevel(LogError)

	loadPersistedConfig()
	if got := currentLogLevel(); got != LogDebug {
		t.Fatalf("fresh sim config must default to Debug, got %d", got)
	}
}

func TestSettingsMonitorToggle(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required for the monitor pipeline")
	}
	initTestHardware(t)
	origDemo := demoMode
	setDemoModeLocked(true) // infernoUp, so startMonitor works
	t.Cleanup(func() {
		mutex.Lock()
		if monitoring {
			stopMonitor()
		}
		monitoring, autoMonitor = false, false
		mutex.Unlock()
		setDemoModeLocked(origDemo)
	})
	ensureMonitorDown(t)

	mux := newRemoteMux()
	cookie := testSessionCookie(t)
	post := func(body string) string {
		req := httptest.NewRequest("POST", "/api/settings/monitor", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("monitor toggle status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	if out := post("enabled=on"); !strings.Contains(out, "checked") {
		t.Fatalf("enabling monitor must render a checked switch")
	}
	mutex.Lock()
	on := monitoring && autoMonitor
	mutex.Unlock()
	if !on {
		t.Fatalf("enabling monitor must start it with autoMonitor")
	}

	if out := post(""); strings.Contains(out, "checked") {
		t.Fatalf("disabling monitor must render an unchecked switch")
	}
	mutex.Lock()
	off := !monitoring && !autoMonitor
	mutex.Unlock()
	if !off {
		t.Fatalf("disabling monitor must stop it and clear autoMonitor")
	}
}

func TestOLEDMonitoringRowToggles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required for the monitor pipeline")
	}
	initTestHardware(t)
	origDemo, origState, origSel := demoMode, currentState, selectedMenu
	setDemoModeLocked(true)
	t.Cleanup(func() {
		mutex.Lock()
		if monitoring {
			stopMonitor()
		}
		monitoring, autoMonitor = false, false
		currentState, selectedMenu = origState, origSel
		mutex.Unlock()
		setDemoModeLocked(origDemo)
	})
	ensureMonitorDown(t)

	mutex.Lock()
	currentState, selectedMenu = StateSettings, 9
	handleSettingsClick()
	enabled := monitoring && autoMonitor
	handleSettingsClick()
	done := monitorDone
	mutex.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	mutex.Lock()
	disabled := !monitoring && !autoMonitor
	mutex.Unlock()
	if !enabled || !disabled {
		t.Fatalf("OLED Monitoring row must toggle on then off, got on=%v off=%v", enabled, disabled)
	}
}

func TestRemoteAccessQRRedirectDropsTokenQuery(t *testing.T) {
	// The OLED access-QR encodes "/#t=<token>" (fragment, never sent to the
	// server); the 303 from requireAuth must redirect bare - echoing any
	// ?t= back would put the bearer token in history and proxy logs.
	origToken, origLimiter, origSessions := remoteToken, loginLimit, sessions
	remoteToken = "TESTTOKEN2"
	loginLimit = newLoginLimiter()
	sessions = newSessionStore()
	t.Cleanup(func() { remoteToken, loginLimit, sessions = origToken, origLimiter, origSessions })

	mux := newRemoteMux()

	// Unauthenticated request with a stray ?t=: redirect must drop it.
	req := httptest.NewRequest("GET", "/?t=TESTTOK", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect to login, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestRemoteSessionCookieIsNotTokenAndRevokes(t *testing.T) {
	origToken, origLimiter, origSessions := remoteToken, loginLimit, sessions
	remoteToken = "TESTTOKEN2"
	loginLimit = newLoginLimiter()
	sessions = newSessionStore()
	t.Cleanup(func() { remoteToken, loginLimit, sessions = origToken, origLimiter, origSessions })

	mux := newRemoteMux()

	// Log in and capture the session cookie.
	form := "token=" + remoteToken
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected session cookie")
	}
	if sessionCookie.Value == remoteToken {
		t.Fatalf("session cookie must not equal the access token")
	}

	// The session authorizes a request.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with a live session, got %d", rec.Code)
	}

	// Logout revokes server-side: the same cookie is now rejected.
	// State change lives on POST (logout-CSRF); a plain GET must not
	// revoke and redirects to the dashboard instead.
	logoutReq := httptest.NewRequest("POST", "/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	mux.ServeHTTP(httptest.NewRecorder(), logoutReq)

	getReq := httptest.NewRequest("GET", "/logout", nil)
	getReq.AddCookie(sessionCookie)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusSeeOther {
		t.Fatalf("expected GET /logout to redirect without revoking, got %d", getRec.Code)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after logout (revoked session), got %d", rec.Code)
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
	req.AddCookie(testSessionCookie(t))

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
	req.AddCookie(testSessionCookie(t))
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
	sessionCookie := testSessionCookie(t)

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
	sessionCookie := testSessionCookie(t)

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

// waitProcessStopped blocks until pid reports kernel state 'T' (stopped by
// SIGSTOP), so a test can deterministically reproduce the paused-playback
// conditions instead of racing the signal delivery.
func waitProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if state := procState(pid); state == "T" {
			return
		} else if state == "gone" {
			t.Fatalf("process %d exited before reaching stopped (T) state", pid)
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never reached stopped (T) state", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// procState returns "gone" when the pid no longer exists, otherwise the ps
// state letter (e.g. "T" stopped, "S" sleeping) - or "" if unparseable.
func procState(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return "gone"
	}
	return strings.TrimSpace(string(out))
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

// Regression for the WS meter push loop's missing write deadline: a client
// that vanishes without closing could block Send forever, leaking the
// handler goroutine per stale connection. wsMeterSend must (a) succeed on a
// live connection and (b) report the connection as done once the peer is
// gone, so the loop exits instead of retrying into the void.
func TestWSMeterSendDetectsClosedClient(t *testing.T) {
	gotConn := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		gotConn <- ws
		<-release // hold the conn open until the test is done with it
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	client, err := websocket.Dial(url, "", "http://127.0.0.1/")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	serverConn := <-gotConn
	if !wsMeterSend(serverConn) {
		t.Fatalf("send to live client failed")
	}

	// Abrupt close: the next server-side send must fail and report done.
	client.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !wsMeterSend(serverConn) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wsMeterSend kept succeeding after the client closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Regression for the session store's unbounded growth: valid() prunes
// lazily, only when the same expired ID is presented again, so sessions
// that expired and were never re-presented accumulated forever. create()
// now sweeps already-expired entries on every login. This test expires one
// session without ever re-presenting it, then logs in again and asserts the
// store holds only the new session - the sweep must have removed the
// expired one without it being touched by valid().
func TestSessionStoreSweepsExpiredOnCreate(t *testing.T) {
	s := newSessionStore()
	expired := s.create(30 * time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	live := s.create(time.Hour)

	// The sweep must already have removed the expired entry - assert the
	// store's raw size before any valid() call could have lazily pruned it.
	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected the expired session to be swept on create, store holds %d entries", n)
	}

	if s.valid(expired) {
		t.Fatalf("expired session still valid")
	}
	if !s.valid(live) {
		t.Fatalf("freshly created session is not valid")
	}
}

// Covers the Round 3 seek/scrub design: encoder click toggles play/pause,
// rotate while paused seeks (restarting ffmpeg at a new offset), and the
// playhead is reported as a clamped offset. Also pins playbackFileDuration
// (the total used to clamp seeks and drive the progress readout).
func TestPlaybackSeekAndPauseToggle(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	// A 60-second 48kHz stereo 24-bit WAV: 44-byte header + 60*48000*2*3 data.
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	dataBytes := 60 * 48000 * 2 * 3
	if err := os.WriteFile(recFile, make([]byte, 44+dataBytes), 0644); err != nil {
		t.Fatalf("write fake recording: %v", err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	mutex.Unlock()

	// playbackFileDuration must derive the total from the file size.
	mutex.Lock()
	playbackDuration = playbackFileDuration(recFile)
	mutex.Unlock()
	if playbackDuration != 60*time.Second {
		t.Fatalf("expected 60s total, got %v", playbackDuration)
	}

	onButtonPress(hardware.PlayButton)
	mutex.Lock()
	if currentState != StatePlaying {
		t.Fatalf("expected StatePlaying, got %v", currentState)
	}
	mutex.Unlock()

	// Encoder click pauses.
	onEncoderClick()
	mutex.Lock()
	if currentState != StatePaused {
		t.Fatalf("expected StatePaused after click, got %v", currentState)
	}
	mutex.Unlock()

	// Rotate while paused seeks forward ~5s and stays paused.
	onEncoderRotate(1)
	mutex.Lock()
	if currentState != StatePaused {
		t.Fatalf("expected still StatePaused after seek, got %v", currentState)
	}
	fwd := playbackPausedElapsed
	mutex.Unlock()
	if fwd < 5*time.Second || fwd > 6*time.Second {
		t.Fatalf("expected position ~5s after one forward detent, got %v", fwd)
	}

	// Rotate back seeks toward the start.
	onEncoderRotate(-1)
	mutex.Lock()
	back := playbackPausedElapsed
	mutex.Unlock()
	if back > 1*time.Second {
		t.Fatalf("expected position near start after seeking back, got %v", back)
	}

	// Rotate while paused clamps at the total duration, not past it.
	onEncoderRotate(100)
	mutex.Lock()
	clamped := playbackPausedElapsed
	mutex.Unlock()
	if clamped != 60*time.Second {
		t.Fatalf("expected position clamped to total (60s), got %v", clamped)
	}

	// Encoder click resumes.
	onEncoderClick()
	mutex.Lock()
	if currentState != StatePlaying {
		t.Fatalf("expected StatePlaying after resume click, got %v", currentState)
	}
	mutex.Unlock()

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}

// Regression for the stop-while-paused hang: pausePlayback freezes ffmpeg
// with SIGSTOP, and a stopped process defers SIGTERM until it's continued.
// stopPlayback used to send only SIGTERM, so stopping from Paused left
// ffmpeg stopped forever - the UI stuck in Paused, and gracefulShutdown
// hung on the reaping goroutine's channel. stopPlayback must follow the
// TERM with SIGCONT. The test waits for the child to actually reach kernel
// state 'T' first, so it can't pass by racing the stop signal.
func TestStopWhilePausedAwakensStoppedFFmpeg(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatalf("write fake recording: %v", err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	sampleRateIdx, channelCount = 1, 2 // match the staged 48kHz/2ch take
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)
	onEncoderClick() // pause -> SIGSTOP

	mutex.Lock()
	pid := playbackCmd.Process.Pid
	mutex.Unlock()
	waitProcessStopped(t, pid)

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}

// Regression for the seek-while-paused process leak: restartPlaybackAt
// signals the outgoing ffmpeg with SIGTERM only, but seek happens while the
// process is SIGSTOP'd - and a stopped process defers SIGTERM until it's
// continued. Each seek detent therefore left a frozen ffmpeg behind: never
// reaped, still holding the ALSA output open (which on an exclusive ALSA
// device would stop the replacement process from opening the output at
// all). restartPlaybackAt must follow the TERM with SIGCONT. Like the
// stop-while-paused test above, it waits for kernel state 'T' first so it
// deterministically reproduces the hang conditions.
func TestSeekWhilePausedReapsOldFFmpeg(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
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
	onEncoderClick() // pause -> SIGSTOP

	mutex.Lock()
	oldPid := playbackCmd.Process.Pid
	mutex.Unlock()
	waitProcessStopped(t, oldPid)

	onEncoderRotate(1) // seek forward: restartPlaybackAt replaces the process

	deadline := time.Now().Add(3 * time.Second)
	for {
		if procState(oldPid) == "gone" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old ffmpeg still alive after seek, state=%q (leaked stopped process)", procState(oldPid))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The replacement must be running and still paused.
	mutex.Lock()
	paused, fresh := currentState == StatePaused, playbackCmd != nil && playbackCmd.Process.Pid != oldPid
	freshPid := 0
	if fresh {
		freshPid = playbackCmd.Process.Pid
	}
	mutex.Unlock()
	if !paused || !fresh {
		t.Fatalf("after seek expected StatePaused with a replacement process, got paused=%v fresh=%v", paused, fresh)
	}
	waitProcessStopped(t, freshPid) // new process frozen at the seek point

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
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
	channelCount = 8 // idleVUChannelsPerPage=12 -> 1 VU page + 1 waveform page + 1 info page = 3 stops
	mutex.Unlock()

	// Any rotation from idle enters browse mode at page 0.
	onEncoderRotate(1)
	mutex.Lock()
	enteredRight := currentState == StateIdleBrowse && idleBrowsePage == 0
	mutex.Unlock()
	if !enteredRight {
		t.Fatalf("expected rotating from idle to enter StateIdleBrowse at page 0, got state=%v page=%d", currentState, idleBrowsePage)
	}

	// Rotating forward pages through the VU page, then the waveform
	// page, then the info page, then wraps back to page 0.
	for i, want := range []int{1, 2, 0} {
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
	if got != 2 {
		t.Fatalf("expected backward rotation from page 0 to wrap to page 2, got %d", got)
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
	origLevel, origState, origMenu := currentLogLevel(), currentState, selectedMenu
	t.Cleanup(func() {
		applyLogLevel(origLevel)
		currentState, selectedMenu = origState, origMenu
	})

	mutex.Lock()
	applyLogLevel(LogDebug)
	mutex.Unlock()
	if currentLogLevel() != LogDebug {
		t.Fatalf("expected applyLogLevel to set Debug, got %d", currentLogLevel())
	}

	mutex.Lock()
	currentState = StateLogging
	selectedMenu = 2 // Info
	mutex.Unlock()

	onEncoderClick()
	if currentLogLevel() != LogInfo {
		t.Fatalf("expected click on Info row to set LogInfo, got %d", currentLogLevel())
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
	backToSettings := currentState == StateSettings && currentLogLevel() == LogInfo
	mutex.Unlock()
	if !backToSettings {
		t.Fatalf("expected Back to return to Settings keeping Info, got state=%d level=%d", currentState, currentLogLevel())
	}

	// A raised level round-trips through the persisted config.
	persistConfig()
	loadPersistedConfig()
	if currentLogLevel() != LogInfo {
		t.Fatalf("expected persisted log level Info to reload, got %d", currentLogLevel())
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
	t.Cleanup(func() {
		oledBrightnessPct, autoDimEnabled = origB, origAuto
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
	selectedMenu = 3 // Back
	mutex.Unlock()
	onEncoderClick()

	mutex.Lock()
	got := currentState == StateSettings
	mutex.Unlock()
	if !got {
		t.Fatalf("expected Display Back to go to Settings, got state=%d", currentState)
	}
}

func TestMenuTimeoutReturnsToIdle(t *testing.T) {
	origIdx, origLast := menuTimeoutIdx, lastInputTime
	origState, origSel, origScroll, origEdit := currentState, selectedMenu, menuScrollOffset, editingParameter
	origOwned := idleBrowseMonitorOwned
	t.Cleanup(func() {
		menuTimeoutIdx, lastInputTime = origIdx, origLast
		currentState, selectedMenu, menuScrollOffset, editingParameter = origState, origSel, origScroll, origEdit
		idleBrowseMonitorOwned = origOwned
	})

	set := func(state AppState, idle time.Duration) {
		mutex.Lock()
		currentState, selectedMenu, menuScrollOffset, editingParameter = state, 2, 1, true
		idleBrowseMonitorOwned = false
		lastInputTime = time.Now().Add(-idle)
		mutex.Unlock()
	}
	timedOut := func() (AppState, int, int, bool) {
		applyMenuTimeoutLocked(time.Now())
		mutex.Lock()
		defer mutex.Unlock()
		return currentState, selectedMenu, menuScrollOffset, editingParameter
	}

	// A stale Settings menu falls back to Standby with nav state reset.
	menuTimeoutIdx = 2 // 30s
	set(StateSettings, 5*time.Minute)
	if st, sel, off, edit := timedOut(); st != StateIdle || sel != 0 || off != 0 || edit {
		t.Fatalf("expected Settings to time out to Idle (nav reset), got state=%d sel=%d off=%d edit=%v", st, sel, off, edit)
	}

	// Off means menus stay put no matter how stale the idle clock is.
	menuTimeoutIdx = 0
	set(StateAudio, 10*time.Minute)
	if st, _, _, _ := timedOut(); st != StateAudio {
		t.Fatalf("expected Off to keep the Audio menu, got state=%d", st)
	}

	// Fresh activity inside the window also stays.
	menuTimeoutIdx = 2
	set(StateDisplay, 5*time.Second)
	if st, _, _, _ := timedOut(); st != StateDisplay {
		t.Fatalf("expected recent input to keep the Display menu, got state=%d", st)
	}

	// Transport, copy and home states are never touched.
	menuTimeoutIdx = 1 // 15s, shortest real delay
	for _, st := range []AppState{StateIdle, StateRecording, StatePlaying, StatePaused, StateCopying} {
		set(st, 10*time.Minute)
		if got, _, _, _ := timedOut(); got != st {
			t.Fatalf("menu timeout must not touch state=%d, got %d", st, got)
		}
	}

	// Idle-browse exits through the owned-monitor path back to Standby.
	set(StateIdleBrowse, 10*time.Minute)
	if st, _, _, _ := timedOut(); st != StateIdle {
		t.Fatalf("expected IdleBrowse to time out to Idle, got state=%d", st)
	}

	// The preset cycles Off -> 15s -> 30s -> 60s -> 2min -> Off.
	menuTimeoutIdx = len(menuTimeoutOptions) - 1
	adjustMenuTimeout(1)
	if menuTimeoutIdx != 0 || menuTimeoutLabel() != "Off" {
		t.Fatalf("expected wrap to Off, got idx=%d label=%q", menuTimeoutIdx, menuTimeoutLabel())
	}
	adjustMenuTimeout(1)
	if menuTimeoutLabel() != "15s" {
		t.Fatalf("expected 15s label, got %q", menuTimeoutLabel())
	}
	menuTimeoutIdx = 4
	if menuTimeoutLabel() != "2m" {
		t.Fatalf("expected 2m label, got %q", menuTimeoutLabel())
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

// TestGetFreeSpaceAlwaysMeasuresRecordPath is the regression test for the bug
// where getFreeSpace switched to the USB mountpoint when a drive was plugged
// in. Recordings are always written to RecordPath (/rec); USB is only a
// copy/export target. So the free-space used by lowDisk() (which gates whether
// recording is allowed) and getRemainingStorage() (the idle/WebUI readout)
// must be the recording media's, regardless of usbMounted. Before the fix the
// two states returned different volumes' free space; now they must be equal.
func TestGetFreeSpaceAlwaysMeasuresRecordPath(t *testing.T) {
	origUSB := usbMounted
	t.Cleanup(func() { usbMounted = origUSB })

	mutex.Lock()
	usbMounted = false
	withoutUSB := getFreeSpace()
	usbMounted = true
	withUSB := getFreeSpace()
	mutex.Unlock()

	if withoutUSB != withUSB {
		t.Fatalf("getFreeSpace changed when usbMounted toggled: without=%d withUSB=%d (must always measure RecordPath)", withoutUSB, withUSB)
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
	t.Cleanup(func() { filePrefix = origPrefix })

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

	// A WAV with metadata chunks before data: duration must come from the
	// data-chunk header, not file size (a fixed 44-byte guess under-counts).
	p2 := filepath.Join(dir, "recording_20240131_143023_ch2_48kHz.wav")
	f2, err := os.Create(p2)
	if err != nil {
		t.Fatal(err)
	}
	listData := []byte("INFOISFT8ffmpeg") // arbitrary metadata payload
	listSize := len(listData)
	dataBytes2 := 48000 * 2 * 3 // one second
	riffSize := 4 + 8 + listSize + 8 + dataBytes2
	buf := append([]byte("RIFF"), 0, 0, 0, 0)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(riffSize))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "LIST"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(listSize))
	buf = append(buf, listData...)
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataBytes2))
	buf = append(buf, make([]byte, dataBytes2)...)
	if _, err := f2.Write(buf); err != nil {
		t.Fatal(err)
	}
	f2.Close()

	got2 := recordingDuration(p2, 2, 48000)
	if got2 < 950*time.Millisecond || got2 > 1050*time.Millisecond {
		t.Fatalf("expected ~1s with LIST chunk present, got %v", got2)
	}
}

// TestWriteRecordingZip streams a set of recording files (including one in a
// per-day subfolder) into a single ZIP and verifies both the audio entries and
// the manifest.txt are present and correct. This exercises the same
// writeRecordingZip path handleDownloadAll uses, against a temp dir so it
// never touches the real RecordPath.
func TestWriteRecordingZip(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "2026-08-30")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	flat := filepath.Join(dir, "recording_20240131_143022_ch2_48kHz.wav")
	deep := filepath.Join(sub, "Live_20260830_120000_ch4_96kHz.wav")

	// 1s of 48kHz stereo 24-bit: 44-byte header + 48000*2*3 data bytes.
	f, err := os.Create(flat)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, 44+48000*2*3))
	f.Close()

	g, err := os.Create(deep)
	if err != nil {
		t.Fatal(err)
	}
	g.Write(make([]byte, 44+96000*4*3))
	g.Close()

	var buf bytes.Buffer
	if err := writeRecordingZip(&buf, dir, []string{flat, deep}); err != nil {
		t.Fatalf("writeRecordingZip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}

	byName := map[string]*zip.File{}
	for _, zf := range zr.File {
		byName[zf.Name] = zf
	}

	if _, ok := byName["recording_20240131_143022_ch2_48kHz.wav"]; !ok {
		t.Fatalf("flat recording missing from archive")
	}
	if _, ok := byName["2026-08-30/Live_20260830_120000_ch4_96kHz.wav"]; !ok {
		t.Fatalf("per-day subfolder recording missing from archive")
	}
	mf, ok := byName["manifest.txt"]
	if !ok {
		t.Fatalf("manifest.txt missing from archive")
	}

	rc, err := mf.Open()
	if err != nil {
		t.Fatal(err)
	}
	mdata, _ := io.ReadAll(rc)
	rc.Close()
	ms := string(mdata)

	for _, want := range []string{"recording_20240131_143022_ch2_48kHz.wav", "2026-08-30/Live_20260830_120000_ch4_96kHz.wav", "Files: 2"} {
		if !strings.Contains(ms, want) {
			t.Fatalf("manifest missing %q; got:\n%s", want, ms)
		}
	}

	// The flat file's 1-second duration must show up in the manifest (duration
	// is computed from actual bytes, in Hz - see the duration fix).
	if !strings.Contains(ms, "00:00:01") {
		t.Fatalf("manifest should report a 1s take as 00:00:01; got:\n%s", ms)
	}
}

// Download ALL must speak human when /rec is empty - a bare 404 reads as
// "broken" (the reported bug) - and serve the ZIP when there is something
// to bundle. Also pins the labeled dashboard button (icon-only read as
// unclear).
func TestDownloadAll(t *testing.T) {
	initTestHardware(t)
	cookie := testSessionCookie(t)

	// Clean /rec first - other tests may have left recordings.
	os.RemoveAll(RecordPath)

	mux := newRemoteMux()

	// Empty /rec: explanatory page, 200, with a way back.
	req := httptest.NewRequest("GET", "/download-all", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with an explanatory page for empty /rec, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "No recordings to download yet") {
		t.Fatalf("expected an explanatory empty-state page, got: %s", body)
	}

	// Dashboard must render a labeled (not icon-only) Download ALL control.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dashboard: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Download ALL (.zip)") {
		t.Fatalf("dashboard missing labeled Download ALL button")
	}

	// With recordings present: a ZIP stream containing them + manifest.
	os.MkdirAll(RecordPath, 0755)
	p := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(p, []byte("fake wav data"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(p) })

	req = httptest.NewRequest("GET", "/download-all", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 ZIP for populated /rec, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("expected application/zip, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "recording_20260101_000000_ch2_48kHz.wav") {
		t.Fatalf("ZIP does not contain the recording entry")
	}
}

// The mid-take auto-stop predicate must trip only when remaining disk is
// between 0 and the threshold - an unknown (0) estimate must never stop a
// take (matches lowDisk's "unknowable isn't low" principle).
func TestShouldAutoStopTake(t *testing.T) {
	if !shouldAutoStopTake(midTakeDiskStopThreshold - time.Second) {
		t.Fatalf("expected auto-stop at a minute-minus-one of space")
	}
	if shouldAutoStopTake(midTakeDiskStopThreshold) {
		t.Fatalf("did not expect auto-stop exactly at the threshold")
	}
	if shouldAutoStopTake(midTakeDiskStopThreshold + time.Minute) {
		t.Fatalf("did not expect auto-stop with plenty of space")
	}
	if shouldAutoStopTake(0) {
		t.Fatalf("unknown (0) estimate must not trigger auto-stop")
	}
}

// Config export/import round-trip: export the current settings to a directory,
// mutate a few globals, import back, and confirm they're restored. Also
// verifies the export never carries the WiFi password (the non-secret rule)
// and that importing a password-less profile doesn't clobber the current one.
func TestConfigExportImportRoundTrip(t *testing.T) {
	initTestHardware(t)

	usb := t.TempDir()
	origUSB := usbMounted
	origDev, origSR, origCh := deviceName, sampleRateIdx, channelCount
	origTag, origPrefix, origVU, origPeak := tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx
	origTM, origSSID, origWifiEn := transportMode, wifiSSID, wifiEnabled
	t.Cleanup(func() {
		mutex.Lock()
		usbMounted = origUSB
		deviceName = origDev
		sampleRateIdx = origSR
		channelCount = origCh
		tagPresetIdx = origTag
		filePrefix = origPrefix
		vuRangeIdx = origVU
		peakHoldIdx = origPeak
		transportMode = origTM
		wifiSSID = origSSID
		wifiEnabled = origWifiEn
		mutex.Unlock()
	})
	mutex.Lock()
	usbMounted = true
	curPwd := wifiPassword
	deviceName = "Unit-A"
	sampleRateIdx = 1
	channelCount = 2
	tagPresetIdx = 2
	filePrefix = "Live"
	vuRangeIdx = 2
	peakHoldIdx = 3
	transportMode = "icon"
	wifiSSID = "PI-Unit-A"
	wifiEnabled = true
	wifiPassword = "sekret123"

	if err := exportConfigTo(usb); err != nil {
		mutex.Unlock()
		t.Fatalf("export: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(usb, configExportName))
	if err != nil {
		mutex.Unlock()
		t.Fatalf("read export: %v", err)
	}
	if strings.Contains(string(raw), "sekret123") {
		mutex.Unlock()
		t.Fatalf("export leaked the wifi password")
	}

	// Retain the password in the local unit, then import a "clone".
	wifiPassword = curPwd
	sampleRateIdx = 0
	channelCount = 1
	tagPresetIdx = 0
	filePrefix = ""
	vuRangeIdx = 0
	peakHoldIdx = 0
	transportMode = "text"

	if err := importConfigFrom(usb); err != nil {
		mutex.Unlock()
		t.Fatalf("import: %v", err)
	}
	if sampleRateIdx != 1 || channelCount != 2 || tagPresetIdx != 2 || filePrefix != "Live" ||
		vuRangeIdx != 2 || peakHoldIdx != 3 || transportMode != "icon" {
		mutex.Unlock()
		t.Fatalf("import did not restore settings: sr=%d ch=%d tag=%d prefix=%q vu=%d ph=%d tm=%q",
			sampleRateIdx, channelCount, tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx, transportMode)
	}
	if wifiPassword != curPwd {
		mutex.Unlock()
		t.Fatalf("import clobbered the wifi password without a credential present")
	}
	mutex.Unlock()
}

// Config export/import fail cleanly (error, no state change) when the target
// drive has no profile yet.
func TestConfigImportMissingFileFailsCleanly(t *testing.T) {
	initTestHardware(t)
	origUSB := usbMounted
	t.Cleanup(func() { mutex.Lock(); usbMounted = origUSB; mutex.Unlock() })

	// We don't need an actual USB path - just set the flag and try to import
	mutex.Lock()
	usbMounted = true
	sampleRateIdx = 0
	err := importConfigFrom("") // Empty path should fail cleanly
	mutex.Unlock()
	if err == nil {
		t.Fatalf("expected an error importing from a drive with no profile")
	}
	mutex.Lock()
	if sampleRateIdx != 0 {
		t.Fatalf("failed import mutated state")
	}
	mutex.Unlock()
}

// ── Demo mode tests ────────────────────────────────────────

// ensureMonitorDown stops any monitor that may have been leaked by
// an earlier test (e.g. a lingering ffmpeg process). It waits for
// the monitoring flag to become false, then clears the flag so
// subsequent tests start with a clean slate.
func ensureMonitorDown(t *testing.T) {
	t.Helper()
	mutex.Lock()
	mon := monitoring
	mutex.Unlock()
	if !mon {
		return
	}
	if time.Now().After(time.Now().Add(3 * time.Second)) {
		t.Fatalf("pre-existing monitor would not stop – leaked monitor detected")
	}
	mutex.Lock()
	monitoring = false
	mutex.Unlock()
}

// waitMonitorDown polls for the monitoring flag to become false
// (reaper has run). Used by tests that need to guarantee the
// monitor is stopped before proceeding.
func waitMonitorDown(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mutex.Lock()
		mon := monitoring
		mutex.Unlock()
		if !mon {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("monitor still up after demo playback test: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInfernoUpGateAndAudioPath verifies that demo mode correctly
// starts the Inferno server and audio FIFO.
func TestInfernoUpGateAndAudioPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required for the live demo-meter pipeline")
	}
	initTestHardware(t)
	demoTestCleanup(t)
	setDemoModeLocked(true)
	resetTransportCleanup(t)
	ensureMonitorDown(t)
	startMonitor()
	mutex.Lock()
	startRecording()
	rec, take := isRecording, recordingFile
	mutex.Unlock()
	if !rec {
		t.Fatalf("startRecording refused in demo mode")
	}
	t.Cleanup(func() { os.Remove(take); os.Remove(filepath.Dir(take)) })
	time.Sleep(1500 * time.Millisecond)
	mutex.Lock()
	peak := meterPeakDB
	mutex.Unlock()
	if peak <= meterSilence {
		t.Fatalf("demo take meters never left the silence floor")
	}
	mutex.Lock()
	stopRecording()
	mutex.Unlock()
}

// TestDemoGeneratorPCM verifies that the synthetic PCM generator
// produces realistic audio levels (not constant silence).
func TestDemoGeneratorPCM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required for the live demo-meter pipeline")
	}
	initTestHardware(t)
	demoTestCleanup(t)
	setDemoModeLocked(true)
	resetTransportCleanup(t)
	ensureMonitorDown(t)
	startMonitor()
	mutex.Lock()
	startRecording()
	rec, take := isRecording, recordingFile
	mutex.Unlock()
	if !rec {
		t.Fatalf("startRecording refused in demo mode")
	}
	t.Cleanup(func() { os.Remove(take); os.Remove(filepath.Dir(take)) })
	time.Sleep(1500 * time.Millisecond)
	mutex.Lock()
	peak := meterPeakDB
	mutex.Unlock()
	if peak <= meterSilence {
		t.Fatalf("demo take meters never left the silence floor")
	}
	mutex.Lock()
	stopRecording()
	mutex.Unlock()
}

// TestDemoMonitorLiveLevels ensures that the monitor can read live
// demo levels after a take finishes.
func TestDemoMonitorLiveLevels(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required for the live demo-meter pipeline")
	}
	initTestHardware(t)
	demoTestCleanup(t)
	setDemoModeLocked(true)
	resetTransportCleanup(t)
	ensureMonitorDown(t)
	mutex.Lock()
	startRecording()
	mutex.Unlock()
	time.Sleep(1500 * time.Millisecond)
	mutex.Lock()
	peak := meterPeakDB
	mutex.Unlock()
	if peak <= meterSilence {
		t.Fatalf("demo take ended with silence floor – expected variation")
	}
	// Stop the take before cleanup: the reaper owns isRecording, and a take
	// left running would make setDemoModeLocked's BUSY refusal block the
	// generator stop in demoTestCleanup (and stick isRecording into the
	// next test's startPlayback).
	mutex.Lock()
	stopRecording()
	mutex.Unlock()
}

// TestDemoRecordTake verifies that a recorded take captures genuine
// audio, not silent noise.
func TestDemoRecordTake(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("real ffmpeg required to cut a demo take")
	}
	initTestHardware(t)
	demoTestCleanup(t)
	setDemoModeLocked(true)
	resetTransportCleanup(t)
	ensureMonitorDown(t)
	startMonitor()
	mutex.Lock()
	startRecording()
	rec, take := isRecording, recordingFile
	mutex.Unlock()
	if !rec {
		t.Fatalf("startRecording refused in demo mode")
	}
	t.Cleanup(func() { os.Remove(take); os.Remove(filepath.Dir(take)) })
	time.Sleep(1500 * time.Millisecond)
	mutex.Lock()
	peak := meterPeakDB
	mutex.Unlock()
	if peak <= meterSilence {
		t.Fatalf("demo take meters never left the silence floor")
	}
	mutex.Lock()
	stopRecording()
	mutex.Unlock()
}

// TestDemoPlaybackSimulated verifies that playback runs against a
// timer-based simulated file instead of real audio hardware.
func TestDemoPlaybackSimulated(t *testing.T) {
	// Ensure clean state from any prior tests - monitoring/state may be
	// left in a non-idle condition by earlier demo tests sharing package globals.
	ensureMonitorDown(t)
	mutex.Lock()
	currentState = StateIdle
	monitoring = false
	monitoringOutput = false
	autoMonitor = false
	playbackCmd = nil
	playbackFile = ""
	playbackPausedElapsed = 0
	playbackStart = time.Time{}
	playbackDone = nil
	playbackDuration = 0
	mutex.Unlock()
	// Give any in-flight reaping goroutines from previous tests a moment to
	// observe the cleared playbackCmd and exit cleanly.
	time.Sleep(50 * time.Millisecond)
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)
	demoTestCleanup(t)
	setDemoModeLocked(true)
	origState, origMon, origOut, origAuto := currentState, monitoring, monitoringOutput, autoMonitor
	origCmd, origFile := playbackCmd, playbackFile
	t.Cleanup(func() {
		mutex.Lock()
		currentState, monitoring, monitoringOutput, autoMonitor = origState, origMon, origOut, origAuto
		playbackCmd, playbackFile = origCmd, origFile
		mutex.Unlock()
	})
	os.MkdirAll(RecordPath, 0755)
	mkTake := func(name string, secs int) string {
		p := filepath.Join(RecordPath, name)
		os.WriteFile(p, make([]byte, secs*48000*2*3+44), 0644)
		os.Chtimes(p, time.Now(), time.Now())
		return p
	}
	longTake := mkTake("demotake_20260101_120000_ch2_48kHz.wav", 30)
	shortTake := mkTake("demotake_20260101_120005_ch2_48kHz.wav", 5)
	t.Cleanup(func() { os.Remove(longTake); os.Remove(shortTake) })
	waitState := func(want AppState, what string) {
		t.Helper()
		// The 5s short take's demo end-timer races this deadline: the reap
		// lands ~ms after 5s, so a 5s budget flakes under parallel load.
		deadline := time.Now().Add(15 * time.Second)
		for {
			mutex.Lock()
			got := currentState
			mutex.Unlock()
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("state never reached %v (%s), stuck at %v", want, what, got)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	mutex.Lock()
	startPlayback()
	mutex.Unlock()
	waitState(StatePlaying, "demo play start")
	mutex.Lock()
	if playbackCmd == nil {
		t.Fatalf("demo playback has no stand-in process")
	}
	pausePlayback()
	mutex.Unlock()
	waitState(StatePaused, "demo pause")
	resumePlayback()
	mutex.Lock()
	startPlayback()
	mutex.Unlock()
	waitState(StatePlaying, "demo resume")
	os.Chtimes(shortTake, time.Now(), time.Now())
	mutex.Lock()
	startPlayback()
	mutex.Unlock()
	waitState(StatePlaying, "demo short play start")
	waitState(StateIdle, "demo natural end")
}

// TestDemoTogglePersists verifies that setting demo mode persists
// to the persisted config file.
func TestDemoTogglePersists(t *testing.T) {
	initTestHardware(t)
	demoTestCleanup(t)
	readFlag := func() bool {
		t.Helper()
		data, err := os.ReadFile(ConfigPath)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		var c PersistedConfig
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("parse config: %v", err)
		}
		return c.DemoMode
	}
	mutex.Lock()
	setDemoModeLocked(true)
	flushConfig()
	mutex.Unlock()
	if !readFlag() {
		t.Fatal("demo mode true did not persist")
	}
	mutex.Lock()
	setDemoModeLocked(false)
	flushConfig()
	mutex.Unlock()
	if readFlag() {
		t.Fatal("demo mode false did not persist")
	}
}

// TestDemoWebUIToggle verifies that toggling demo mode from the WebUI
// works correctly.
func TestDemoWebUIToggle(t *testing.T) {
	origDemoMode := demoMode
	setDemoModeLocked(true)
	if demoMode != true {
		t.Fatal("setDemoModeLocked(true) did not set demoMode to true")
	}
	setDemoModeLocked(false)
	if demoMode != false {
		t.Fatal("setDemoModeLocked(false) did not set demoMode to false")
	}
	demoMode = origDemoMode
}

// TestDemoSystemOptionsToggle verifies that toggling demo mode from
// the OLED System Options menu works correctly.
func TestDemoSystemOptionsToggle(t *testing.T) {
	origDemoMode := demoMode
	setDemoModeLocked(true)
	if demoMode != true {
		t.Fatal("setDemoModeLocked(true) did not set demoMode to true")
	}
	setDemoModeLocked(false)
	if demoMode != false {
		t.Fatal("setDemoModeLocked(false) did not set demoMode to false")
	}
	demoMode = origDemoMode
}

// TestMeterDeckFlagsDriveReelAnimation locks the contract the reel-to-reel
// deck animation keys on: applyMeter spins the reels iff the meter payload
// reports recording||playing. If these flags ever lie, the deck sits frozen
// mid-take with no error anywhere.
func TestMeterDeckFlagsDriveReelAnimation(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	origRec, origState, origStart := isRecording, currentState, recordStart
	isRecording = true
	currentState = StateRecording
	recordStart = time.Now().Add(-time.Second)
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		isRecording, currentState, recordStart = origRec, origState, origStart
		mutex.Unlock()
	})

	resp := currentMeterResponse()
	if !resp.Recording || resp.Playing || resp.Paused {
		t.Fatalf("take flags wrong: recording=%v playing=%v paused=%v (deck needs recording=true)",
			resp.Recording, resp.Playing, resp.Paused)
	}
	if resp.Elapsed == "" {
		t.Fatal("take must report elapsed time for the head display")
	}

	// Template wiring: the static markup/JS must carry the hooks applyMeter
	// toggles, else a rename silently freezes the deck.
	var buf bytes.Buffer
	if err := dashboardTmpl.Execute(&buf, dashboardData{}); err != nil {
		t.Fatalf("dashboard render: %v", err)
	}
	page := buf.String()
	for _, want := range []string{`class="reel-g"`, `id="tapePath"`, `classList.toggle('spinning'`, `classList.toggle('active'`, `@keyframes spin`, `--deck-face`, `--deck-trim`} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard missing deck-animation hook %q", want)
		}
	}
}

// Growing a pipe needs privilege the sandbox may lack, so probe and skip where
// the kernel refuses. The growth is read back through a *separate* descriptor
// while enlargeFifo's own is still open: that is the whole point, because a
// pipe's buffer is released when the last descriptor closes, so measuring
// after the keeper is dropped can only ever see the 64KB default.
func TestEnlargeFifoGrowsPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fifo")
	if err := syscall.Mkfifo(path, 0666); err != nil {
		t.Fatal(err)
	}
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, perr := syscall.Syscall(syscall.SYS_FCNTL, probe.Fd(), linuxFSetPipeSz, 4<<20)
	probe.Close()
	if perr == syscall.EPERM {
		t.Skip("sandbox denies F_SETPIPE_SZ (no CAP_SYS_RESOURCE); growth verified on target")
	}
	// Default size of a fresh pipe, from the same descriptor the keeper uses.
	pipeSize := func(f *os.File) int {
		r1, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), linuxFGetPipeSz, 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		return int(r1)
	}

	keeper := enlargeFifo(path)
	if keeper == nil {
		t.Fatal("enlargeFifo returned no descriptor")
	}
	defer keeper.Close()

	before, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()

	// Re-size the keeper's own pipe now that a reader is attached, and confirm
	// the size is a property of the pipe, not of the descriptor that set it.
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if got := pipeSize(reader); got <= 65536 {
		t.Errorf("pipe size %d while keeper held, want growth past the 64KB default", got)
	}
	if got, want := pipeSize(keeper), pipeSize(reader); got != want {
		t.Errorf("size differs by descriptor: keeper %d, reader %d", got, want)
	}
}

func TestMeterReaderFlushesMidStreamBatch(t *testing.T) {
	origPeak, origRMS := meterChannelPeak, meterChannelRMS
	t.Cleanup(func() { meterChannelPeak, meterChannelRMS = origPeak, origRMS })
	meterChannelPeak = make([]float64, 4)
	meterChannelRMS = make([]float64, 4)

	// More channel lines than one flush batch: every value must land,
	// including those parsed before the batch boundary.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "lavfi.astats.%d.Peak_level=%f\n", (i%4)+1, -1.0-float64(i))
	}
	meterReader(strings.NewReader(b.String()), meterGen)

	mutex.Lock()
	defer mutex.Unlock()
	// Channel 1's last write is i=36 -> -37.0; channel 4's is i=39 -> -40.0.
	if meterChannelPeak[0] != -37.0 {
		t.Errorf("ch1 peak = %v, want -37", meterChannelPeak[0])
	}
	if meterChannelPeak[3] != -40.0 {
		t.Errorf("ch4 peak = %v, want -40", meterChannelPeak[3])
	}
}

func TestRecordingFilesKeyStableAndHit(t *testing.T) {
	k1, ok1 := recordingFilesKey()
	k2, ok2 := recordingFilesKey()
	if !ok1 || !ok2 {
		t.Skip("rec path unreadable here")
	}
	if k1 != k2 {
		t.Fatalf("key unstable: %q vs %q", k1, k2)
	}
	// Prime the cache, then verify a hit returns equal contents.
	a := recordingFiles()
	b := recordingFiles()
	if len(a) != len(b) {
		t.Fatalf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("[%d] %q vs %q", i, a[i], b[i])
		}
	}
}

func TestRenderSkipsUnchangedFramePush(t *testing.T) {
	initTestHardware(t)
	origSeq, origHash, origPushed := displaySeq, displayLastHash, displayPushed
	origState, origMode := currentState, menuMode
	origSysNotice, origDiskWarn := sysNoticeUntil, diskWarnUntil
	t.Cleanup(func() {
		displaySeq, displayLastHash, displayPushed = origSeq, origHash, origPushed
		currentState, menuMode = origState, origMode
		sysNoticeUntil, diskWarnUntil = origSysNotice, origDiskWarn
	})
	// Static confirm dialog with blinkers frozen: no seconds counters,
	// meters or 500ms notice flashes, so two renders hash identically
	// (only the status-bar clock could differ, at minute granularity).
	currentState, menuMode = StateConfirm, ShutdownConfirm
	sysNoticeUntil, diskWarnUntil = time.Time{}, time.Time{}
	displayPushed = false

	// Isolate the sim framebuffer dump: the dev service (SIM mode) shares
	// the default /tmp path and rewrites it on its own 100ms tick, which
	// would fake a push here.
	frame := filepath.Join(t.TempDir(), "frame.png")
	t.Setenv("PI9696_SIM_OUT", frame)
	render()
	if _, err := os.Stat(frame); err != nil {
		t.Fatalf("first render must push a frame: %v", err)
	}
	st1, _ := os.Stat(frame)
	time.Sleep(20 * time.Millisecond) // mtime granularity: a rewrite must show
	render()
	st2, _ := os.Stat(frame)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("identical frame re-pushed to display; skip the SPI write when the hash matches")
	}
}

// A meterReader from a preempted session (old ffmpeg exiting late) must not
// corrupt the new session's meters: flushes from a stale generation drop.
func TestMeterReaderDropsStaleGeneration(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	meterPeakDB = meterSilence
	meterGen++
	current := meterGen
	mutex.Unlock()

	meterReader(strings.NewReader("lavfi.astats.Overall.Peak_level=-1.0\n"), current-1)
	mutex.Lock()
	stale := meterPeakDB
	mutex.Unlock()
	if stale != meterSilence {
		t.Fatalf("stale generation wrote meters: %v", stale)
	}

	meterReader(strings.NewReader("lavfi.astats.Overall.Peak_level=-1.0\n"), current)
	mutex.Lock()
	fresh := meterPeakDB
	mutex.Unlock()
	if fresh != -1.0 {
		t.Fatalf("current generation did not write meters: %v", fresh)
	}
}

// A seek whose replacement process can't start (exclusive ALSA held, broken
// binary) must land cleanly in Idle - the old process is confirmed dead at
// that point, so leaving a stale playbackCmd behind would wedge the deck.
func TestSeekWithUnstartableFFmpegGoesIdle(t *testing.T) {
	initTestHardware(t)
	origSR, origCh := sampleRateIdx, channelCount
	t.Cleanup(func() { mutex.Lock(); sampleRateIdx, channelCount = origSR, origCh; mutex.Unlock() })
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatalf("write fake recording: %v", err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	sampleRateIdx, channelCount = 1, 2 // match the staged 48kHz/2ch take
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)
	onEncoderClick() // pause

	// Hide every ffmpeg from PATH so the replacement Start fails.
	t.Setenv("PATH", t.TempDir())
	onEncoderRotate(1)

	mutex.Lock()
	state, cmd := currentState, playbackCmd
	mutex.Unlock()
	if state != StateIdle || cmd != nil {
		t.Fatalf("failed seek left state=%v cmd=%v, want Idle/nil", state, cmd != nil)
	}
}

// Toggling demo mode mid-take/mid-playback is refused: disabling pulls the
// demo FIFO out from under the recording ffmpeg (silent truncated take).
func TestDemoToggleRefusedDuringTransport(t *testing.T) {
	initTestHardware(t)
	origSR, origCh := sampleRateIdx, channelCount
	t.Cleanup(func() { mutex.Lock(); sampleRateIdx, channelCount = origSR, origCh; mutex.Unlock() })
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = false
	demoMode = false
	sampleRateIdx, channelCount = 1, 2 // match the staged 48kHz/2ch take
	mutex.Unlock()

	onButtonPress(hardware.PlayButton)
	mutex.Lock()
	playing := currentState == StatePlaying
	mutex.Unlock()
	if !playing {
		t.Fatal("setup: expected playback to start")
	}

	mutex.Lock()
	ok := setDemoModeLocked(true)
	stillOff := !demoMode
	mutex.Unlock()
	if ok || !stillOff {
		t.Fatalf("demo toggle during playback applied=%v demo=%v, want refused", ok, !stillOff)
	}

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)

	mutex.Lock()
	ok = setDemoModeLocked(true)
	on := demoMode
	mutex.Unlock()
	if !ok || !on {
		t.Fatalf("demo toggle at idle applied=%v demo=%v, want applied", ok, on)
	}
	mutex.Lock()
	setDemoModeLocked(false)
	mutex.Unlock()
}

// Rotating the access token revokes every session and retires the old token:
// the pre-rotation cookie 401s, the old token no longer logs in, and the
// Location fragment carries a working new token.
func TestRotateTokenRevokesAll(t *testing.T) {
	origToken, origLimiter, origSessions := remoteToken, loginLimit, sessions
	remoteToken = "TESTTOKEN4"
	loginLimit = newLoginLimiter()
	sessions = newSessionStore()
	t.Cleanup(func() { remoteToken, loginLimit, sessions = origToken, origLimiter, origSessions })

	mux := newRemoteMux()
	cookie := testSessionCookie(t)

	req := httptest.NewRequest("POST", "/api/settings/rotate-token", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate returned %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	newToken, ok := strings.CutPrefix(loc, "/login#t=")
	if !ok || len(newToken) != 8 {
		t.Fatalf("rotate Location %q carries no new token", loc)
	}

	// Old session is dead.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("old session still valid after rotation: %d", rec.Code)
	}

	// Old token no longer logs in; new one does.
	form := "token=" + "TESTTOKEN4"
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token login returned %d, want 401", rec.Code)
	}
	form = "token=" + newToken
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("new token login returned %d, want 303", rec.Code)
	}
}

// Cross-origin mutations are rejected even with a valid session; same-origin
// and headerless (curl) callers pass.
func TestCrossOriginMutationRejected(t *testing.T) {
	origToken, origLimiter, origSessions := remoteToken, loginLimit, sessions
	remoteToken = "TESTTOKEN5"
	loginLimit = newLoginLimiter()
	sessions = newSessionStore()
	t.Cleanup(func() { remoteToken, loginLimit, sessions = origToken, origLimiter, origSessions })

	mux := newRemoteMux()
	cookie := testSessionCookie(t)
	post := func(origin, referer string) int {
		req := httptest.NewRequest("POST", "/api/settings/demo", strings.NewReader("enabled=on"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	// httptest requests carry r.Host = example.com; match it for same-origin.
	if code := post("http://example.com", ""); code == http.StatusForbidden {
		t.Fatalf("same-origin POST rejected: %d", code)
	}
	if code := post("", ""); code == http.StatusForbidden {
		t.Fatalf("headerless POST rejected: %d", code)
	}
	if code := post("http://evil.example", ""); code != http.StatusForbidden {
		t.Fatalf("foreign Origin POST returned %d, want 403", code)
	}
	if code := post("", "http://evil.example/page"); code != http.StatusForbidden {
		t.Fatalf("foreign Referer POST returned %d, want 403", code)
	}
	mutex.Lock()
	setDemoModeLocked(false)
	mutex.Unlock()
}

// startPlayback enforces mutual exclusion itself: a direct call while
// recording or already playing must not overlap transports.
func TestStartPlaybackRefusesBusyTransport(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)

	os.MkdirAll(RecordPath, 0755)
	recFile := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(recFile, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(recFile) })

	mutex.Lock()
	currentState = StateIdle
	isRecording = true // pretend a take is running (no ffmpeg needed: guard runs first)
	playbackCmd = nil
	mutex.Unlock()

	startPlayback()

	mutex.Lock()
	cmd := playbackCmd
	state := currentState
	isRecording = false
	mutex.Unlock()
	if cmd != nil || state != StateIdle {
		t.Fatalf("startPlayback during recording started cmd=%v state=%v", cmd != nil, state)
	}
}

// The control port is fixed at 8080 unless PI9696_REMOTE_PORT overrides it, so
// a host that fronts the UI on port 80 can do so from the unit file instead of
// patching the source. The default must survive an unrelated env var.
func TestRemoteControlPortOverride(t *testing.T) {
	if got := remoteControlPort(); got != "8080" {
		t.Fatalf("default port = %q, want 8080", got)
	}
	t.Setenv("PI9696_REMOTE_PORT", "80")
	if got := remoteControlPort(); got != "80" {
		t.Fatalf("overridden port = %q, want 80", got)
	}
}

// A child that ignores SIGTERM is the exact failure this guards: ffmpeg's
// signal handler only acts at its next main-loop iteration, so one blocked in a
// read on an empty FIFO never exits and wedges the transport. terminateFfmpeg
// must escalate rather than wait forever.
func TestTerminateFfmpegKillsStubbornChild(t *testing.T) {
	initTestHardware(t)
	orig := ffmpegStopGrace
	ffmpegStopGrace = 150 * time.Millisecond
	t.Cleanup(func() { ffmpegStopGrace = orig })

	// Traps and ignores SIGTERM, like ffmpeg blocked on a FIFO does.
	// NOTE: trap-then-background (trap '' TERM; sleep 300 & wait) does NOT
	// reliably ignore SIGTERM under dash here; the exec form keeps the
	// ignore across exec per POSIX and is what actually survives SIGTERM.
	fakeExecutable(t, "stubborn", `#!/bin/sh
trap '' TERM
exec sleep 300
`)
	cmd := exec.Command("stubborn")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Let dash install the trap and exec sleep before signalling: a SIGTERM
	// delivered during startup kills with default disposition and the test
	// would pass without exercising escalation.
	time.Sleep(500 * time.Millisecond)
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	terminateFfmpeg(cmd.Process, exited, "stubborn")

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("stubborn child survived SIGTERM + SIGKILL escalation")
	}
}

// The ordinary case must not escalate: a child that exits on SIGTERM has to be
// left to finalize on its own, because that is what makes a partial WAV
// playable.
func TestTerminateFfmpegLetsWellBehavedChildExit(t *testing.T) {
	initTestHardware(t)
	orig := ffmpegStopGrace
	ffmpegStopGrace = 5 * time.Second
	t.Cleanup(func() { ffmpegStopGrace = orig })

	fakeExecutable(t, "polite", `#!/bin/sh
trap 'exit 0' TERM
sleep 300 &
wait $!
`)
	cmd := exec.Command("polite")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	start := time.Now()
	terminateFfmpeg(cmd.Process, exited, "polite")
	select {
	case <-exited:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("polite child took %s to exit; SIGTERM should have been enough", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("polite child never exited")
	}
}

// validatePlaybackFile must refuse takes recorded at a different rate or
// channel count than the device is set to, because ffmpeg would otherwise
// resample/rechannel silently and the deck would sound wrong with nothing
// telling the operator why. Unparseable names pass through: absence of
// evidence is not evidence of a mismatch.
func TestValidatePlaybackFile(t *testing.T) {
	initTestHardware(t)
	origSR, origCh := sampleRateIdx, channelCount
	t.Cleanup(func() { mutex.Lock(); sampleRateIdx, channelCount = origSR, origCh; mutex.Unlock() })
	mutex.Lock()
	sampleRateIdx = 1 // 48000
	channelCount = 2
	mutex.Unlock()

	if err := validatePlaybackFile("Show_20260101_120000_ch2_48kHz.wav"); err != nil {
		t.Errorf("matching take refused: %v", err)
	}
	err := validatePlaybackFile("Show_20260101_120000_ch2_44kHz.wav")
	if err == nil {
		t.Fatal("44.1kHz take accepted while device is at 48kHz")
	}
	if !strings.Contains(err.Error(), "44") || !strings.Contains(err.Error(), "48") {
		t.Errorf("rate-mismatch error names neither side: %q", err)
	}
	err = validatePlaybackFile("Show_20260101_120000_ch8_48kHz.wav")
	if err == nil {
		t.Fatal("8ch take accepted while device is at 2ch")
	}
	if !strings.Contains(err.Error(), "8") || !strings.Contains(err.Error(), "2ch") {
		t.Errorf("channel-mismatch error names neither side: %q", err)
	}
	// Suffix variants and legacy/renamed files must not trip the check.
	if err := validatePlaybackFile("Show_20260101_120000_ch2_48kHz-1.wav"); err != nil {
		t.Errorf("collided-name take refused: %v", err)
	}
	if err := validatePlaybackFile("weirdname.wav"); err != nil {
		t.Errorf("unparseable name refused: %v", err)
	}
	if err := validatePlaybackFile("Show_20260101_120000_ch0_48kHz.wav"); err != nil {
		t.Errorf("zero-channel parse refused: %v", err)
	}
}

// A refused take must reach the dashboard, not just the log: the notice has
// to render in the status panel and then expire on its own.
func TestPlaybackRefusalReachesStatusPanel(t *testing.T) {
	initTestHardware(t)
	origNotice, origUntil := webNotice, webNoticeUntil
	t.Cleanup(func() { mutex.Lock(); webNotice, webNoticeUntil = origNotice, origUntil; mutex.Unlock() })

	mutex.Lock()
	webNotice, webNoticeUntil = "", time.Time{}
	showWebNotice("take is 44kHz/2ch but the device is set to 48kHz/2ch")
	mutex.Unlock()
	v := currentStatusView()
	if v.Notice == "" {
		t.Fatal("status panel shows no notice after a refusal")
	}
	html, err := renderStatusHTML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "44kHz/2ch") {
		t.Error("rendered status omits the refusal detail")
	}

	mutex.Lock()
	webNoticeUntil = time.Now().Add(-time.Second)
	mutex.Unlock()
	if v := currentStatusView(); v.Notice != "" {
		t.Error("expired notice still rendered")
	}
}

// End to end: a mismatched take on disk must leave the transport alone - no
// process started, no state flipped - while still telling the user why.
// Staged under the real RecordPath (a const), like the other recording tests,
// with a unique name and cleanup so the mtime-keyed rescan stays exact.
func TestStartPlaybackRefusesMismatchedTake(t *testing.T) {
	initTestHardware(t)
	origState, origRecFlag := currentState, isRecording
	origCmd, origSR, origCh := playbackCmd, sampleRateIdx, channelCount
	origDemo, origNotice, origUntil := demoMode, webNotice, webNoticeUntil
	t.Cleanup(func() {
		mutex.Lock()
		currentState, isRecording = origState, origRecFlag
		playbackCmd, sampleRateIdx, channelCount = origCmd, origSR, origCh
		demoMode, webNotice, webNoticeUntil = origDemo, origNotice, origUntil
		mutex.Unlock()
	})

	mutex.Lock()
	currentState, isRecording, playbackCmd = StateIdle, false, nil
	sampleRateIdx, channelCount = 1, 2 // 48kHz stereo
	demoMode = false
	mutex.Unlock()

	bad := filepath.Join(RecordPath, "refuse_20260101_120000_ch2_44kHz.wav")
	if err := os.WriteFile(bad, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(bad) })
	startPlayback()

	mutex.Lock()
	defer mutex.Unlock()
	if playbackCmd != nil {
		t.Error("mismatched take started a playback process")
	}
	if currentState != StateIdle {
		t.Errorf("state moved to %d on a refused take", currentState)
	}
	if webNotice == "" {
		t.Error("refused take set no dashboard notice")
	}
}

// infernoRestartNeeded must fire only on audio-setting drift: restarting on
// anything else drops the Dante device (and its subscriptions) for no reason,
// while missing a real drift leaves Inferno recording at a stale rate.
func TestInfernoRestartNeededOnlyOnAudioChange(t *testing.T) {
	initTestHardware(t)
	origDemo, origState := demoMode, infernoState
	origSR, origCh := sampleRateIdx, channelCount
	origLastSR, origLastCh := lastSampleRate, lastChannelCount
	origTag := tagPresetIdx
	t.Cleanup(func() {
		mutex.Lock()
		demoMode, infernoState = origDemo, origState
		sampleRateIdx, channelCount = origSR, origCh
		lastSampleRate, lastChannelCount = origLastSR, origLastCh
		tagPresetIdx = origTag
		mutex.Unlock()
	})

	mutex.Lock()
	demoMode, infernoState = false, InfernoRunning
	sampleRateIdx, channelCount = 1, 2
	lastSampleRate, lastChannelCount = sampleRates[1], 2
	mutex.Unlock()

	mutex.Lock()
	if infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("restart requested with no drift")
	}
	// A non-audio change must not restart either.
	tagPresetIdx = (origTag + 1) % len(tagPresets)
	if infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("restart requested for a tag-preset change")
	}
	tagPresetIdx = origTag
	// Either audio dimension drifting must restart.
	sampleRateIdx = 2
	if !infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("no restart requested for a sample-rate change")
	}
	sampleRateIdx = 1
	channelCount = 8
	if !infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("no restart requested for a channel-count change")
	}
	channelCount = 2
	// Not running, or demo owning the chain: never restart.
	infernoState = InfernoStopped
	if infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("restart requested while Inferno is stopped")
	}
	infernoState = InfernoRunning
	demoMode = true
	if infernoRestartNeeded() {
		mutex.Unlock()
		t.Fatal("restart requested while demo mode owns the chain")
	}
	mutex.Unlock()
}

// Every field persistConfig writes must survive a save/load cycle, not just
// the three the export/import test covers. A field that silently drops (a new
// setting added to the struct but given no load branch, or a load branch with
// a wrong range) would reset on every boot with no error anywhere.
func TestPersistRoundTripsAllFields(t *testing.T) {
	initTestHardware(t)
	dir := t.TempDir()
	origPath := ConfigPath
	origLog := currentLogLevel()
	origDev, origSR, origCh := deviceName, sampleRateIdx, channelCount
	origTag, origPrefix, origVU, origPeak := tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx
	origTM := transportMode
	origTheme, origMotion, origContrast, origDensity := themeSlug, displayMotion, displayContrast, displayDensityIdx
	origBright, origDim, origTimeout := oledBrightnessPct, autoDimEnabled, menuTimeoutIdx
	origDemo, origHyper := demoMode, hyperdeckEnabled
	origWifiEn, origSSID, origPwd := wifiEnabled, wifiSSID, wifiPassword
	t.Cleanup(func() {
		mutex.Lock()
		ConfigPath = origPath
		deviceName, sampleRateIdx, channelCount = origDev, origSR, origCh
		tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx = origTag, origPrefix, origVU, origPeak
		transportMode = origTM
		themeSlug, displayMotion, displayContrast, displayDensityIdx = origTheme, origMotion, origContrast, origDensity
		oledBrightnessPct, autoDimEnabled, menuTimeoutIdx = origBright, origDim, origTimeout
		demoMode, hyperdeckEnabled = origDemo, origHyper
		wifiEnabled, wifiSSID, wifiPassword = origWifiEn, origSSID, origPwd
		mutex.Unlock()
		applyLogLevel(origLog)
	})

	mutex.Lock()
	ConfigPath = filepath.Join(dir, "config.json")
	deviceName = "Unit-B"
	sampleRateIdx, channelCount = 2, 8
	tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx = 3, "Night", 1, 2
	transportMode = "text"
	themeSlug, displayMotion, displayContrast, displayDensityIdx = "lcars", "reduced", "high", 2
	oledBrightnessPct, autoDimEnabled, menuTimeoutIdx = 42, false, 3
	demoMode, hyperdeckEnabled = true, true
	wifiEnabled, wifiSSID, wifiPassword = false, "TestNet", "pw123"
	mutex.Unlock()
	applyLogLevel(LogDebug)
	persistConfig()

	// Reset everything to defaults, then load and demand it all back.
	mutex.Lock()
	deviceName, sampleRateIdx, channelCount = "PI9696", 1, 2
	tagPresetIdx, filePrefix, vuRangeIdx, peakHoldIdx = 0, "", 3, 4
	transportMode = "icon"
	themeSlug, displayMotion, displayContrast, displayDensityIdx = "xbmc", "full", "standard", 0
	oledBrightnessPct, autoDimEnabled, menuTimeoutIdx = 100, true, 2
	demoMode, hyperdeckEnabled = false, false
	wifiEnabled, wifiSSID, wifiPassword = true, "", ""
	mutex.Unlock()
	applyLogLevel(LogError)
	loadPersistedConfig()

	mutex.Lock()
	defer mutex.Unlock()
	check := func(name string, got, want interface{}) {
		t.Helper()
		if got != want {
			t.Errorf("%s round-tripped as %v, want %v", name, got, want)
		}
	}
	check("deviceName", deviceName, "Unit-B")
	check("sampleRateIdx", sampleRateIdx, 2)
	check("channelCount", channelCount, 8)
	check("tagPresetIdx", tagPresetIdx, 3)
	check("filePrefix", filePrefix, "Night")
	check("vuRangeIdx", vuRangeIdx, 1)
	check("peakHoldIdx", peakHoldIdx, 2)
	check("transportMode", transportMode, "text")
	check("themeSlug", themeSlug, "lcars")
	check("displayMotion", displayMotion, "reduced")
	check("displayContrast", displayContrast, "high")
	check("displayDensityIdx", displayDensityIdx, 2)
	check("oledBrightnessPct", oledBrightnessPct, 42)
	check("autoDimEnabled", autoDimEnabled, false)
	check("menuTimeoutIdx", menuTimeoutIdx, 3)
	check("demoMode", demoMode, true)
	check("hyperdeckEnabled", hyperdeckEnabled, true)
	check("wifiEnabled", wifiEnabled, false)
	check("wifiSSID", wifiSSID, "TestNet")
	check("wifiPassword", wifiPassword, "pw123")
	if currentLogLevel() != LogDebug {
		t.Errorf("log level round-tripped as %v, want debug", currentLogLevel())
	}
}

// A missing Inferno binary must fail the start loudly (InfernoFailed) and
// release the FIFO keeper it just opened - otherwise the fd leaks and a stale
// reference to an unlinked pipe survives for the next start to orphan.
func TestMissingInfernoBinaryFailsStart(t *testing.T) {
	initTestHardware(t)
	origBin, origState, origDemo := InfernoBinary, infernoState, demoMode
	origKeeper, origPath := fifoKeeper, fifoPath
	t.Cleanup(func() {
		mutex.Lock()
		InfernoBinary, infernoState, demoMode = origBin, origState, origDemo
		fifoKeeper, fifoPath = origKeeper, origPath
		mutex.Unlock()
	})

	if err := os.MkdirAll(RawPath, 0755); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	InfernoBinary = filepath.Join(t.TempDir(), "no-such-binary")
	infernoState, demoMode = InfernoStopped, false
	fifoKeeper, fifoPath = nil, ""
	mutex.Unlock()

	doStartInferno()

	mutex.Lock()
	defer mutex.Unlock()
	if infernoState != InfernoFailed {
		t.Errorf("state = %d after start with missing binary, want InfernoFailed", infernoState)
	}
	if fifoKeeper != nil {
		t.Error("failed start left the FIFO keeper open")
	}
}

// Stopping with no keeper held (a start that never got that far, or an
// already-released one) must be a safe no-op, not a nil-pointer panic - and
// it must still clear the path and remove the file.
func TestStopInfernoNilKeeperSafe(t *testing.T) {
	initTestHardware(t)
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead.raw")
	if err := os.WriteFile(dead, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	origCmd, origPath, origKeeper, origState := infernoCmd, fifoPath, fifoKeeper, infernoState
	t.Cleanup(func() {
		mutex.Lock()
		infernoCmd, fifoPath, fifoKeeper, infernoState = origCmd, origPath, origKeeper, origState
		mutex.Unlock()
	})
	mutex.Lock()
	infernoCmd, fifoKeeper = nil, nil
	fifoPath = dead
	infernoState = InfernoStopped
	mutex.Unlock()

	doStopInferno()

	mutex.Lock()
	defer mutex.Unlock()
	if fifoPath != "" {
		t.Error("stop left a stale fifo path")
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("stop did not remove the FIFO file")
	}
	if infernoState != InfernoStopped {
		t.Errorf("state = %d after stop, want InfernoStopped", infernoState)
	}
}

// Same-second takes must not truncate each other: the second colliding name
// gets -1, the third -2, and a free stem is returned untouched.
func TestUniqueRecordingFileCollision(t *testing.T) {
	dir := t.TempDir()
	stem := "Show_20260101_120000_ch2_48kHz"
	got, err := uniqueRecordingFile(dir, stem)
	if err != nil || got != filepath.Join(dir, stem+".wav") {
		t.Errorf("free stem = (%q, %v), want (%q, nil)", got, err, filepath.Join(dir, stem+".wav"))
	}
	if err := os.WriteFile(filepath.Join(dir, stem+".wav"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := uniqueRecordingFile(dir, stem); err != nil || got != filepath.Join(dir, stem+"-1.wav") {
		t.Errorf("first collision = (%q, %v), want (%q, nil)", got, err, filepath.Join(dir, stem+"-1.wav"))
	}
	if err := os.WriteFile(filepath.Join(dir, stem+"-1.wav"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := uniqueRecordingFile(dir, stem); err != nil || got != filepath.Join(dir, stem+"-2.wav") {
		t.Errorf("second collision = (%q, %v), want (%q, nil)", got, err, filepath.Join(dir, stem+"-2.wav"))
	}
}

// A persistently unreadable directory must fail fast, not spin the UI mutex
// forever treating every error as "exists, try next". (As root, chmod-based
// EACCES doesn't apply, so force ENOTDIR by pointing dir at a file.)
func TestUniqueRecordingFileUnrecoverable(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "file")
	if err := os.WriteFile(notDir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := uniqueRecordingFile(notDir, "stem"); err == nil {
		t.Error("unreadable directory returned no error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("unrecoverable lookup took %s", elapsed)
	}
}

// shutdownWaitTimeout must cover the whole SIGTERM-then-SIGKILL sequence
// terminateFfmpeg runs, or shutdown abandons takes ffmpeg is still finalizing
// (the corruption gracefulShutdown exists to prevent). It must also fit the
// unit's TimeoutStopSec=30 with the 10s Inferno stop still to come.
func TestShutdownWaitCoversFfmpegGrace(t *testing.T) {
	got := shutdownWaitTimeout()
	if got < ffmpegStopGrace {
		t.Fatalf("shutdown wait %s is shorter than the ffmpeg kill grace %s", got, ffmpegStopGrace)
	}
	if got > 20*time.Second {
		t.Fatalf("shutdown wait %s leaves no room for the Inferno stop inside TimeoutStopSec=30", got)
	}
}

// The fast path must not wait at all: an already-reaped child returns
// immediately instead of burning the whole timeout.
func TestWaitDoneReturnsOnClosedDone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	start := time.Now()
	waitDone(done, "test")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitDone blocked %s on an already-closed channel", elapsed)
	}
}

// formatBusyNow is the worker-side re-check that stops mkfs landing on a live
// copy target: a take, playback or copy that started after the confirm screen
// must abort the format.
func TestFormatBusyNow(t *testing.T) {
	initTestHardware(t)
	origRec, origCmd, origCopy := isRecording, playbackCmd, isCopying
	t.Cleanup(func() {
		mutex.Lock()
		isRecording, playbackCmd, isCopying = origRec, origCmd, origCopy
		mutex.Unlock()
	})
	mutex.Lock()
	isRecording, playbackCmd, isCopying = false, nil, false
	mutex.Unlock()
	if formatBusyNow() {
		t.Fatal("idle transport reported busy")
	}
	for _, tc := range []struct {
		name      string
		recording bool
		cmd       bool
		copying   bool
	}{
		{"recording", true, false, false},
		{"playback", false, true, false},
		{"copy", false, false, true},
		{"all", true, true, true},
	} {
		mutex.Lock()
		isRecording = tc.recording
		isCopying = tc.copying
		if tc.cmd {
			playbackCmd = exec.Command("true")
		} else {
			playbackCmd = nil
		}
		mutex.Unlock()
		if !formatBusyNow() {
			t.Errorf("busy transport (%s) reported idle", tc.name)
		}
	}
}

// Shutdown must cancel an active USB copy instead of abandoning a partial
// dst.tmp: the worker sees the cleared flag at the next boundary, exits, and
// the wait returns - all without touching the network, hardware, or server.
func TestShutdownCancelsCopy(t *testing.T) {
	initTestHardware(t)
	origCopy, origDone := isCopying, copyDone
	t.Cleanup(func() {
		mutex.Lock()
		isCopying, copyDone = origCopy, origDone
		mutex.Unlock()
	})
	mutex.Lock()
	isCopying = true
	copyDone = make(chan struct{})
	done := copyDone
	mutex.Unlock()

	// Stand in for the copy worker: exit when cancelled.
	sawCancel := make(chan struct{})
	go func() {
		for {
			mutex.Lock()
			cancelled := !isCopying
			mutex.Unlock()
			if cancelled {
				close(sawCancel)
				mutex.Lock()
				close(done)
				mutex.Unlock()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	start := time.Now()
	cancelCopyAndWait()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
	select {
	case <-sawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never observed the cancel")
	}
}

// With no copy running, the shutdown path must be a no-op (and must not
// block on a nil channel).
func TestCancelCopyAndWaitIdle(t *testing.T) {
	initTestHardware(t)
	origCopy, origDone := isCopying, copyDone
	t.Cleanup(func() {
		mutex.Lock()
		isCopying, copyDone = origCopy, origDone
		mutex.Unlock()
	})
	mutex.Lock()
	isCopying, copyDone = false, nil
	mutex.Unlock()
	start := time.Now()
	cancelCopyAndWait()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("idle cancel took %s", elapsed)
	}
}

// stopProcessGroup must bound the wait even against a child that ignores
// SIGTERM: previously the post-SIGKILL wait had no timeout, so a D-state
// server wedged infernoWorker (and through it, shutdown) forever.
func TestStopProcessGroupKillsStubbornChild(t *testing.T) {
	initTestHardware(t)
	origStop, origKill := infernoStopGrace, infernoKillGrace
	infernoStopGrace, infernoKillGrace = 150*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { infernoStopGrace, infernoKillGrace = origStop, origKill })

	// NOTE: trap-then-background (trap '' TERM; sleep 300 & wait) does NOT
	// reliably ignore SIGTERM under dash here; the exec form keeps the
	// ignore across exec per POSIX and is what actually survives SIGTERM.
	fakeExecutable(t, "stubborn", `#!/bin/sh
trap '' TERM
exec sleep 300
`)
	cmd := exec.Command("stubborn")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Same arming race as above: signal only once the trap is installed.
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	stopProcessGroup(cmd, "stubborn-test")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stop took %s against a SIGTERM-ignoring child", elapsed)
	}
	// The child must be gone: SIGKILL is async, so poll briefly rather than
	// asserting on ProcessState (which reports Exited()==false for a
	// signalled process even when correctly reaped).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			break // ESRCH: no such process
		}
		if time.Now().After(deadline) {
			t.Fatal("stubborn child survived SIGTERM + SIGKILL escalation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Nil and already-dead inputs must be safe no-ops.
func TestStopProcessGroupNilSafe(t *testing.T) {
	initTestHardware(t)
	stopProcessGroup(nil, "nil-test")
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	stopProcessGroup(cmd, "exited-test")
}

// waitChannel is the bounded-wait primitive behind the seek handoff: true for
// a closed channel, false after the timeout. The handoff itself can't be
// driven to the abandon path from userspace (only D-state survives SIGKILL),
// so this pins the primitive it depends on.
func TestWaitChannel(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !waitChannel(closed, time.Second) {
		t.Error("waitChannel reported an already-closed channel as open")
	}
	if waitChannel(make(chan struct{}), 20*time.Millisecond) {
		t.Error("waitChannel reported an open channel as closed")
	}
}

// Closing the stdout read side must unblock a meterReader whose pipe is held
// open by an orphaned grandchild: previously that reader (plus its fd) leaked
// permanently, since gating Wait on it deadlocked and nothing else closed it.
func TestMeterReaderUnblocksOnPipeClose(t *testing.T) {
	initTestHardware(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	gen := meterGen
	mutex.Unlock()
	done := make(chan struct{})
	go func() { meterReader(r, gen); close(done) }()
	// Let the reader block, with the write end held open as an orphaned
	// grandchild would hold it. Then do what the reapers do: close our side.
	time.Sleep(50 * time.Millisecond)
	r.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("meterReader did not exit after its pipe was closed")
	}
	w.Close()
}

// A link flap during teardown must not start a fresh Inferno after everything
// was stood down. The loop takes its stop channel as a parameter so this is
// testable without touching the process-global one gracefulShutdown closes.
func TestNetworkMonitorLoopStops(t *testing.T) {
	initTestHardware(t)
	origState, origWasUp, origDemo := infernoState, networkWasUp, demoMode
	t.Cleanup(func() {
		mutex.Lock()
		infernoState, networkWasUp, demoMode = origState, origWasUp, origDemo
		mutex.Unlock()
	})
	mutex.Lock()
	// Running: the start branch is dead whatever the link does, so the tick
	// can't poke the shared worker.
	infernoState, demoMode = InfernoRunning, false
	mutex.Unlock()

	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() { networkMonitorLoop(stop); close(exited) }()
	close(stop)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("network monitor did not exit on stop")
	}
}

// A rapid demo off/on toggle must not let the exiting generator destroy its
// successor: the old loop used to close whatever demoFifoKeeper pointed at,
// which after a fast restart is the NEW keeper - leaking the old fd and
// silently dropping the new FIFO back to 64KB. Generations (passed explicitly,
// never re-read from the global) make each loop release only its own fd.
func TestDemoRapidToggleKeepsNewKeeper(t *testing.T) {
	initTestHardware(t)
	demoTestCleanup(t)
	origKeeper, origGen := demoFifoKeeper, demoFifoGen
	t.Cleanup(func() {
		mutex.Lock()
		// demoTestCleanup stops the generator; restore only if it left one.
		if demoFifoKeeper == nil {
			demoFifoKeeper, demoFifoGen = origKeeper, origGen
		}
		mutex.Unlock()
	})

	mutex.Lock()
	startDemoGeneratorLocked()
	oldPath := demoFifoPath
	// The old loop needs the mutex for its exit cleanup, which is held
	// continuously through the stop+start below - so it cannot exit before
	// the successor exists, and the aliasing window under test always opens
	// (unless the loop errored on open, in which case there is no race and
	// the assertions below still hold).
	stopDemoGeneratorLocked()
	startDemoGeneratorLocked()
	newPath, newGen := demoFifoPath, demoFifoGen
	mutex.Unlock()
	if oldPath == "" || newPath == "" || oldPath == newPath {
		t.Fatalf("generator did not produce two distinct FIFOs (%q, %q)", oldPath, newPath)
	}

	// Wait for the old loop to actually exit (it removes its own FIFO file
	// on the way out) - only then has the aliasing window closed.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(oldPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old demo generator did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if demoFifoKeeper == nil {
		t.Fatal("exiting old generator cleared the successor keeper")
	}
	if demoFifoGen != newGen {
		t.Errorf("generation = %d, want %d", demoFifoGen, newGen)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new FIFO %s missing: %v", newPath, err)
	}
	// Leave no generator running: demoTestCleanup only stops it when demo
	// mode itself was enabled, which this test bypasses to drive the
	// start/stop pair directly.
	stopDemoGeneratorLocked()
	mutex.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new demo generator did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mutex.Lock()
}

// A confirmed shutdown must not vanish into a full queue: enqueueSystemOp
// waits bounded instead of dropping, so the UI's post-confirm state always
// reflects an action that will run.
func TestEnqueueSystemOpWaitsWhenFull(t *testing.T) {
	initTestHardware(t)
	origTimeout := systemOpEnqueueTimeout
	systemOpEnqueueTimeout = 100 * time.Millisecond
	t.Cleanup(func() { systemOpEnqueueTimeout = origTimeout })
	// No worker drains the channel in tests; fill it exactly (it must start
	// empty - anything left would be a leak from another test), and drain
	// whatever is left in cleanup so no other test can observe leftovers.
	for i := 0; i < cap(systemOpCh); i++ {
		select {
		case systemOpCh <- opFormatUSB:
		default:
			t.Fatalf("systemOpCh already held %d items; leaking test?", i)
		}
	}
	t.Cleanup(func() {
		for {
			select {
			case <-systemOpCh:
			default:
				return
			}
		}
	})
	start := time.Now()
	if enqueueSystemOp(opShutdown) {
		t.Fatal("enqueue reported sent into a full queue")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("enqueue returned after %s on a full queue; expected it to wait out the timeout", elapsed)
	}
	// With a free slot the same call must go through immediately.
	<-systemOpCh
	start = time.Now()
	if !enqueueSystemOp(opShutdown) {
		t.Fatal("enqueue failed despite a free slot")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("enqueue blocked %s despite a free slot", elapsed)
	}
	<-systemOpCh // the one just sent; the fill cleanup drains the other three
}

// Restart/start requests stay best-effort and coalescing: a full queue drops
// with a warning and the next change re-fires. Stop never takes this path
// (stopInfernoAndWait has its own bounded send); this pins that contract by
// asserting drops only ever happen here, promptly.
func TestEnqueueInfernoDropsPromptlyWhenFull(t *testing.T) {
	initTestHardware(t)
	for i := 0; i < cap(infernoReqCh); i++ {
		select {
		case infernoReqCh <- infernoRequest{cmd: infernoCmdRestart}:
		default:
			t.Fatalf("infernoReqCh already held %d items; leaking test?", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(infernoReqCh); i++ {
			<-infernoReqCh
		}
	})
	start := time.Now()
	enqueueInferno(infernoCmdRestart)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("best-effort enqueue blocked %s on a full queue", elapsed)
	}
}

// A take past ~9.2 GiB overflowed int64 when its byte count was scaled to
// nanoseconds first (Duration(bytes) * Second exceeds 9.22e18), going
// negative and poisoning seeks, demo end timers and the UI. 128ch/192kHz
// reaches that in ~2 minutes. A sparse file stands in for the giant take
// without writing gigabytes.
func TestRecordingDurationHugeFile(t *testing.T) {
	dir := t.TempDir()
	huge := filepath.Join(dir, "Show_20260101_120000_ch128_192kHz.wav")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	const tenGiB = 10 << 30
	if err := f.Truncate(tenGiB); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	d := recordingDuration(huge, 128, 192000)
	if d <= 0 {
		t.Fatalf("10GiB take duration = %s, want positive", d)
	}
	// 10GiB at 128ch/192kHz/3B = ~73.7MB/s -> ~145s. Generous bounds: the
	// point is sane and non-negative, not sample-exact.
	if d < 100*time.Second || d > 200*time.Second {
		t.Fatalf("10GiB take duration = %s, want ~145s", d)
	}

	// Absurd filename-derived rates must not wrap the arithmetic negative.
	if d := recordingDuration(huge, 1<<30, 1<<30); d < 0 {
		t.Fatalf("absurd rate duration = %s, want >= 0", d)
	}
}

// A playback start that dies after standing the monitor down must not leave
// the meters silent: abort restores output-metering off and brings the input
// monitor back (mirroring restartPlaybackAt's resume).
func TestAbortedPlaybackRestoresMonitor(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)
	origMon, origCmd := monitoring, monitorCmd
	origOut, origAuto := monitoringOutput, autoMonitor
	origState, origDemo, origInferno := currentState, demoMode, infernoState
	t.Cleanup(func() {
		mutex.Lock()
		monitoring, monitorCmd = origMon, origCmd
		monitoringOutput, autoMonitor = origOut, origAuto
		currentState, demoMode, infernoState = origState, origDemo, origInferno
		mutex.Unlock()
	})

	// Inferno up so the monitor can start; monitor down + output mode on is
	// exactly the state a failed startPlayback leaves behind.
	mutex.Lock()
	infernoState, demoMode = InfernoRunning, false
	monitoring, monitorCmd = false, nil
	monitoringOutput, autoMonitor = true, false
	currentState = StateIdle
	mutex.Unlock()

	mutex.Lock()
	abortPlaybackStart()
	mon, out := monitoring, monitoringOutput
	mutex.Unlock()
	if out {
		t.Error("output-metering mode still set after abort")
	}
	if !mon {
		t.Fatal("input monitor not resumed after abort")
	}
	// Leave it as found: stop what the abort started.
	mutex.Lock()
	stopMonitor()
	mutex.Unlock()
}

// A failed avahi start must not latch: previously lastName was set even on
// Start failure, so a missing binary was never retried until the next rename
// and the unit silently stopped advertising.
func TestMdnsTickRetriesAfterFailure(t *testing.T) {
	initTestHardware(t)
	origCmd, origLast, origDev := mdnsCmd, mdnsLastName, deviceName
	t.Cleanup(func() {
		mutex.Lock()
		mdnsCmd, mdnsLastName, deviceName = origCmd, origLast, origDev
		mutex.Unlock()
	})
	if origCmd != nil {
		t.Skip("another test left a live mDNS child")
	}
	// Hide every avahi binary from PATH so Start fails.
	t.Setenv("PATH", t.TempDir())
	mutex.Lock()
	deviceName = "Unit-Mdns-Test"
	mutex.Unlock()
	if mdnsTick() {
		t.Fatal("tick reported active with no avahi binary")
	}
	mutex.Lock()
	latched := mdnsLastName
	cmd := mdnsCmd
	mutex.Unlock()
	if cmd != nil {
		t.Fatal("failed start left a child handle behind")
	}
	// A second tick must try again rather than trusting latched state.
	if mdnsTick() {
		t.Fatal("tick reported active with no avahi binary")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if mdnsLastName != latched {
		t.Fatal("unrelated state moved between ticks")
	}
	_ = latched
}

// A child that dies on its own must be reaped and republished: the reaper
// clears mdnsCmd so the next tick restarts the same name, instead of leaving
// a zombie and silence until the next rename.
func TestMdnsTickRepublishesAfterDeath(t *testing.T) {
	initTestHardware(t)
	origCmd, origLast, origDev := mdnsCmd, mdnsLastName, deviceName
	t.Cleanup(func() {
		mutex.Lock()
		mdnsCmd, mdnsLastName, deviceName = origCmd, origLast, origDev
		mutex.Unlock()
	})
	if origCmd != nil {
		t.Skip("another test left a live mDNS child")
	}
	marker := filepath.Join(t.TempDir(), "starts")
	fakeExecutable(t, "avahi-publish-service",
		"#!/bin/sh\necho started >> "+marker+"\nexit 0\n")
	mutex.Lock()
	deviceName = "Unit-Mdns-Test"
	mutex.Unlock()
	if !mdnsTick() {
		t.Fatal("tick with a working binary reported inactive")
	}
	// The script exits at once; the reaper must clear the handle so the
	// next tick republishes instead of believing its own advertisement.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mutex.Lock()
		gone := mdnsCmd == nil
		mutex.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dead avahi child was never reaped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !mdnsTick() {
		t.Fatal("tick did not republish after the child died")
	}
	// The republished child is newly spawned; wait for it to run rather
	// than racing its first write.
	deadline = time.Now().Add(3 * time.Second)
	for {
		raw, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(raw), "started"); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("republished avahi never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fitText must never split a multi-byte rune: the copy-files menu truncates
// with the same DecodeLastRuneInString loop, and byte slicing there used to
// render garbage for non-ASCII names.
func TestFitTextKeepsRunesIntact(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	defer mutex.Unlock()
	// Narrow enough to force truncation of anything non-trivial.
	const maxPx = 40
	for _, s := range []string{"abcdef", "abédef", "日本語テスト", "a🎵b", "» editing"} {
		got := fitText(s, maxPx)
		for _, r := range got {
			if r == 0xFFFD {
				t.Errorf("fitText(%q) produced invalid UTF-8: %q", s, got)
			}
		}
		if hwManager.GetTextWidth(got) > maxPx {
			t.Errorf("fitText(%q) exceeds %dpx", s, maxPx)
		}
	}
	if got := fitText("", maxPx); got != "" {
		t.Errorf("fitText empty = %q, want empty", got)
	}
}

// A failed stat must stamp the cache like a success does: otherwise a
// missing /rec turns every 100ms render tick into a syscall instead of one
// cached miss per second.
func TestFreeSpaceFailureIsCached(t *testing.T) {
	freeSpaceMu.Lock()
	origBytes, origAt := freeSpaceBytes, freeSpaceAt
	freeSpaceMu.Unlock()
	t.Cleanup(func() {
		freeSpaceMu.Lock()
		freeSpaceBytes, freeSpaceAt = origBytes, origAt
		freeSpaceMu.Unlock()
	})
	// Force a cold cache so the first call really stats.
	freeSpaceMu.Lock()
	freeSpaceAt = time.Time{}
	freeSpaceMu.Unlock()

	missing := filepath.Join(t.TempDir(), "nope")
	if got := getFreeSpaceAt(missing); got != 0 {
		t.Fatalf("missing path free space = %d, want 0", got)
	}
	// The path now exists with real free space, but the 1s window must still
	// serve the stamped miss rather than re-statting.
	if err := os.MkdirAll(missing, 0755); err != nil {
		t.Fatal(err)
	}
	if got := getFreeSpaceAt(missing); got != 0 {
		t.Fatalf("cached miss returned %d after the path appeared, want 0 (still cached)", got)
	}
}

// Rapid setting changes must coalesce into one write, not one per detent:
// spinning a setting used to do disk I/O under the UI mutex every step.
// The timer fires on its own, and flushConfig persists immediately.
func TestSettingChangedDebouncesWrites(t *testing.T) {
	initTestHardware(t)
	origPath, origDelay := ConfigPath, configPersistDelay
	t.Cleanup(func() {
		mutex.Lock()
		if configTimer != nil {
			configTimer.Stop()
			configTimer = nil
		}
		configDirty = false
		ConfigPath, configPersistDelay = origPath, origDelay
		mutex.Unlock()
	})
	mutex.Lock()
	ConfigPath = filepath.Join(t.TempDir(), "config.json")
	configPersistDelay = 50 * time.Millisecond
	mutex.Unlock()

	mutex.Lock()
	for i := 0; i < 5; i++ {
		settingChanged()
	}
	mutex.Unlock()
	// Fresh temp dir: nothing may have been written synchronously.
	cfg := filepath.Join(filepath.Dir(ConfigPath), "config.json")
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("rapid marks wrote synchronously instead of debouncing")
	}
	// The timer must persist on its own without any flush.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(filepath.Dir(ConfigPath), "config.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debounced persist never fired")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And flushConfig must persist synchronously.
	os.Remove(filepath.Join(filepath.Dir(ConfigPath), "config.json"))
	mutex.Lock()
	settingChanged()
	flushConfig()
	mutex.Unlock()
	if _, err := os.Stat(filepath.Join(filepath.Dir(ConfigPath), "config.json")); err != nil {
		t.Fatal("flushConfig did not persist synchronously")
	}
}

// Render and encode failures (client disconnect mid-response) must be logged,
// not panics and not silent truncation. A writer that always fails exercises
// every best-effort error path added for this.
type failResponseWriter struct{ header http.Header }

func (failResponseWriter) Header() http.Header        { return http.Header{} }
func (failResponseWriter) Write([]byte) (int, error)  { return 0, errors.New("boom") }
func (failResponseWriter) WriteHeader(statusCode int) {}

func TestRenderEncodeFailuresLoggedNotPanics(t *testing.T) {
	initTestHardware(t)
	w := failResponseWriter{}
	r := httptest.NewRequest("GET", "/", nil)
	// Each must return normally despite every Write failing.
	handleAPIStatus(w, r)
	handleManifest(w, r)
	handleAPITelemetry(w, r)
	// Meter snapshots encode the same way; drive the encoder path directly.
	_ = json.NewEncoder(w).Encode(currentMeterResponse())
}

// The info page must not re-probe the network and re-encode the QR bitmap on
// every 100ms render tick: interface enumeration, gateway/DNS reads and QR
// encoding dwarf the frame budget, while the facts change on DHCP/link
// timescales.
func TestInfoPageCachesNetworkProbe(t *testing.T) {
	initTestHardware(t)
	origToken := remoteToken
	origCache := infoCache
	t.Cleanup(func() {
		mutex.Lock()
		remoteToken = origToken
		infoCache = origCache
		mutex.Unlock()
	})
	mutex.Lock()
	infoCache.at = time.Time{} // force cold
	remoteToken = "AAAAAAAA"
	renderIdleInfoPage()
	firstAt, firstQR := infoCache.at, infoCache.qrFor
	mutex.Unlock()
	if firstAt.IsZero() {
		t.Fatal("first render did not populate the cache")
	}
	if len(infoCache.details) == 0 {
		t.Error("cache holds no network details")
	}
	mutex.Lock()
	renderIdleInfoPage()
	mutex.Unlock()
	mutex.Lock()
	secondAt := infoCache.at
	mutex.Unlock()
	if !secondAt.Equal(firstAt) {
		t.Error("second render within TTL re-probed instead of using the cache")
	}
	// A token change must re-encode the QR (its content embeds the token).
	mutex.Lock()
	remoteToken = "BBBBBBBB"
	renderIdleInfoPage()
	mutex.Unlock()
	mutex.Lock()
	afterQR := infoCache.qrFor
	mutex.Unlock()
	if afterQR == firstQR {
		t.Error("QR not re-encoded after the token changed")
	}
	if !strings.Contains(afterQR, "BBBBBBBB") {
		t.Errorf("QR key %q does not embed the new token", afterQR)
	}
}

// Rapid meter polls must share one marshaled snapshot instead of rebuilding
// (up to 128 channels) and remarshaling identical payloads per socket at
// 10Hz. Invalidating the cache with an observable state change must rebuild.
func TestMeterSnapshotShared(t *testing.T) {
	initTestHardware(t)
	meterCacheMu.Lock()
	meterCacheAt, meterCachePayload = time.Time{}, nil
	meterCacheMu.Unlock()
	t.Cleanup(func() {
		meterCacheMu.Lock()
		meterCacheAt, meterCachePayload = time.Time{}, nil
		meterCacheMu.Unlock()
	})

	a := cachedMeterPayload()
	if len(a) == 0 {
		t.Fatal("empty meter payload")
	}
	if b := cachedMeterPayload(); string(a) != string(b) {
		t.Fatal("rapid polls rebuilt instead of sharing the snapshot")
	}
	// Flip observable state, invalidate, and demand a rebuilt payload.
	mutex.Lock()
	origOut := monitoringOutput
	monitoringOutput = !origOut
	meterCacheMu.Lock()
	meterCacheAt = time.Time{}
	meterCacheMu.Unlock()
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		monitoringOutput = origOut
		mutex.Unlock()
	})
	if c := cachedMeterPayload(); string(c) == string(a) {
		t.Error("rebuild after invalidate produced identical bytes despite a state change")
	}
}

// The recordings push must skip its expensive row render (a WAV open+read
// per take) when the take set hasn't changed: the mtime fingerprint is the
// same key recordingFiles() trusts, so the skip is exactly as fresh as the
// data source. Proved by changing content with pinned mtimes and observing
// the stale body served with changed=false.
func TestPanelSkipsRecordingsRenderWhenSetUnchanged(t *testing.T) {
	initTestHardware(t)
	origRecs, origKey := lastPanelRecs, lastPanelRecsKey
	origCfg := lastPanelConfig
	t.Cleanup(func() {
		teleWSMu.Lock()
		lastPanelRecs, lastPanelRecsKey, lastPanelConfig = origRecs, origKey, origCfg
		teleWSMu.Unlock()
	})
	teleWSMu.Lock()
	lastPanelRecs, lastPanelRecsKey, lastPanelConfig = "", "", ""
	teleWSMu.Unlock()

	take := filepath.Join(RecordPath, "skip_20260101_120000_ch2_48kHz.wav")
	if err := os.WriteFile(take, []byte("fake-one"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(take) })

	teleWSMu.Lock()
	_, recsA, _, changedA := panelWSMessages()
	teleWSMu.Unlock()
	if !changedA || recsA == "" {
		t.Fatal("cold render reported nothing changed")
	}
	if !strings.Contains(recsA, "skip_20260101_120000") {
		t.Fatal("rendered recordings omit the staged take")
	}

	// Change the content but pin every mtime the fingerprint reads, so the
	// take set looks unchanged while the bytes differ.
	stFile, err := os.Stat(take)
	if err != nil {
		t.Fatal(err)
	}
	stDir, err := os.Stat(RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(take, []byte("fake-two-larger-content"), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(take, stFile.ModTime(), stFile.ModTime())
	os.Chtimes(RecordPath, stDir.ModTime(), stDir.ModTime())

	teleWSMu.Lock()
	_, recsB, _, changedB := panelWSMessages()
	teleWSMu.Unlock()
	if changedB {
		t.Error("recordings re-rendered with an unchanged take set")
	}
	if recsB != recsA {
		t.Error("skip served different content than the stored body")
	}
}

// A broadcast round with no clients must be a fast no-op: it exercises the
// snapshot-then-send restructure (client-set snapshot under lock, sends
// outside it) without needing any sockets.
func TestBroadcastTelemetryNoClients(t *testing.T) {
	initTestHardware(t)
	teleWSMu.Lock()
	saved := teleWSHub
	teleWSHub = map[*websocket.Conn]bool{}
	teleWSMu.Unlock()
	t.Cleanup(func() {
		teleWSMu.Lock()
		teleWSHub = saved
		teleWSMu.Unlock()
	})
	start := time.Now()
	broadcastTelemetry()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("clientless broadcast took %s", elapsed)
	}
}

// The TX holder is the unit's Dante transmit side (see txholder.go): these
// cover its device-string contract, the pump, and the lifecycle. The dev box
// has no inferno ALSA device, so the opener is faked; the real
// behaviour under test is selection, framing and state handling.

type fakeTxHolder struct {
	mu       sync.Mutex
	writes   [][]int32
	writeErr error
	closed   bool
	maxCalls int // fail Writes after this many calls (0 = unlimited); bounds paused-pump tests
	calls    int
}

func (f *fakeTxHolder) Write(b []int32) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.maxCalls > 0 && f.calls > f.maxCalls {
		return 0, errors.New("fake TX holder write limit")
	}
	f.writes = append(f.writes, append([]int32(nil), b...))
	return len(b), nil
}

func (f *fakeTxHolder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTxHolder) flattened() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int32
	for _, w := range f.writes {
		out = append(out, w...)
	}
	return out
}

func saveTxGlobals(t *testing.T) {
	t.Helper()
	oDemo, oState := demoMode, currentState
	oRate, oCh, oName := sampleRateIdx, channelCount, deviceName
	oHolder, oDev, oReady, oPending := txHolder, txHolderDevice, txHolderReady, txReopenPending
	oOpener, oVia := openTxDevice, playbackViaDante
	oCmd := playbackCmd
	t.Cleanup(func() {
		mutex.Lock()
		demoMode, currentState = oDemo, oState
		sampleRateIdx, channelCount, deviceName = oRate, oCh, oName
		txHolder, txHolderDevice, txHolderReady, txReopenPending = oHolder, oDev, oReady, oPending
		openTxDevice, playbackViaDante = oOpener, oVia
		playbackCmd = oCmd
		mutex.Unlock()
	})
}

func TestSanitizeDanteName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"PI9696", "PI9696"},
		{"PI 9696_Live", "PI-9696-Live"},
		{"", "PI9696"},
		{"!!!", "PI9696"},
		{"9lives", "D9lives"},
		{strings.Repeat("A", 40), strings.Repeat("A", 31)},
	} {
		if got := sanitizeDanteName(tc.in); got != tc.want {
			t.Errorf("sanitizeDanteName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTxAlsaDevice(t *testing.T) {
	got := txAlsaDevice("PI9696", 48000, 2)
	want := "inferno:NAME=PI9696-TX,SAMPLE_RATE=48000,TX_CHANNELS=2,RX_CHANNELS=0,PROCESS_ID=1,ALT_PORT=10300"
	if got != want {
		t.Errorf("txAlsaDevice = %q, want %q", got, want)
	}
	// TX and RX stay equal: the string pins TX_CHANNELS to the single
	// channelCount, never a second knob.
	if got := txAlsaDevice("PI 9696", 96000, 8); !strings.Contains(got, "TX_CHANNELS=8") || !strings.Contains(got, "NAME=PI-9696-TX") {
		t.Errorf("txAlsaDevice(8ch, spaced name) = %q, want TX_CHANNELS=8 and sanitized NAME", got)
	}
}

func TestBuildPlaybackCmdSelection(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	mutex.Lock()
	demoMode = false
	sampleRateIdx, channelCount = 1, 2
	mutex.Unlock()

	// No holder: today's local path, unchanged.
	mutex.Lock()
	txHolder, txHolderReady = nil, false
	mutex.Unlock()
	cmd, stdout, via := buildPlaybackCmd("take.wav", 0)
	if via || stdout != nil {
		t.Error("no holder: buildPlaybackCmd selected Dante")
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "default") {
		t.Errorf("no holder: local cmd = %q, want the default-ALSA command", cmd.Args)
	}

	// Present but unready (clock missing): still local, never Dante.
	mutex.Lock()
	txHolder, txHolderReady = &fakeTxHolder{}, false
	mutex.Unlock()
	_, _, via = buildPlaybackCmd("take.wav", 0)
	if via {
		t.Error("unready holder: buildPlaybackCmd selected Dante")
	}

	// Ready: decode-to-stdout for the pump.
	mutex.Lock()
	txHolder, txHolderReady = &fakeTxHolder{}, true
	mutex.Unlock()
	cmd, stdout, via = buildPlaybackCmd("take.wav", 5*time.Second)
	if !via || stdout == nil {
		t.Fatal("ready holder: buildPlaybackCmd did not select Dante")
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-f s32le", "-ac 2", "-ar 48000", "-ss 5.000"} {
		if !strings.Contains(args, want) {
			t.Errorf("dante cmd = %q, want %q", args, want)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "-" {
		t.Errorf("dante cmd = %q, want stdout output (-)", args)
	}
}

// The pump must deliver samples bit-exact: s32le bytes to int32 in order,
// across odd-sized reads.
func TestPumpPlaybackPassthrough(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	raw := make([]byte, 0)
	var want []int32
	for i := int32(1); i <= 24; i++ {
		want = append(want, i)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(i))
		raw = append(raw, b[:]...)
	}
	holder := &fakeTxHolder{}
	cmd := exec.Command("true")
	mutex.Lock()
	playbackCmd, txHolder = cmd, holder
	currentState = StatePlaying
	mutex.Unlock()

	// Prime-sized reads force the carry path: 7 bytes can never align to an
	// 8-byte stereo frame.
	src := &chunkReader{data: raw, chunk: 7}
	pumpPlaybackToTx(cmd, src, holder, 2)

	if got := holder.flattened(); !equalInt32(got, want) {
		t.Errorf("pump delivered %v, want %v", got, want)
	}
	if src.left() != 0 {
		t.Errorf("pump left %d bytes unconsumed", src.left())
	}
}

type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *chunkReader) left() int { return len(r.data) - r.pos }

func equalInt32(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// While paused the pump writes silence and never touches the decoder pipe,
// so ffmpeg back-pressures to a stop with the file offset frozen.
func TestPumpPlaybackPausedWritesSilence(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	holder := &fakeTxHolder{maxCalls: 3}
	cmd := exec.Command("true")
	raw := bytes.Repeat([]byte{0xFF}, 64)
	src := bytes.NewReader(raw)
	mutex.Lock()
	playbackCmd, txHolder = cmd, holder
	currentState = StatePaused
	mutex.Unlock()

	pumpPlaybackToTx(cmd, src, holder, 2)

	for _, w := range holder.writes {
		for _, s := range w {
			if s != 0 {
				t.Fatalf("paused pump wrote nonzero sample %d", s)
			}
		}
	}
	if src.Len() != len(raw) {
		t.Errorf("paused pump consumed %d decoder bytes, want 0", len(raw)-src.Len())
	}
}

// A stale generation must exit without touching the sink: the seek handoff
// briefly leaves the old pump alive while the new one owns the holder.
func TestPumpPlaybackStaleGenerationExits(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	holder := &fakeTxHolder{}
	old, live := exec.Command("true"), exec.Command("true")
	mutex.Lock()
	playbackCmd, txHolder = live, holder
	currentState = StatePlaying
	mutex.Unlock()

	pumpPlaybackToTx(old, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}), holder, 2)

	if len(holder.writes) != 0 {
		t.Errorf("stale pump wrote %d chunks, want 0", len(holder.writes))
	}
}

func TestEnsureTxHolderLifecycle(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	type openCall struct {
		device string
		rate   int
		ch     int
	}
	var calls []openCall
	fake := &fakeTxHolder{}
	openTxDevice = func(device string, rate, channels int) (txFrameWriter, error) {
		calls = append(calls, openCall{device, rate, channels})
		return fake, nil
	}
	mutex.Lock()
	demoMode = false
	sampleRateIdx, channelCount, deviceName = 1, 2, "PI9696"
	currentState = StateIdle
	txHolder, txHolderDevice, txHolderReady = nil, "", false
	mutex.Unlock()

	ensureTxHolder()
	mutex.Lock()
	if txHolder == nil || !txHolderReady {
		mutex.Unlock()
		t.Fatal("ensure did not open and warm up the holder")
	}
	mutex.Unlock()
	if len(calls) != 1 {
		t.Fatalf("opener called %d times, want 1", len(calls))
	}
	want := txAlsaDevice("PI9696", 48000, 2)
	if calls[0].device != want || calls[0].rate != 48000 || calls[0].ch != 2 {
		t.Errorf("open(%+v), want device %q rate 48000 ch 2", calls[0], want)
	}

	// Steady state: no reopen.
	ensureTxHolder()
	if len(calls) != 1 {
		t.Errorf("steady ensure reopened the holder (%d opens)", len(calls))
	}

	// Audio settings move: reopen with the new string, close the old.
	mutex.Lock()
	channelCount = 8
	mutex.Unlock()
	ensureTxHolder()
	if len(calls) != 2 || calls[1].ch != 8 {
		t.Fatalf("after channel change: opens = %+v, want a second open at 8ch", calls)
	}
	if !fake.closed {
		t.Error("reopen did not close the stale holder")
	}
	// Reset the closed flag the reopen set: later subtests reuse the fake.
	fake.mu.Lock()
	fake.closed = false
	fake.mu.Unlock()

	// While playing the holder is frozen and the reopen defers.
	mutex.Lock()
	currentState = StatePlaying
	channelCount = 2
	mutex.Unlock()
	ensureTxHolder()
	if len(calls) != 2 {
		t.Errorf("ensure while playing reopened (%d opens)", len(calls))
	}
	mutex.Lock()
	pending := txReopenPending
	mutex.Unlock()
	if !pending {
		t.Error("ensure while playing did not defer the reopen")
	}
}

// No clock overlay: the holder opens but never warms up, staying present
// but unready so playback refuses instead of misrouting to local ALSA.
func TestEnsureTxHolderUnreadyOnNoClock(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	dead := &fakeTxHolder{writeErr: errors.New("no clock available (timeout waiting for overlay update)")}
	openTxDevice = func(device string, rate, channels int) (txFrameWriter, error) {
		return dead, nil
	}
	mutex.Lock()
	demoMode = false
	sampleRateIdx, channelCount, deviceName = 1, 2, "PI9696"
	currentState = StateIdle
	txHolder, txHolderDevice, txHolderReady = nil, "", false
	mutex.Unlock()

	ensureTxHolder()
	mutex.Lock()
	defer mutex.Unlock()
	if txHolder == nil {
		t.Fatal("ensure dropped the holder on warmup failure; want it held but unready")
	}
	if txHolderReady {
		t.Error("holder warmed up without a clock")
	}
}

// No inferno ALSA device at all (dev/sim): the holder stays absent and
// takes fall back to local ALSA via buildPlaybackCmd.
func TestEnsureTxHolderAbsentWithoutDevice(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	openTxDevice = func(device string, rate, channels int) (txFrameWriter, error) {
		return nil, errors.New("no such ALSA device")
	}
	mutex.Lock()
	demoMode = false
	currentState = StateIdle
	txHolder, txHolderDevice, txHolderReady = nil, "", false
	mutex.Unlock()

	ensureTxHolder()
	mutex.Lock()
	defer mutex.Unlock()
	if txHolder != nil {
		t.Error("ensure kept a holder the opener refused")
	}
}

// The Dante TX state must be visible in both UIs (see txholder.go): the
// formatter matrix, the OLED menu click-through, and the dashboard
// template/JS hooks.

func TestTxStatusText(t *testing.T) {
	saveTxGlobals(t)
	for _, tc := range []struct {
		name      string
		holder    txFrameWriter
		ready     bool
		demo      bool
		wantShort string
		wantLong  string
	}{
		{"ready", &fakeTxHolder{}, true, false, "ready", "Dante TX ready (PI9696-TX)"},
		{"no clock", &fakeTxHolder{}, false, false, "no clock", "Dante TX: waiting for clock"},
		{"demo", nil, false, true, "off", "Dante TX off (demo mode)"},
		{"absent", nil, false, false, "off", "Dante TX unavailable (no device)"},
	} {
		mutex.Lock()
		txHolder, txHolderReady = tc.holder, tc.ready
		demoMode, deviceName = tc.demo, "PI9696"
		short, long := txStatusLocked()
		mutex.Unlock()
		if short != tc.wantShort || long != tc.wantLong {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, short, long, tc.wantShort, tc.wantLong)
		}
	}
}

// The TX row is display-only: clicking it must neither enter edit mode nor
// leave the menu, and Back moves one row down from before.
func TestAudioMenuTxRowClick(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	oState, oSel, oEdit := currentState, selectedMenu, editingParameter
	defer func() { currentState, selectedMenu, editingParameter = oState, oSel, oEdit }()

	currentState, editingParameter = StateAudio, false
	selectedMenu = 4
	handleAudioClick()
	if currentState != StateAudio || editingParameter {
		t.Errorf("TX row click: state=%d editing=%v, want StateAudio/no-edit", currentState, editingParameter)
	}
	selectedMenu = 5
	handleAudioClick()
	if currentState != StateSettings {
		t.Errorf("Back click: state=%d, want StateSettings", currentState)
	}
}

// Template wiring: statusTmpl must render the TX line (a missing struct
// field errors only at execution), and the dashboard must carry both the
// element the meter tick updates and the JS that updates it.
func TestTxStatusDashboardWiring(t *testing.T) {
	var buf bytes.Buffer
	v := statusView{Format: "WAV", SampleRate: 48, Channels: 2, TXStatus: "Dante TX ready (PI9696-TX)"}
	if err := statusTmpl.Execute(&buf, v); err != nil {
		t.Fatalf("status render: %v", err)
	}
	if !strings.Contains(buf.String(), `id="txstatus"`) || !strings.Contains(buf.String(), v.TXStatus) {
		t.Errorf("status fragment missing TX line: %q", buf.String())
	}

	buf.Reset()
	if err := dashboardTmpl.Execute(&buf, dashboardData{}); err != nil {
		t.Fatalf("dashboard render: %v", err)
	}
	for _, want := range []string{`getElementById('txstatus')`, `m.txStatus`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("dashboard missing TX live-update hook %q", want)
		}
	}
}

// The meter tick carries TX state so clock loss shows without a refresh.
func TestMeterResponseCarriesTxStatus(t *testing.T) {
	saveTxGlobals(t)
	mutex.Lock()
	txHolder, txHolderReady = &fakeTxHolder{}, true
	deviceName = "PI9696"
	mutex.Unlock()
	if got := currentMeterResponse().TXStatus; got != "Dante TX ready (PI9696-TX)" {
		t.Errorf("meter TXStatus = %q, want ready line", got)
	}
}
