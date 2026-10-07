// Package alsapcm is a minimal cgo wrapper over ALSA, just enough for the
// app to be the single client of an Inferno virtual soundcard.
//
// One Device holds a capture handle and a playback handle on the same ALSA
// device name. That pairing is the whole point: inferno's plugin keeps its
// inferno servers in a process-global map keyed by device ID
// (alsa_pcm_inferno/src/lib.rs, get_or_create_instance), so two handles opened
// by one process share a single Inferno instance and bind inferno's UDP ports
// once. Two separate processes cannot do this - the second fails with
// EADDRINUSE - which is why the app must be the client itself rather than
// delegating to arecord/aplay.
//
// Samples are signed 32-bit little-endian, interleaved, which is what the
// plugin requires ("Application must support 32-bit signed integer audio
// samples"). Frames are passed as []int32 so no byte-order handling is needed
// on either side.
package alsapcm

/*
#cgo pkg-config: alsa

#include <alsa/asoundlib.h>
#include <stdlib.h>

// cgo turns any C function returning int into (T, error) and hides the value,
// which would cost us the ALSA error code. These void helpers pass frames and
// the raw negative errno back through out-parameters instead.

static void pcm_open_both(const char *dev, int rate, int channels, int latency_us,
                          snd_pcm_t **cap, snd_pcm_t **play, int *out_err) {
	int err;
	*out_err = 0;
	if ((err = snd_pcm_open(cap, dev, SND_PCM_STREAM_CAPTURE, 0)) < 0) { *out_err = err; return; }
	if ((err = snd_pcm_open(play, dev, SND_PCM_STREAM_PLAYBACK, 0)) < 0) {
		snd_pcm_close(*cap);
		*out_err = err;
		return;
	}
	// Both handles must agree on format, rate, channels and access, or the
	// plugin sees conflicting configurations for one device.
	if ((err = snd_pcm_set_params(*cap, SND_PCM_FORMAT_S32_LE, SND_PCM_ACCESS_RW_INTERLEAVED,
	                              (unsigned int)channels, (unsigned int)rate, 1,
	                              (snd_pcm_uframes_t)latency_us)) < 0) {
		snd_pcm_close(*cap); snd_pcm_close(*play); *out_err = err; return;
	}
	if ((err = snd_pcm_set_params(*play, SND_PCM_FORMAT_S32_LE, SND_PCM_ACCESS_RW_INTERLEAVED,
	                              (unsigned int)channels, (unsigned int)rate, 1,
	                              (snd_pcm_uframes_t)latency_us)) < 0) {
		snd_pcm_close(*cap); snd_pcm_close(*play); *out_err = err; return;
	}
}

static void pcm_readi(snd_pcm_t *pcm, void *buf, int frames, int *out_frames, int *out_err) {
	snd_pcm_sframes_t n = snd_pcm_readi(pcm, buf, (snd_pcm_uframes_t)frames);
	if (n < 0) { *out_frames = 0; *out_err = (int)n; return; }
	*out_frames = (int)n;
	*out_err = 0;
}

static void pcm_writei(snd_pcm_t *pcm, const void *buf, int frames, int *out_frames, int *out_err) {
	snd_pcm_sframes_t n = snd_pcm_writei(pcm, buf, (snd_pcm_uframes_t)frames);
	if (n < 0) { *out_frames = 0; *out_err = (int)n; return; }
	*out_frames = (int)n;
	*out_err = 0;
}

static void pcm_delay(snd_pcm_t *pcm, long *out_frames, int *out_err) {
	snd_pcm_sframes_t d = 0;
	int err = snd_pcm_delay(pcm, &d);
	*out_frames = (long)d;
	*out_err = err < 0 ? err : 0;
}

static void pcm_prepare(snd_pcm_t *pcm, int *out_err) {
	int err = snd_pcm_prepare(pcm);
	*out_err = err < 0 ? err : 0;
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// LatencyUs is the ALSA buffer/period hint in microseconds. It is a knob
// rather than a constant because it trades latency against resilience
// to scheduling jitter on the host, and the right value is a property of the
// machine: inferno's own default network latency is 10ms, so this must not
// go much below that for the network side, while the ALSA side only feeds
// the plugin's ring and can be deeper - file playback has no live-input
// deadline, so depth here buys immunity from app-mutex stalls (the render
// tick can hold the mutex ~15ms during an SPI push) rather than costing
// anything audible. Raised after TX xrun-dribble diagnosis on the test unit.
const LatencyUs = 120000

// Device is an open pair of ALSA handles on one Inferno virtual soundcard.
type Device struct {
	// capMu and playMu are held across each Read and Write respectively,
	// and Close takes both: closing a handle while another goroutine is
	// inside snd_pcm_readi/writei on it is a use-after-free in C. The app
	// does exactly that shape - the capture loop, the playback pump and the
	// TX warm-up each run on their own goroutine against one paired device
	// that a restart closes. One lock per direction, so a blocked capture
	// read never stalls playback writes.
	capMu, playMu sync.Mutex
	cap, play     *C.snd_pcm_t
	name          string
	rate          int
	channels      int
	framesPerIO   int
	closed        bool
	// xruns counts overruns/underruns recovered in place. Each one is a gap
	// in the audio that Read/Write otherwise hide from the caller.
	xruns     atomic.Int64
	playDelay atomic.Int64
}

// Xruns returns how many overruns (capture) and underruns (playback) the
// device has recovered from since it was opened.
func (d *Device) Xruns() int64 { return d.xruns.Load() }

// PlaybackDelay is the playback stream's delay after the last write, in
// frames (snd_pcm_delay): how long until a frame written now is played.
// 0 before the first write.
func (d *Device) PlaybackDelay() int64 { return d.playDelay.Load() }

// Open opens the named ALSA device for both capture and playback at the given
// rate and channel count. Both settings come from the app's audio settings, so
// changing either means closing and reopening - which is why the device is
// only rebuilt when the user actually changes something.
func Open(name string, rate, channels int) (*Device, error) {
	if rate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("alsapcm: invalid rate/channels %d/%d", rate, channels)
	}
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var cap, play *C.snd_pcm_t
	var rc C.int
	C.pcm_open_both(cname, C.int(rate), C.int(channels), C.int(LatencyUs), &cap, &play, &rc)
	if rc != 0 {
		return nil, fmt.Errorf("alsapcm: open %s: %w", name, alsaErr(rc))
	}

	d := &Device{
		cap: cap, play: play, name: name, rate: rate, channels: channels,
		// Keep each I/O comfortably under a period so a single read or write
		// cannot overrun it.
		framesPerIO: 1024,
	}
	runtime.SetFinalizer(d, (*Device).Close)
	return d, nil
}

// Read captures interleaved frames into buf, which must hold a whole number of
// frames. It returns the number of frames read; a short read is normal at the
// end of a buffer and is not an error.
//
// An overrun (EPIPE) is recovered from in place by re-preparing the handle, so
// a late wakeup costs one dropped buffer rather than tearing the device down
// and taking the inferno device off the network with it.
func (d *Device) Read(buf []int32) (int, error) {
	// Argument checks before state, so a malformed call can never divide by
	// zero or reach ALSA: a sub-frame buffer is a silent no-op whatever the
	// device is doing, and an unusable one is a plain error.
	if d.channels <= 0 {
		return 0, fmt.Errorf("alsapcm: read on device with %d channels", d.channels)
	}
	want := len(buf) / d.channels
	if want == 0 {
		return 0, nil
	}
	d.capMu.Lock()
	defer d.capMu.Unlock()
	if d.closed || d.cap == nil {
		return 0, fmt.Errorf("alsapcm: read on closed device")
	}
	if want > d.framesPerIO {
		want = d.framesPerIO
	}
	for attempt := 0; attempt < 2; attempt++ {
		var frames, rc C.int
		C.pcm_readi(d.cap, unsafe.Pointer(&buf[0]), C.int(want), &frames, &rc)
		if rc == 0 {
			return int(frames), nil
		}
		if !recoverable(rc) {
			return 0, fmt.Errorf("alsapcm: read: %w", alsaErr(rc))
		}
		d.xruns.Add(1)
		var prc C.int
		C.pcm_prepare(d.cap, &prc)
		if prc != 0 {
			return 0, fmt.Errorf("alsapcm: reprepare after read: %w", alsaErr(prc))
		}
	}
	return 0, fmt.Errorf("alsapcm: read did not recover")
}

// Write plays interleaved frames from buf, which must hold a whole number of
// frames, and returns the number of frames written. Overruns are recovered in
// place for the same reason as Read.
func (d *Device) Write(buf []int32) (int, error) {
	// Same argument-before-state ordering as Read; see the comment there.
	if d.channels <= 0 {
		return 0, fmt.Errorf("alsapcm: write on device with %d channels", d.channels)
	}
	want := len(buf) / d.channels
	if want == 0 {
		return 0, nil
	}
	d.playMu.Lock()
	defer d.playMu.Unlock()
	if d.closed || d.play == nil {
		return 0, fmt.Errorf("alsapcm: write on closed device")
	}
	if want > d.framesPerIO {
		want = d.framesPerIO
	}
	for attempt := 0; attempt < 2; attempt++ {
		var frames, rc C.int
		C.pcm_writei(d.play, unsafe.Pointer(&buf[0]), C.int(want), &frames, &rc)
		if rc == 0 {
			// How far behind the write the stream plays, for the
			// timecode output and the chase (PlaybackDelay).
			var delay C.long
			var drc C.int
			C.pcm_delay(d.play, &delay, &drc)
			if drc == 0 && delay >= 0 {
				d.playDelay.Store(int64(delay))
			}
			return int(frames), nil
		}
		if !recoverable(rc) {
			return 0, fmt.Errorf("alsapcm: write: %w", alsaErr(rc))
		}
		d.xruns.Add(1)
		var prc C.int
		C.pcm_prepare(d.play, &prc)
		if prc != 0 {
			return 0, fmt.Errorf("alsapcm: reprepare after write: %w", alsaErr(prc))
		}
	}
	return 0, fmt.Errorf("alsapcm: write did not recover")
}

// recoverable reports whether an ALSA error is a transient overrun that
// snd_pcm_prepare can clear, as opposed to a real fault.
func recoverable(err C.int) bool {
	return err == -C.EPIPE || err == -C.ESTRPIPE
}

// Close releases both handles. Safe to call more than once, which matters
// because it is also a finalizer. It waits for any Read or Write in
// progress on another goroutine to return before freeing the handles.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.capMu.Lock()
	defer d.capMu.Unlock()
	d.playMu.Lock()
	defer d.playMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	runtime.SetFinalizer(d, nil)
	if d.cap != nil {
		C.snd_pcm_close(d.cap)
		d.cap = nil
	}
	if d.play != nil {
		C.snd_pcm_close(d.play)
		d.play = nil
	}
	return nil
}

// alsaErr maps ALSA's negative return codes onto Go errors, naming the common
// ones because a bare integer is useless in a log.
func alsaErr(err C.int) error {
	switch err {
	case -C.ENOENT:
		return fmt.Errorf("no such ALSA device")
	case -C.EACCES:
		return fmt.Errorf("permission denied")
	case -C.EBUSY:
		return fmt.Errorf("device busy")
	case -C.EINVAL:
		return fmt.Errorf("invalid argument (format, rate or channel count unsupported)")
	case -C.ENOMEM:
		return fmt.Errorf("out of memory")
	case -C.EPIPE:
		return fmt.Errorf("underrun/overrun")
	default:
		return fmt.Errorf("alsa error %d", int(err))
	}
}
