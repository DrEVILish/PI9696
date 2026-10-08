package main

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setTimecodeSettings sets the timecode settings for a test and restores
// them (and the readers' state) afterwards.
func setTimecodeSettings(t *testing.T, source, record int, output bool) {
	t.Helper()
	mutex.Lock()
	oS, oR, oO, oRate, oPeer := tcSourceIdx, tcRecordIdx, tcOutputOn, tcRateIdx, tcMTCPeer
	tcSourceIdx, tcRecordIdx, tcOutputOn, tcRateIdx = source, record, output, tcRateDefault
	tcSettingsChangedLocked()
	mutex.Unlock()
	resetTimecodeReaders()
	t.Cleanup(func() {
		mutex.Lock()
		tcSourceIdx, tcRecordIdx, tcOutputOn, tcRateIdx, tcMTCPeer = oS, oR, oO, oRate, oPeer
		tcSettingsChangedLocked()
		mutex.Unlock()
		resetTimecodeReaders()
	})
}

func resetTimecodeReaders() {
	tcMu.Lock()
	defer tcMu.Unlock()
	tcLTC.dec, tcLTC.has, tcLTC.run, tcLTC.stream = nil, false, 0, nil
	tcMTC.has, tcMTC.run, tcMTC.dec = false, 0, mtcDecoder{}
	tcPlay.active, tcPlay.owner = false, nil
}

// ltcStream renders frames of channels channels with LTC from start (25
// fps) on the last channel and an audible 1 kHz tone on the others.
func ltcStream(start Timecode, sr, channels int, pos int64, n int) []int32 {
	r := tcRates[tcRateDefault]
	e := newLTCEncoder(r, sr)
	base := samplesFromFrames(start.frames(r), r, sr)
	buf := make([]int32, n*channels)
	for i := 0; i < n; i++ {
		tone := int32(math.Sin(2*math.Pi*1000*float64(pos+int64(i))/float64(sr))*0.25*(1<<31)) &^ 0xFF
		for c := 0; c < channels-1; c++ {
			buf[i*channels+c] = tone
		}
		buf[i*channels+channels-1] = e.sample(base + pos + int64(i))
	}
	return buf
}

func TestLTCInputLocksAndStampsExactly(t *testing.T) {
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	start := Timecode{10, 0, 0, 0}
	stream := new(int)
	now := time.Now()
	const sr, chunk = 48000, 1024
	// 0.5 s in real-time chunks ending now.
	n := sr / 2 / chunk * chunk
	for pos := 0; pos < n; pos += chunk {
		at := now.Add(-time.Duration(n-pos-chunk) * time.Second / sr)
		tcFeedLTC(stream, ltcStream(start, sr, 3, int64(pos), chunk), 3, int64(pos), at, sr)
	}
	r := tcInputNow(now)
	if !r.locked || r.source != "LTC" || r.rate != tcRateDefault {
		t.Fatalf("not locked after 0.5 s of LTC: %+v", r)
	}
	// Now is stream position n: 10:00:00:00 + n samples.
	want := float64(start.frames(tcRates[tcRateDefault])) + float64(n)*25/sr
	if math.Abs(r.frames-want) > 0.1 {
		t.Fatalf("position now %.3f frames, want %.3f", r.frames, want)
	}
	// A take starting at stream position 12345 is stamped with that
	// sample's timecode, to the sample.
	stamp := tcStampTake(12345, true, sr, now)
	wantRef := samplesFromFrames(start.frames(tcRates[tcRateDefault]), tcRates[tcRateDefault], sr) + 12345
	if stamp.TimeReference != wantRef || stamp.Source != "LTC" || stamp.Start != "10:00:00:06" || stamp.Rate != "25" {
		t.Fatalf("stamp %+v, want time reference %d at 10:00:00:06", stamp, wantRef)
	}
	// Silence: the lock goes once the code is stale.
	if r := tcInputNow(now.Add(time.Second)); r.locked {
		t.Fatal("still locked a second after the code stopped")
	}
}

