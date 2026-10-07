package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pi9696/alsapcm"
	"pi9696/hardware"
)

// The timecode engine: what the unit reads, writes and stamps.
//
//   - Input (sync in): SMPTE LTC on the TIMECODE receive channel, or MTC
//     over IP (RTP-MIDI). The selected source is "locked" once a few
//     consecutive frames have arrived and stays so while they keep coming.
//   - Output (sync out): LTC on the TIMECODE transmit channel and MTC to
//     every RTP-MIDI peer. It follows the transport: the take's own
//     timecode while playing, otherwise the incoming timecode when locked
//     (a relay) or the time of day.
//   - Takes: the timecode of a take's first sample is recorded as metadata
//     (the BWF time reference, plus the sidecar), and optionally the
//     TIMECODE channel itself as the take's last channel.
//
// Settings live under the app mutex like every other setting; the capture
// loop and the transmitter read their atomic mirrors (tcLive) and never
// take the app mutex.

const (
	tcSourceOff = iota
	tcSourceLTC
	tcSourceMTC
)

var tcSourceNames = []string{"Off", "LTC", "MTC"}

const (
	tcRecordOff = iota
	tcRecordMeta
	tcRecordAudio
)

var tcRecordNames = []string{"Off", "Metadata", "Audio"}

// Timecode settings (persisted; guarded by the app mutex).
var (
	tcSourceIdx = tcSourceOff
	tcRecordIdx = tcRecordMeta
	tcOutputOn  bool
	tcRateIdx   = tcRateDefault
	tcMTCPeer   string // RTP-MIDI peer to invite ("" none): host or host:port
)

// tcLive mirrors the settings for the audio threads.
var tcLive struct {
	source atomic.Int32
	output atomic.Bool
	rate   atomic.Int32
}

func init() { tcLive.rate.Store(tcRateDefault) }

// tcSettingsChangedLocked publishes the settings to tcLive. Callers hold
// the app mutex.
func tcSettingsChangedLocked() {
	tcLive.source.Store(int32(tcSourceIdx))
	tcLive.output.Store(tcOutputOn)
	tcLive.rate.Store(int32(tcRateIdx))
	select {
	case tcServicesKick <- struct{}{}:
	default:
	}
}

// applyTimecodeConfig loads the timecode settings from a config; unknown or
// absent values keep the current ones. Caller holds the app mutex.
func applyTimecodeConfig(c *PersistedConfig) {
	if i := indexFold(tcSourceNames, c.TCSource); i >= 0 {
		tcSourceIdx = i
	}
	if i := indexFold(tcRecordNames, c.TCRecord); i >= 0 {
		tcRecordIdx = i
	}
	tcOutputOn = c.TCOutput
	for i, r := range tcRates {
		if r.Name == c.TCRate {
			tcRateIdx = i
		}
	}
	if c.TCMTCPeer == "" || validMTCPeer(c.TCMTCPeer) {
		tcMTCPeer = c.TCMTCPeer
	}
	tcSettingsChangedLocked()
}

func indexFold(list []string, s string) int {
	for i, v := range list {
		if s != "" && strings.EqualFold(v, s) {
			return i
		}
	}
	return -1
}

// validMTCPeer accepts a host name or address, optionally with :port.
func validMTCPeer(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	host := s
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65534 {
			return false
		}
		host = h
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':') {
			return false
		}
	}
	return host != ""
}

// tcOutputRate is the rate the generator runs at when nothing else says.
func tcOutputRate() int {
	return int(tcLive.rate.Load())
}

// --- Input -----------------------------------------------------------------

// tcLockFrames is how many consecutive frames make a lock.
const tcLockFrames = 3

// tcStaleAfter: a source that has sent nothing for this long has stopped.
// LTC sends every frame (33-42 ms), MTC completes a label every two.
const tcStaleAfter = 250 * time.Millisecond

var tcMu sync.Mutex

