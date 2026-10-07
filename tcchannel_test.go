package main

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestDropLastChannel(t *testing.T) {
	in := []int32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	got := dropLastChannel(in, 3)
	if want := []int32{1, 2, 4, 5, 7, 8}; !equalInt32(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestToTxFramesAddsTimecodeChannel(t *testing.T) {
	le := func(v ...int32) []byte {
		var b []byte
		for _, x := range v {
			b = binary.LittleEndian.AppendUint32(b, uint32(x))
		}
		return b
	}
	out := make([]int32, 6)
	// A plain take: its channels, then a silent TIMECODE.
	txPlayout{fileChannels: 2, txChannels: 3}.toTxFrames(le(1, -2, 3, -4), out, 2)
	if want := []int32{1, -2, 0, 3, -4, 0}; !equalInt32(out, want) {
		t.Fatalf("2ch take on a 3ch transmitter: %v, want %v", out, want)
	}
	// A take that recorded timecode as audio: its last channel is TIMECODE.
	txPlayout{fileChannels: 3, txChannels: 3}.toTxFrames(le(1, 2, 7, 3, 4, 8), out, 2)
	if want := []int32{1, 2, 7, 3, 4, 8}; !equalInt32(out, want) {
		t.Fatalf("3ch take on a 3ch transmitter: %v, want %v", out, want)
	}
}

// patternDevice delivers frames whose samples say where they came from:
// frame index << 8 | channel, at real-time pace.
type patternDevice struct {
	fakeTxHolder
	channels int
	mu       sync.Mutex
	next     int32
	done     chan struct{}
	once     sync.Once
}

func (d *patternDevice) Read(buf []int32) (int, error) {
	select {
	case <-d.done:
		return 0, errors.New("closed")
	case <-time.After(5 * time.Millisecond):
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := min(len(buf)/d.channels, 240)
	for f := 0; f < n; f++ {
		for c := 0; c < d.channels; c++ {
			buf[f*d.channels+c] = d.next<<8 | int32(c)
		}
		d.next++
	}
	return n, nil
}

func (d *patternDevice) Close() error {
	d.once.Do(func() { close(d.done) })
	return nil
}

func (d *patternDevice) Xruns() int64 { return 0 }

// readFrames reads n frames of ch channels from fd, waiting for them.
func readFrames(t *testing.T, fd, n, ch int) [][]int32 {
	t.Helper()
	buf := make([]byte, n*ch*4)
	deadline := time.Now().Add(3 * time.Second)
	for off := 0; off < len(buf); {
		r, err := syscall.Read(fd, buf[off:])
		if err == syscall.EAGAIN || r == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("timed out reading %d frames", n)
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		off += r
	}
	out := make([][]int32, n)
	for f := range out {
		out[f] = make([]int32, ch)
		for c := range out[f] {
			out[f][c] = int32(binary.LittleEndian.Uint32(buf[(f*ch+c)*4:]))
		}
	}
	return out
}

// The capture loop strips TIMECODE from the FIFO, and a layout switch
// drains the FIFO and restarts it, frame-aligned, with every channel from
// the stream position it reports.
func TestInfernoRxLoopTimecodeLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	rd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(rd)
	dev := &patternDevice{channels: 3, done: make(chan struct{})}
	quit, done := make(chan struct{}), make(chan struct{})
	go infernoRxLoop(dev, path, quit, done, 0, 3, 48000)
	defer func() { close(quit); dev.Close(); <-done }()

	// Audio only: channels 0 and 1, consecutive frames.
	for i, f := range readFrames(t, rd, 500, 2) {
		if f[0]&0xFF != 0 || f[1]&0xFF != 1 || (i > 0 && f[0]>>8 != int32(i)+(f[0]>>8-int32(i))) {
			t.Fatalf("frame %d = %x", i, f)
		}
	}
	time.Sleep(50 * time.Millisecond) // let the FIFO fill up behind us
	mutex.Lock()
	pos, ok := fifoSetLayout(true)
	mutex.Unlock()
	if !ok {
		t.Fatal("the capture loop did not answer the layout switch")
	}
	got := readFrames(t, rd, 500, 3)
	for i, f := range got {
		want := []int32{int32(pos+int64(i))<<8 | 0, int32(pos+int64(i))<<8 | 1, int32(pos+int64(i))<<8 | 2}
		if !equalInt32(f, want) {
			t.Fatalf("frame %d after the switch (pos %d) = %x, want %x", i, pos, f, want)
		}
	}
	mutex.Lock()
	pos, ok = fifoSetLayout(false)
	mutex.Unlock()
	if !ok {
		t.Fatal("no answer switching back")
	}
	if f := readFrames(t, rd, 1, 2)[0]; f[0] != int32(pos)<<8 || f[1] != int32(pos)<<8|1 {
		t.Fatalf("first frame back in the audio layout = %x, want position %d", f, pos)
	}
}

// A dropping writer never part-writes a chunk: one that does not fit the
// FIFO whole is dropped whole, so the capture loop never waits on it.
func TestFifoFeedDropsWholeChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	f := fifoFeed{fd: fd, ctl: make(chan fifoLayoutReq), dropWhenFull: true}
	size, _, _ := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), linuxFGetPipeSz, 0)
	// Leave 100 bytes of room.
	if err := f.write(make([]byte, int(size)-100), nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := f.write(make([]byte, 4096), nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 100*time.Millisecond || f.dropped != 4096 {
		t.Fatalf("a chunk that does not fit: dropped %d bytes after %v", f.dropped, time.Since(start))
	}
	var held int32
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCINQ, uintptr(unsafe.Pointer(&held)))
	if int(held) != int(size)-100 {
		t.Fatalf("FIFO holds %d bytes, want %d: part of the dropped chunk was written", held, int(size)-100)
	}
}