func TestStampFallsBackToTimeOfDay(t *testing.T) {
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	at := time.Date(2026, 10, 7, 13, 14, 15, 200_000_000, time.Local)
	stamp := tcStampTake(0, false, 48000, at)
	if stamp.Source != "time of day" || stamp.Start != "13:14:15:05" {
		t.Fatalf("stamp %+v", stamp)
	}
	if want := int64((13*3600 + 14*60 + 15.2) * 48000); stamp.TimeReference != want {
		t.Fatalf("time reference %d, want %d", stamp.TimeReference, want)
	}
}

func TestMTCInputLocks(t *testing.T) {
	setTimecodeSettings(t, tcSourceMTC, tcRecordMeta, false)
	r := tcRates[tcRateDefault]
	tc := Timecode{1, 2, 3, 0}
	for k := 0; k < 3; k++ {
		group := tc.add(int64(2*k), r)
		for p := 0; p < 8; p++ {
			tcHandleMIDI([]byte{0xF1, mtcQuarterFrame(p, group, r)}, "test")
		}
	}
	got := tcInputNow(time.Now())
	if !got.locked || got.source != "MTC" {
		t.Fatalf("MTC not locked: %+v", got)
	}
	// The last label was 01:02:03:04, completed 1.75 frames into it.
	if want := float64(tc.add(4, r).frames(r)) + 1.75; math.Abs(got.frames-want) > 0.2 {
		t.Fatalf("MTC position %.2f, want %.2f", got.frames, want)
	}
	// A full frame is a locate: seen, not running.
	tcHandleMIDI(mtcFullFrame(Timecode{5, 0, 0, 0}, r), "test")
	if got := tcInputNow(time.Now()); got.locked || !got.seen {
		t.Fatalf("after a full frame: %+v", got)
	}
}

func TestOverallLevels(t *testing.T) {
	p, r := overallLevels([]float64{-12, -6, -100}, []float64{-20, -20, meterSilence})
	if p != -6 {
		t.Fatalf("peak %v", p)
	}
	// Two channels at -20 dB and one silent: mean power is 2/3 of -20 dB.
	if want := -20 + 10*math.Log10(2.0/3); math.Abs(r-want) > 0.01 {
		t.Fatalf("rms %v, want %v", r, want)
	}
}

func TestMeterReaderOwnOverallIgnoresTimecode(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	meterGen++
	gen := meterGen
	meterChannelPeak = []float64{meterSilence, meterSilence}
	meterChannelRMS = []float64{meterSilence, meterSilence}
	mutex.Unlock()
	meterReaderOverall(strings.NewReader("frame:0\n"+
		"lavfi.astats.1.Peak_level=-20\nlavfi.astats.1.RMS_level=-30\n"+
		"lavfi.astats.2.Peak_level=-18\nlavfi.astats.2.RMS_level=-30\n"+
		"lavfi.astats.3.Peak_level=-6\nlavfi.astats.3.RMS_level=-9\n"+
		"lavfi.astats.Overall.Peak_level=-6\nlavfi.astats.Overall.RMS_level=-9\n"), gen, true)
	mutex.Lock()
	defer mutex.Unlock()
	if meterPeakDB != -18 || math.Abs(meterRMSDB+30) > 0.001 {
		t.Fatalf("overall %v/%v includes the TIMECODE channel", meterPeakDB, meterRMSDB)
	}
}

// decodeLTCChannel decodes channel ch of interleaved frames.
func decodeLTCChannel(frames []int32, stride, ch, sr int) []ltcDecoded {
	var out []ltcDecoded
	newLTCDecoder(sr).feed(frames, stride, ch, func(f ltcDecoded) { out = append(out, f) })
	return out
}