// tcLTC is the LTC reader's state (tcMu).
var tcLTC struct {
	dec    *ltcDecoder
	stream any   // which writer feeds the decoder (capture loop, demo)
	sr     int   // its sample rate
	next   int64 // the stream position expected next
	// The last frame: next is the frame count of the frame after it
	// (where the source was when it ended), at stream position endPos and
	// wall time wall.
	has        bool
	nextFrames int64
	endPos     int64
	wall       time.Time
	rate       int
	run        int // consecutive frames
}

// tcMTC is the MTC reader's state (tcMu).
var tcMTC struct {
	dec     mtcDecoder
	has     bool
	frames  float64 // position when the last label completed
	wall    time.Time
	rate    int
	running bool
	run     int
	from    string
}

// tcRateForDecoded resolves a decoded frame's rate: the decoder's reading
// once it has one, else the output rate setting when that fits the drop
// flag, else the usual rate for the flag.
func tcRateForDecoded(read int, drop bool) int {
	if read >= 0 {
		return read
	}
	if r := tcOutputRate(); tcRates[r].Drop == drop {
		return r
	}
	if drop {
		return tcRateFor(30, true, true)
	}
	return tcRateDefault
}

// tcFeedLTC hands frames from the TIMECODE channel (the last of stride
// interleaved channels) to the LTC reader. first is the stream position of
// the first frame, readAt when the chunk arrived (its last frame's time).
func tcFeedLTC(stream any, frames []int32, stride int, first int64, readAt time.Time, sr int) {
	n := int64(len(frames) / stride)
	tcMu.Lock()
	defer tcMu.Unlock()
	l := &tcLTC
	if l.dec == nil || l.stream != stream || l.sr != sr || l.next != first {
		l.dec, l.stream, l.sr = newLTCDecoder(sr), stream, sr
		l.dec.pos = first
		l.run = 0
	}
	end := first + n
	l.dec.feed(frames, stride, stride-1, func(f ltcDecoded) {
		rate := tcRateForDecoded(f.rate, f.drop)
		next := f.tc.frames(tcRates[rate]) + 1
		if l.has && next == l.nextFrames+1 && rate == l.rate {
			l.run++
		} else {
			l.run = 1
		}
		l.has, l.nextFrames, l.endPos, l.rate = true, next, f.endPos, rate
		l.wall = readAt.Add(-time.Duration(end-f.endPos) * time.Second / time.Duration(sr))
	})
	l.next = end
}

// tcHandleMIDI takes one MIDI message from an RTP-MIDI peer.
func tcHandleMIDI(msg []byte, from string) {
	if len(msg) == 0 {
		return
	}
	now := time.Now()
	tcMu.Lock()
	defer tcMu.Unlock()
	m := &tcMTC
	var d mtcDecoded
	var ok bool
	switch msg[0] {
	case 0xF1:
		if len(msg) == 2 {
			d, ok = m.dec.quarterFrame(msg[1])
		}
	case 0xF0:
		d, ok = m.dec.sysEx(msg)
	}
	if !ok {
		return
	}
	frames := float64(d.tc.frames(tcRates[d.rate])) + d.offsetFrames
	if d.running && m.has && m.running && m.rate == d.rate && math.Abs(frames-m.frames-2) < 0.01 {
		m.run++
	} else if d.running {
		m.run = 1
	} else {
		m.run = 0
	}
	m.has, m.frames, m.wall, m.rate, m.running, m.from = true, frames, now, d.rate, d.running, from
}

// tcReading is the selected input at one moment.
type tcReading struct {
	source string
	frames float64 // position in frames since midnight
	rate   int
	locked bool
	// stopped: a source that was seen but is not running (MTC parked at a
	// full frame, LTC that stopped): frames holds where it stopped.
	seen bool
}

