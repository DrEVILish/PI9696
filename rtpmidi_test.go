package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRTPMIDISessionInviteAndExchange(t *testing.T) {
	type got struct {
		mu   sync.Mutex
		msgs [][]byte
		up   []string
	}
	var ga, gb got
	rec := func(g *got) (func([]byte, string), func(string, bool)) {
		return func(m []byte, _ string) {
				g.mu.Lock()
				g.msgs = append(g.msgs, append([]byte(nil), m...))
				g.mu.Unlock()
			}, func(p string, up bool) {
				g.mu.Lock()
				g.up = append(g.up, fmt.Sprint(up))
				g.mu.Unlock()
			}
	}
	ma, pa := rec(&ga)
	mb, pb := rec(&gb)
	a, err := startRTPMIDI("A", 0, ma, pa)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	b, err := startRTPMIDI("B", 0, mb, pb)
	if err != nil {
		t.Fatal(err)
	}
	bctrl, _ := b.ports()
	a.setInvite(fmt.Sprintf("127.0.0.1:%d", bctrl))
	waitFor(t, 6*time.Second, "both ends connected", func() bool {
		return len(a.peerNames()) == 1 && len(b.peerNames()) == 1
	})
	if p := a.peerNames()[0]; p != "B (127.0.0.1)" {
		t.Fatalf("A's peer = %q", p)
	}
	qf := []byte{0xF1, 0x35}
	full := mtcFullFrame(Timecode{1, 2, 3, 4}, tcRates[tcRateDefault])
	a.send(qf)
	b.send(full)
	waitFor(t, 2*time.Second, "messages delivered", func() bool {
		ga.mu.Lock()
		gb.mu.Lock()
		defer ga.mu.Unlock()
		defer gb.mu.Unlock()
		return len(ga.msgs) == 1 && len(gb.msgs) == 1
	})
	if !bytes.Equal(gb.msgs[0], qf) || !bytes.Equal(ga.msgs[0], full) {
		t.Fatalf("A got % x, B got % x", ga.msgs[0], gb.msgs[0])
	}
	// Closing says goodbye: the other end drops the peer at once.
	b.close()
	waitFor(t, 2*time.Second, "A sees B leave", func() bool {
		ga.mu.Lock()
		defer ga.mu.Unlock()
		return len(a.peerNames()) == 0 && fmt.Sprint(ga.up) == "[true false]"
	})
}

func TestParseRTPMIDICommands(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{"one QF", []byte{0x02, 0xF1, 0x12}, [][]byte{{0xF1, 0x12}}},
		{"delta before each later command", []byte{0x06, 0xF1, 0x12, 0x00, 0xF1, 0x23, 0x00}, [][]byte{{0xF1, 0x12}, {0xF1, 0x23}}},
		{"Z flag: delta before the first", []byte{0x23, 0x81, 0x00, 0xF8}, [][]byte{{0xF8}}},
		{"running status", []byte{0x07, 0x90, 0x3C, 0x40, 0x00, 0x3E, 0x40, 0x00}, [][]byte{{0x90, 0x3C, 0x40}, {0x90, 0x3E, 0x40}}},
		{"sysex", append([]byte{0x0A}, mtcFullFrame(Timecode{10, 0, 0, 0}, tcRates[tcRateDefault])...), [][]byte{mtcFullFrame(Timecode{10, 0, 0, 0}, tcRates[tcRateDefault])}},
		{"long header", append([]byte{0x80, 0x02}, 0xF1, 0x70), [][]byte{{0xF1, 0x70}}},
		{"truncated", []byte{0x05, 0xF1}, nil},
		{"data without status", []byte{0x02, 0x12, 0x34}, nil},
	}
	for _, c := range cases {
		got := parseRTPMIDICommands(c.in)
		if fmt.Sprintf("% x", got) != fmt.Sprintf("% x", c.want) {
			t.Errorf("%s: got % x, want % x", c.name, got, c.want)
		}
	}
}