// The pump puts the take's timecode on TIMECODE while the output is on:
// the take's start plus the position, to the sample.
func TestPumpGeneratesTakeTimecode(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	setTimecodeSettings(t, tcSourceOff, tcRecordMeta, true)
	const sr = 48000
	r := tcRates[tcRateDefault]
	ref := samplesFromFrames((Timecode{11, 0, 0, 0}).frames(r), r, sr)
	raw := make([]byte, sr*2*4) // 1 s of 2-channel silence
	holder := &fakeTxHolder{}
	cmd := exec.Command("true")
	mutex.Lock()
	playbackCmd, txHolder = cmd, holder
	currentState = StatePlaying
	mutex.Unlock()
	p := txPlayout{fileChannels: 2, txChannels: 3, sampleRate: sr, startFrame: sr / 2, tcRef: ref, tcRate: tcRateDefault}
	pumpPlaybackToTx(cmd, &chunkReader{data: raw, chunk: 4096}, holder, p)
	got := decodeLTCChannel(holder.flattened(), 3, 2, sr)
	if len(got) < 20 {
		t.Fatalf("decoded %d frames", len(got))
	}
	// Played from 0.5 s in: the first whole frame is 11:00:00:13 (or :14).
	first := got[0].tc
	if first != (Timecode{11, 0, 0, 13}) && first != (Timecode{11, 0, 0, 14}) {
		t.Fatalf("first frame out %v", first)
	}
	for i := 1; i < len(got); i++ {
		if got[i].tc != got[i-1].tc.add(1, r) {
			t.Fatalf("frame %d: %v after %v", i, got[i].tc, got[i-1].tc)
		}
	}
	// Each frame ends where its label says, relative to the take start.
	f := got[0]
	wantEnd := samplesFromFrames(f.tc.frames(r)+1, r, sr) - ref - p.startFrame
	if d := f.endPos - wantEnd; d < -2 || d > 2 {
		t.Fatalf("frame %v ends at stream sample %d, want %d", f.tc, f.endPos, wantEnd)
	}
}

func TestIdleFeederTimecodeIsContinuous(t *testing.T) {
	setTimecodeSettings(t, tcSourceOff, tcRecordMeta, true)
	const sr = 48000
	// At the device's pace (writes block once its ring is full): the clock
	// moves one chunk per write.
	clock := time.Now()
	g := tcIdleGen{clock: func() time.Time { return clock }}
	var all []int32
	buf := make([]int32, txPumpFrames*3)
	for i := 0; i < 50; i++ {
		g.fill(buf, 3, sr, ringHolder{8192})
		all = append(all, buf...)
		clock = clock.Add(time.Duration(txPumpFrames) * time.Second / sr)
	}
	got := decodeLTCChannel(all, 3, 2, sr)
	r := tcRates[tcRateDefault]
	if len(got) < 25 {
		t.Fatalf("decoded %d frames", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].tc != got[i-1].tc.add(1, r) {
			t.Fatalf("frame %d: %v after %v", i, got[i].tc, got[i-1].tc)
		}
	}
	// It is the time of day (plus the ring), give or take a second.
	now := tcTimeOfDayFrames(time.Now(), r)
	if d := float64(got[0].tc.frames(r)) - now; d < -50 || d > 25 {
		t.Fatalf("idle code %v is %.0f frames off the time of day", got[0].tc, d)
	}
	// Off: silence on TIMECODE.
	setTimecodeSettings(t, tcSourceOff, tcRecordMeta, false)
	for i := range buf {
		buf[i] = 1
	}
	g.fill(buf, 3, sr, ringHolder{8192})
	for i := 2; i < len(buf); i += 3 {
		if buf[i] != 0 {
			t.Fatal("TIMECODE not silent with the output off")
		}
	}
}

func TestSidecarKeepsTimecodeAcrossRenames(t *testing.T) {
	wav := filepath.Join(t.TempDir(), "take_ch2_48kHz.wav")
	os.WriteFile(wav, nil, 0o644)
	tc := &recTimecode{Start: "10:00:00:00", Rate: "25", Source: "LTC", TimeReference: 1728000000, SampleRate: 48000}
	mutex.Lock()
	snapshotRecordingChannels(wav, 2, tc)
	mutex.Unlock()
	if err := renameRecordingChannels(wav, map[int]string{1: "Kick"}); err != nil {
		t.Fatal(err)
	}
	f, err := readRecordingSidecar(wav)
	if err != nil || f.Timecode == nil || *f.Timecode != *tc || f.Channels[0].Name != "Kick" {
		t.Fatalf("sidecar after rename: %+v %v", f, err)
	}
	ref, rate, ok := takeTimecode(wav, 96000)
	if !ok || ref != 2*1728000000 || rate != tcRateDefault {
		t.Fatalf("takeTimecode at 96 kHz = %d %d %v", ref, rate, ok)
	}
}