// tcInputNow is the selected input as of now: locked with the position
// extrapolated to now, or not.
func tcInputNow(now time.Time) tcReading {
	src := int(tcLive.source.Load())
	tcMu.Lock()
	defer tcMu.Unlock()
	switch src {
	case tcSourceLTC:
		l := &tcLTC
		r := tcReading{source: "LTC", rate: l.rate, seen: l.has}
		if !l.has {
			return r
		}
		age := now.Sub(l.wall)
		r.frames = float64(l.nextFrames)
		if l.run >= tcLockFrames && age < tcStaleAfter {
			r.locked = true
			r.frames += tcFramesOfSeconds(age.Seconds(), tcRates[l.rate])
		}
		return r
	case tcSourceMTC:
		m := &tcMTC
		r := tcReading{source: "MTC", rate: m.rate, seen: m.has, frames: m.frames}
		if m.has && m.running && m.run >= 2 && now.Sub(m.wall) < tcStaleAfter {
			r.locked = true
			r.frames += tcFramesOfSeconds(now.Sub(m.wall).Seconds(), tcRates[m.rate])
		}
		return r
	}
	return tcReading{}
}

// tcInputAtStream is the selected input's position at a capture stream
// position (the take's first sample): exact for LTC, which arrives on the
// same stream, and now-based for MTC.
func tcInputAtStream(pos int64, now time.Time) tcReading {
	r := tcInputNow(now)
	if !r.locked || r.source != "LTC" {
		return r
	}
	tcMu.Lock()
	defer tcMu.Unlock()
	if tcLTC.sr > 0 {
		r.frames = float64(tcLTC.nextFrames) + tcFramesOfSeconds(float64(pos-tcLTC.endPos)/float64(tcLTC.sr), tcRates[tcLTC.rate])
	}
	return r
}

// tcTimeOfDayFrames is the time of day at t as frames since local midnight.
func tcTimeOfDayFrames(t time.Time, r tcRate) float64 {
	y, mo, d := t.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
	return tcFramesOfSeconds(t.Sub(midnight).Seconds(), r)
}

// tcFreeRun is what the generator sends when the transport isn't playing:
// the incoming timecode when locked, else the time of day, at time t.
func tcFreeRun(t time.Time) (frames float64, rate int) {
	if r := tcInputNow(time.Now()); r.locked {
		return r.frames + tcFramesOfSeconds(time.Until(t).Seconds(), tcRates[r.rate]), r.rate
	}
	rate = tcOutputRate()
	return tcTimeOfDayFrames(t, tcRates[rate]), rate
}

// --- Stamping takes -----------------------------------------------------------

// recTimecode is a take's timecode: the label and real position of its
// first sample. Stored in the take's sidecar; the BWF time reference holds
// the same position in samples.
type recTimecode struct {
	Start         string `json:"start"`         // hh:mm:ss:ff (;ff for drop frame)
	Rate          string `json:"rate"`          // a tcRates name
	Source        string `json:"source"`        // LTC, MTC or "time of day"
	TimeReference int64  `json:"timeReference"` // samples since midnight at the take's rate
	SampleRate    int    `json:"sampleRate"`
}

// rateIndex is the stamp's rate as a tcRates index (-1 unknown).
func (t *recTimecode) rateIndex() int {
	for i, r := range tcRates {
		if r.Name == t.Rate {
			return i
		}
	}
	return -1
}

// tcAudioTake is true while a take records the TIMECODE channel as audio:
// the FIFO carries every channel until it ends (app mutex).
var tcAudioTake bool

// tcEndAudioTakeLocked puts the FIFO back to the audio channels after a
// take that recorded timecode as audio (or failed to start). The take's
// ffmpeg is gone, so nothing reads the FIFO across the switch. Caller
// holds the app mutex.
func tcEndAudioTakeLocked() {
	if !tcAudioTake {
		return
	}
	tcAudioTake = false
	if _, ok := fifoSetLayout(false); !ok {
		logWarnf("Timecode: the audio input did not switch back from the TIMECODE channel")
	}
}

