package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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
	origCfg, origRecs := lastPanelConfig, lastPanelRecs
	teleWSHub = map[*websocket.Conn]bool{}
	lastPanelConfig, lastPanelRecs = "", ""
	t.Cleanup(func() {
		teleWSMu.Lock()
		teleWSHub = origHub
		lastPanelConfig, lastPanelRecs = origCfg, origRecs
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
	mutex.Unlock()
	if !fresh {
		t.Fatalf("serving the login page must mark the OLED token fresh")
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
	mutex.Unlock()
	if !readFlag() {
		t.Fatal("demo mode true did not persist")
	}
	mutex.Lock()
	setDemoModeLocked(false)
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
	for _, want := range []string{`class="reel-g"`, `id="tapePath"`, `classList.toggle('spinning'`, `classList.toggle('active'`, `@keyframes spin`, `--ftl-deck-face`, `--ftl-deck-trim`} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard missing deck-animation hook %q", want)
		}
	}
}

func TestEnlargeFifoGrowsPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fifo")
	if err := syscall.Mkfifo(path, 0666); err != nil {
		t.Fatal(err)
	}
	// Growing a pipe needs privilege the test sandbox lacks (EPERM here,
	// succeeds as root on the unit): probe first and skip where the
	// kernel refuses, so the test still verifies growth on target.
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, perr := syscall.Syscall(syscall.SYS_FCNTL, probe.Fd(), linuxFSetPipeSz, 4<<20)
	probe.Close()
	if perr == syscall.EPERM {
		t.Skip("sandbox denies F_SETPIPE_SZ; growth verified on target")
	}
	pipeSize := func() int {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r1, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), linuxFGetPipeSz, 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		return int(r1)
	}
	before := pipeSize()
	enlargeFifo(path)
	if got := pipeSize(); got <= before {
		t.Errorf("pipe size %d, want growth past default %d", got, before)
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