// writeTestWAV writes a minimal 16-bit WAV with an optional bext chunk.
func writeTestWAV(t *testing.T, path string, timeRef int64, bext bool) {
	t.Helper()
	var b []byte
	b = append(b, "RIFF\x00\x00\x00\x00WAVE"...)
	b = append(b, "fmt \x10\x00\x00\x00"...)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 48000)
	b = binary.LittleEndian.AppendUint32(b, 96000)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	if bext {
		chunk := make([]byte, 602)
		binary.LittleEndian.PutUint64(chunk[338:], uint64(timeRef))
		b = append(b, "bext"...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(chunk)))
		b = append(b, chunk...)
	}
	b = append(b, "data\x04\x00\x00\x00\x00\x00\x00\x00"...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWAVTimeReference(t *testing.T) {
	dir := t.TempDir()
	with, without := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
	writeTestWAV(t, with, 3600*48000, true)
	writeTestWAV(t, without, 0, false)
	if ref, err := wavTimeReference(with); err != nil || ref != 3600*48000 {
		t.Fatalf("with bext: %d %v", ref, err)
	}
	if _, err := wavTimeReference(without); err == nil {
		t.Fatal("found a time reference in a WAV without bext")
	}
	// ffmpeg writes what bextArgs asks for, and takeTimecode reads it back
	// for a take without a sidecar.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	out := filepath.Join(dir, "rec_ch1_48kHz.wav")
	stamp := &recTimecode{Start: "01:00:00:00", Rate: "25", Source: "LTC", TimeReference: 3600 * 48000, SampleRate: 48000}
	args := append([]string{"-v", "error", "-f", "lavfi", "-i", "sine=f=1000:d=0.2", "-c:a", "pcm_s24le", "-rf64", "auto"}, stamp.bextArgs()...)
	if o, err := exec.Command("ffmpeg", append(args, out)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, o)
	}
	if ref, _, ok := takeTimecode(out, 48000); !ok || ref != 3600*48000 {
		t.Fatalf("take time reference %d %v", ref, ok)
	}
}

// ltcDevice is an inferno stand-in whose last channel carries LTC from
// 10:00:00:00 and the others a 1 kHz tone, at real-time pace.
type ltcDevice struct {
	fakeTxHolder
	channels, rate int
	mu             sync.Mutex
	pos            int64
	done           chan struct{}
	once           sync.Once
}