// tcStampTake is the timecode of a take whose first sample is capture
// stream position pos (posOK false when the position is unknown: then
// "now"), at sampleRate.
func tcStampTake(pos int64, posOK bool, sampleRate int, now time.Time) *recTimecode {
	var r tcReading
	if posOK {
		r = tcInputAtStream(pos, now)
	} else {
		r = tcInputNow(now)
	}
	src := r.source
	if !r.locked {
		r.rate = tcOutputRate()
		r.frames = tcTimeOfDayFrames(now, tcRates[r.rate])
		src = "time of day"
	}
	rate := tcRates[r.rate]
	return &recTimecode{
		Start:         timecodeAt(int64(math.Floor(r.frames)), rate).format(rate),
		Rate:          rate.Name,
		Source:        src,
		TimeReference: int64(math.Round(tcSecondsOfFrames(r.frames, rate) * float64(sampleRate))),
		SampleRate:    sampleRate,
	}
}

// bextArgs are ffmpeg's arguments for a take's BWF chunk carrying stamp.
func (t *recTimecode) bextArgs() []string {
	return []string{
		"-write_bext", "1",
		"-metadata", fmt.Sprintf("time_reference=%d", t.TimeReference),
		"-metadata", fmt.Sprintf("description=Timecode %s @ %s fps (%s)", t.Start, t.Rate, t.Source),
		"-metadata", "originator=pi9696",
	}
}

// takeTimecode is a take's start as a time reference (samples since
// midnight at sampleRate) and rate: from its sidecar, else its BWF chunk
// (takes from elsewhere; read at the output rate), else midnight - a take
// without timecode starts at 00:00:00:00, as a DAW places one.
func takeTimecode(wav string, sampleRate int) (timeRef int64, rate int, ok bool) {
	if f, err := readRecordingSidecar(wav); err == nil && f.Timecode != nil {
		if r := f.Timecode.rateIndex(); r >= 0 && f.Timecode.SampleRate > 0 {
			return f.Timecode.TimeReference * int64(sampleRate) / int64(f.Timecode.SampleRate), r, true
		}
	}
	if ref, err := wavTimeReference(wav); err == nil {
		return ref, tcOutputRate(), true
	}
	return 0, tcOutputRate(), false
}

// --- Output ------------------------------------------------------------------

// txRingFrames is the plugin's playback ring at rate: once full, a frame
// written now is heard this many frames later.
func txRingFrames(rate int) int {
	ring := 1
	for ring < rate*alsapcm.LatencyUs/1_000_000 {
		ring <<= 1
	}
	return ring
}

// tcIdleGen writes the free-running LTC (tcFreeRun) for the idle feeder.
// It keeps its own sample count, so the code is continuous, and relocates
// when that drifts more than half a frame from where it should be.
type tcIdleGen struct {
	enc   *ltcEncoder
	cur   int64
	valid bool
	clock func() time.Time // nil: time.Now (a seam for tests)
}

// fill writes the TIMECODE channel of buf (frames of channels) for the
// next chunk, or silence when the output is off.
func (g *tcIdleGen) fill(buf []int32, channels, sampleRate int) {
	if !tcLive.output.Load() || channels < 2 {
		g.valid = false
		for i := channels - 1; i < len(buf); i += channels {
			buf[i] = 0
		}
		return
	}
	now := time.Now
	if g.clock != nil {
		now = g.clock
	}
	at := now().Add(time.Duration(txRingFrames(sampleRate)) * time.Second / time.Duration(sampleRate))
	frames, rate := tcFreeRun(at)
	r := tcRates[rate]
	target := int64(tcSecondsOfFrames(frames, r) * float64(sampleRate))
	spf := int64(float64(sampleRate) * float64(r.Den) / float64(r.Num))
	if g.enc == nil || g.enc.rate != r || g.enc.sampleRate != sampleRate {
		g.enc, g.valid = newLTCEncoder(r, sampleRate), false
	}
	if !g.valid || g.cur-target > spf/2 || target-g.cur > spf/2 {
		g.cur, g.valid = target, true
	}
	g.enc.fill(buf, channels, channels-1, g.cur)
	g.cur += int64(len(buf) / channels)
}