func (d *ltcDevice) Read(buf []int32) (int, error) {
	select {
	case <-d.done:
		return 0, os.ErrClosed
	case <-time.After(10 * time.Millisecond):
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := min(len(buf)/d.channels, d.rate/100)
	copy(buf, ltcStream(Timecode{10, 0, 0, 0}, d.rate, d.channels, d.pos, n))
	d.pos += int64(n)
	return n, nil
}

func (d *ltcDevice) Close() error {
	d.once.Do(func() { close(d.done) })
	return nil
}

func (d *ltcDevice) Xruns() int64 { return 0 }

// A take that records timecode as audio has the TIMECODE channel last, and
// its metadata names the timecode of its first sample - which the recorded
// code itself confirms, to the sample. Afterwards the FIFO is back to the
// audio channels.
func TestRecordTimecodeAsAudioAndMetadata(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	initTestHardware(t)
	saveTxGlobals(t)
	useFakeInferno(t)
	setTimecodeSettings(t, tcSourceLTC, tcRecordAudio, false)
	var dev *ltcDevice
	openPairedDevice = func(rate, channels int) (pairedDevice, error) {
		dev = &ltcDevice{channels: channels, rate: rate, done: make(chan struct{})}
		return dev, nil
	}
	mutex.Lock()
	demoMode = false
	sampleRateIdx, channelCount = 1, 2
	currentState = StateIdle
	mutex.Unlock()
	t.Cleanup(stopInfernoAndWait)
	startDone := make(chan struct{})
	infernoReqCh <- infernoRequest{cmd: infernoCmdStart, done: startDone}
	<-startDone
	waitFor(t, 3*time.Second, "LTC lock", func() bool { return tcInputNow(time.Now()).locked })

	mutex.Lock()
	startRecording()
	file, rec := recordingFile, isRecording
	mutex.Unlock()
	if !rec {
		t.Fatal("take did not start")
	}
	t.Cleanup(func() { os.Remove(file); os.Remove(channelsSidecar(file)) })
	if !strings.Contains(filepath.Base(file), "_ch3_") {
		t.Fatalf("take %s should be named for 3 channels", file)
	}
	time.Sleep(1500 * time.Millisecond)
	mutex.Lock()
	stopRecording()
	mutex.Unlock()
	waitFor(t, 5*time.Second, "take finished", func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return !isRecording && monitoring
	})

	sc, err := readRecordingSidecar(file)
	if err != nil || sc.Timecode == nil || sc.Timecode.Source != "LTC" || len(sc.Channels) != 3 || sc.Channels[2].Name != "TIMECODE" {
		t.Fatalf("sidecar %+v %v", sc, err)
	}
	ref, err := wavTimeReference(file)
	if err != nil || ref != sc.Timecode.TimeReference {
		t.Fatalf("bext time reference %d (%v), sidecar %d", ref, err, sc.Timecode.TimeReference)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(data), "data")
	pcm := data[i+8:]
	frames := make([]int32, len(pcm)/3)
	for k := range frames {
		frames[k] = int32(uint32(pcm[3*k])<<8|uint32(pcm[3*k+1])<<16|uint32(pcm[3*k+2])<<24) &^ 0xFF
	}
	got := decodeLTCChannel(frames, 3, 2, 48000)
	if len(got) < 20 {
		t.Fatalf("decoded %d frames from the take's TIMECODE channel", len(got))
	}
	r := tcRates[tcRateDefault]
	// The frame that ends at take sample e started at its label's time.
	f := got[0]
	recorded := samplesFromFrames(f.tc.frames(r)+1, r, 48000) - f.endPos
	if d := recorded - ref; d < -2 || d > 2 {
		t.Fatalf("the recorded code puts the take start at %d, the metadata at %d", recorded, ref)
	}
	// The audio channels hold the tone, not the code.
	if frames[0] == frames[2] && frames[3] == frames[5] {
		t.Fatal("audio channel looks like the code")
	}
	mutex.Lock()
	audio := tcAudioTake
	mutex.Unlock()
	if audio {
		t.Fatal("still in the timecode-as-audio layout after the take")
	}
}

// A code that jumps mid-frame: the new run starts at the new code's first
// frame, whether or not the decoder caught that frame.
func TestLTCRunStartAfterAJump(t *testing.T) {
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	const sr = 48000
	r := tcRates[tcRateDefault]
	for _, cut := range []int{700, 1000, 1500, 1900} {
		resetTimecodeReaders()
		a := Timecode{10, 0, 0, 0}
		b := Timecode{5, 0, 0, 0}
		// 1 s of A, then A's next frame cut after cut samples, then B
		// from its first frame.
		buf := append(ltcStream(a, sr, 1, 0, sr+cut)[:sr+cut], ltcStream(b, sr, 1, 0, sr)...)
		stream := new(int)
		now := time.Now()
		for pos := 0; pos+1024 <= len(buf); pos += 1024 {
			tcFeedLTC(stream, buf[pos:pos+1024], 1, int64(pos), now, sr)
		}
		tcMu.Lock()
		got, run := tcLTC.runStart, tcLTC.run
		tcMu.Unlock()
		if run < tcLockFrames || got != b.frames(r) {
			t.Errorf("cut %d samples into a frame: run of %d from %v, want from %v", cut, run, timecodeAt(got, r), b)
		}
	}
}