// tcPlay is the playing take's timecode position, published by the pump
// for the MTC sender and the chase (tcMu).
var tcPlay struct {
	active  bool
	timeRef int64 // the take's start, samples since midnight
	rate    int
	sr      int
	written int64 // file frame the pump writes next
	at      time.Time
	ring    int64 // frames between writing and hearing
	paused  bool
	owner   any // the pump publishing (its ffmpeg); a retired one leaves it be
}

// tcPlayPosition is the take position (file frames) heard now, from the
// pump's counters. ok is false when no pump is publishing.
func tcPlayPosition(now time.Time) (frame float64, timeRef int64, rate, sr int, ok bool) {
	tcMu.Lock()
	defer tcMu.Unlock()
	p := &tcPlay
	if !p.active || p.sr == 0 {
		return 0, 0, 0, 0, false
	}
	f := float64(p.written - p.ring)
	if !p.paused {
		// The pump writes in bursts; between them the ring drains in real
		// time, at most a chunk's worth.
		f += math.Min(now.Sub(p.at).Seconds()*float64(p.sr), txPumpFrames)
	}
	return math.Max(0, f), p.timeRef, p.rate, p.sr, true
}

// tcOutputNow is the generator's position now (frames since midnight) and
// whether it is running: the playing take's timecode, the free run, or
// nothing while paused.
func tcOutputNow(now time.Time) (frames float64, rate int, running bool) {
	if f, ref, rate, sr, ok := tcPlayPosition(now); ok {
		tcMu.Lock()
		paused := tcPlay.paused
		tcMu.Unlock()
		r := tcRates[rate]
		return tcFramesOfSeconds((float64(ref)+f)/float64(sr), r), rate, !paused
	}
	frames, rate = tcFreeRun(now)
	return frames, rate, true
}

// --- MTC out, RTP-MIDI and the background loop --------------------------------

// tcServicesKick wakes tcServicesLoop after a settings change.
var tcServicesKick = make(chan struct{}, 1)

// tcSession is the RTP-MIDI session while MTC is in use (tcMu for the
// pointer; the session has its own locking).
var tcSession *rtpMIDISession

// tcServicesLoop runs the RTP-MIDI session while MTC is the source or the
// output is on, and logs the input's lock and loss. It never returns.
func tcServicesLoop() {
	var avahi *exec.Cmd
	var invited string
	lastLocked := false
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-tcServicesKick:
		}
		mutex.Lock()
		wantMTC := tcSourceIdx == tcSourceMTC || tcOutputOn
		name := sanitizeDanteName(deviceName)
		peer := tcMTCPeer
		mutex.Unlock()

		tcMu.Lock()
		s := tcSession
		tcMu.Unlock()
		switch {
		case wantMTC && s == nil:
			ns, err := startRTPMIDI(name, rtpMIDIPort, tcHandleMIDI, nil)
			if err != nil {
				logErrorf("Timecode: cannot open the RTP-MIDI session on port %d: %v", rtpMIDIPort, err)
				break
			}
			tcMu.Lock()
			tcSession = ns
			tcMu.Unlock()
			logInfof("Timecode: RTP-MIDI session %q on UDP %d/%d", name, rtpMIDIPort, rtpMIDIPort+1)
			go tcMTCOutLoop(ns)
			invited = ""
			if !isSimMode() {
				avahi = exec.Command("avahi-publish-service", name, "_apple-midi._udp", fmt.Sprint(rtpMIDIPort))
				if err := avahi.Start(); err != nil {
					logWarnf("Timecode: cannot advertise the RTP-MIDI session: %v", err)
					avahi = nil
				} else {
					go avahi.Wait()
				}
			}
			s = ns
		case !wantMTC && s != nil:
			tcMu.Lock()
			tcSession = nil
			tcMu.Unlock()
			s.close()
			if avahi != nil && avahi.Process != nil {
				avahi.Process.Kill()
				avahi = nil
			}
			logInfof("Timecode: RTP-MIDI session closed")
			s = nil
		}
		if s != nil && peer != invited {
			s.setInvite(peer)
			if peer != "" {
				logInfof("Timecode: inviting RTP-MIDI peer %s", peer)
			}
			invited = peer
		}

		r := tcInputNow(time.Now())
		if r.locked != lastLocked && r.source != "" {
			rate := tcRates[r.rate]
			label := timecodeAt(int64(r.frames), rate).format(rate)
			if r.locked {
				logInfof("Timecode in: %s locked at %s (%s fps)", r.source, label, rate.Name)
			} else {
				logWarnf("Timecode in: %s lost at %s", r.source, label)
			}
		}
		lastLocked = r.locked && r.source != ""
	}
}

// tcMTCOutLoop sends MTC quarter frames to the session's peers while the
// output is on: four per frame, following tcOutputNow. A jump (a locate,
// a new take) sends a full frame first; stopping sends one where it
// stopped.
func tcMTCOutLoop(s *rtpMIDISession) {
	lastQ := int64(-1)
	wasRunning := false
	for {
		select {
		case <-s.quit:
			return
		default:
		}
		if !tcLive.output.Load() || len(s.peerNames()) == 0 {
			lastQ, wasRunning = -1, false
			time.Sleep(100 * time.Millisecond)
			continue
		}
		frames, rate, running := tcOutputNow(time.Now())
		r := tcRates[rate]
		if !running {
			if wasRunning {
				s.send(mtcFullFrame(timecodeAt(int64(frames), r), r))
			}
			lastQ, wasRunning = -1, false
			time.Sleep(20 * time.Millisecond)
			continue
		}
		wasRunning = true
		q := int64(math.Floor(frames * 4))
		if lastQ < 0 || q < lastQ || q-lastQ > 8 {
			s.send(mtcFullFrame(timecodeAt(int64(frames), r), r))
			lastQ = q
		}
		for ; lastQ < q; lastQ++ {
			next := lastQ + 1
			group := timecodeAt(next/8*2, r)
			s.send([]byte{0xF1, mtcQuarterFrame(int(next%8), group, r)})
		}
		// Sleep to the next quarter frame.
		wait := tcSecondsOfFrames(float64(q+1)/4-frames, r)
		time.Sleep(time.Duration(math.Max(wait, 0.001) * float64(time.Second)))
	}
}

// --- Status for the UIs ----------------------------------------------------------

// tcStatusText is the input's one-line state: "LTC 10:00:00:12 25" when
// locked, "LTC no signal", or "off".
func tcStatusText() (short, long string) {
	r := tcInputNow(time.Now())
	if r.source == "" {
		return "off", "Timecode input off"
	}
	if !r.locked {
		return r.source + " --:--:--:--", r.source + ": no timecode"
	}
	rate := tcRates[r.rate]
	label := timecodeAt(int64(r.frames), rate).format(rate)
	return r.source + " " + label, fmt.Sprintf("%s %s @ %s fps", r.source, label, rate.Name)
}

// tcOutputText is the generator's current label, "" when off or stopped.
func tcOutputText() string {
	if !tcLive.output.Load() {
		return ""
	}
	frames, rate, running := tcOutputNow(time.Now())
	if !running {
		return ""
	}
	r := tcRates[rate]
	return timecodeAt(int64(frames), r).format(r)
}

// tcPeersText lists the connected RTP-MIDI peers, "" with no session.
func tcPeersText() string {
	tcMu.Lock()
	s := tcSession
	tcMu.Unlock()
	if s == nil {
		return ""
	}
	p := s.peerNames()
	if len(p) == 0 {
		return "no peers"
	}
	return strings.Join(p, ", ")
}

// wavTimeReference reads the BWF time reference (samples since midnight)
// from a WAV or RF64 file's bext chunk.
func wavTimeReference(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, err
	}
	if id := string(hdr[:4]); (id != "RIFF" && id != "RF64") || string(hdr[8:12]) != "WAVE" {
		return 0, errors.New("not a WAV file")
	}
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return 0, errors.New("no bext chunk")
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		switch string(ch[:4]) {
		case "bext":
			var b [346]byte
			if size < int64(len(b)) {
				return 0, errors.New("short bext chunk")
			}
			if _, err := io.ReadFull(f, b[:]); err != nil {
				return 0, err
			}
			return int64(binary.LittleEndian.Uint64(b[338:])), nil
		case "data":
			// bext comes before the audio in every writer that sets it.
			return 0, errors.New("no bext chunk")
		}
		if _, err := f.Seek(size+size&1, io.SeekCurrent); err != nil {
			return 0, err
		}
	}
}

// --- Front panel (OLED) ------------------------------------------------------

// renderTimecodeMenu draws the Timecode submenu. Values fit one 256px row.
// Caller holds the app mutex (render).
func renderTimecodeMenu() {
	out := "off"
	if tcOutputOn {
		out = "on"
	}
	chase := "arm"
	if tcChase.armed {
		chase = "armed"
	}
	in, _ := tcStatusText()
	renderEditableMenu([]hardware.MenuItem{
		{Label: "Source →", Value: tcSourceNames[tcSourceIdx]},
		{Label: "Record →", Value: tcRecordNames[tcRecordIdx]},
		{Label: "Output →", Value: out},
		{Label: "Rate →", Value: tcRates[tcRateIdx].Name},
		{Label: "Chase", Value: chase},
		{Label: "In", Value: in},
		{Label: "← Back", Value: ""},
	})
}

// adjustTimecodeSetting steps the edited row of the Timecode submenu.
// Caller holds the app mutex.
func adjustTimecodeSetting(row, direction int) {
	step := func(i, n int) int { return ((i+direction)%n + n) % n }
	switch row {
	case 0:
		tcSourceIdx = step(tcSourceIdx, len(tcSourceNames))
		if tcSourceIdx == tcSourceOff {
			tcDisarmLocked("timecode source off")
		}
	case 1:
		tcRecordIdx = step(tcRecordIdx, len(tcRecordNames))
	case 2:
		tcOutputOn = !tcOutputOn
	case 3:
		tcRateIdx = step(tcRateIdx, len(tcRates))
	default:
		return
	}
	tcSettingsChangedLocked()
	settingChanged()
}

// handleTimecodeClick: Source/Record/Output/Rate are press-to-edit; Chase
// arms or disarms the selected take; Back returns to Settings. Caller
// holds the app mutex.
func handleTimecodeClick() {
	if editingParameter {
		editingParameter = false
		return
	}
	switch selectedMenu {
	case 0, 1, 2, 3:
		editingParameter = true
	case 4:
		if tcChase.armed {
			tcDisarmLocked("from the front panel")
		} else if err := tcArmLocked(); err != nil {
			showSysNotice("CHASE: " + strings.ToUpper(tcShortReason(err)))
			logWarnf("Chase not armed: %v", err)
		}
	case 6:
		currentState = StateSettings
		selectedMenu = 11
		menuScrollOffset = 0
	}
}

// tcShortReason fits an arm refusal into the panel's notice line.
func tcShortReason(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "source"):
		return "no source"
	case strings.Contains(msg, "stop"):
		return "busy"
	case strings.Contains(msg, "no take"):
		return "no take"
	default:
		return "format mismatch"
	}
}
