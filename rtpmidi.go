package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// RTP-MIDI (RFC 6295) with the AppleMIDI session protocol: MIDI over IP as
// macOS "Network MIDI", rtpMIDI on Windows and most show-control software
// speak it. The unit is one session on a control port and the port after
// it (data). Peers invite it (it accepts every invitation) or it invites a
// configured peer; MIDI received from any peer goes to onMIDI, and send
// reaches every connected peer.
//
// Only what MTC needs: no recovery journal is sent (J=0, which receivers
// accept) and received journals are skipped; receiver feedback is not sent.

// rtpMIDIPort is the session's control port by convention; data is +1.
const rtpMIDIPort = 5004

// rtpMIDIPeerTimeout drops a peer that has sent nothing (MIDI or clock
// sync) for this long. Initiators sync every 10 s or so.
var rtpMIDIPeerTimeout = 90 * time.Second

type rtpMIDIPeer struct {
	ssrc      uint32
	name      string
	ctrl      *net.UDPAddr
	data      *net.UDPAddr
	lastSeen  time.Time
	connected bool // both ports accepted
	invited   bool // this end initiated
	token     uint32
}

func (p *rtpMIDIPeer) String() string {
	return fmt.Sprintf("%s (%s)", p.name, p.ctrl.IP)
}

type rtpMIDISession struct {
	name   string
	ssrc   uint32
	start  time.Time
	ctrl   *net.UDPConn
	data   *net.UDPConn
	onMIDI func(msg []byte, from string)
	onPeer func(peer string, up bool)

	mu    sync.Mutex
	peers map[uint32]*rtpMIDIPeer
	seq   uint16
	// invite is the peer this end keeps inviting ("" none): host:port of
	// its control port.
	invite      string
	inviteToken uint32
	quit        chan struct{}
	done        sync.WaitGroup
}

// startRTPMIDI opens a session on port (control) and port+1 (data). port 0
// picks free ports (tests).
func startRTPMIDI(name string, port int, onMIDI func([]byte, string), onPeer func(string, bool)) (*rtpMIDISession, error) {
	ctrl, data, err := listenRTPMIDIPair(port)
	if err != nil {
		return nil, err
	}
	s := &rtpMIDISession{
		name: name, ssrc: rand.Uint32(), start: time.Now(),
		ctrl: ctrl, data: data, onMIDI: onMIDI, onPeer: onPeer,
		peers: map[uint32]*rtpMIDIPeer{}, quit: make(chan struct{}),
		seq: uint16(rand.Uint32()),
	}
	s.done.Add(3)
	go s.readLoop(ctrl, false)
	go s.readLoop(data, true)
	go s.maintain()
	return s, nil
}

// listenRTPMIDIPair binds two consecutive UDP ports. With port 0 it retries
// until a free pair turns up.
func listenRTPMIDIPair(port int) (*net.UDPConn, *net.UDPConn, error) {
	for try := 0; ; try++ {
		ctrl, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, nil, err
		}
		p := ctrl.LocalAddr().(*net.UDPAddr).Port
		data, err := net.ListenUDP("udp4", &net.UDPAddr{Port: p + 1})
		if err == nil {
			return ctrl, data, nil
		}
		ctrl.Close()
		if port != 0 || try > 20 {
			return nil, nil, err
		}
	}
}

// ports returns the control and data ports in use.
func (s *rtpMIDISession) ports() (int, int) {
	return s.ctrl.LocalAddr().(*net.UDPAddr).Port, s.data.LocalAddr().(*net.UDPAddr).Port
}

func (s *rtpMIDISession) close() {
	s.mu.Lock()
	select {
	case <-s.quit:
		s.mu.Unlock()
		return
	default:
	}
	close(s.quit)
	var bye []*rtpMIDIPeer
	for _, p := range s.peers {
		bye = append(bye, p)
	}
	s.mu.Unlock()
	for _, p := range bye {
		s.sendExchange(s.ctrl, p.ctrl, "BY", p.token)
	}
	s.ctrl.Close()
	s.data.Close()
	s.done.Wait()
}

// setInvite makes the session keep inviting addr ("host" or "host:port",
// control port; "" stops).
func (s *rtpMIDISession) setInvite(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if addr != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, fmt.Sprint(rtpMIDIPort))
		}
	}
	s.invite = addr
	s.inviteToken = rand.Uint32()
}

// peerNames lists the connected peers.
func (s *rtpMIDISession) peerNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.peers {
		if p.connected {
			out = append(out, p.String())
		}
	}
	return out
}

// now is the session clock: 100 us units since the session started.
func (s *rtpMIDISession) now() uint64 {
	return uint64(time.Since(s.start) / (100 * time.Microsecond))
}

const (
	rtpMIDIProtocolVersion = 2
	rtpMIDIPayloadType     = 0x61
)

// exchange packet: FFFF, 2-char command, version, token, ssrc, name\0.
func (s *rtpMIDISession) sendExchange(c *net.UDPConn, to *net.UDPAddr, cmd string, token uint32) {
	b := make([]byte, 0, 64)
	b = append(b, 0xFF, 0xFF, cmd[0], cmd[1])
	b = binary.BigEndian.AppendUint32(b, rtpMIDIProtocolVersion)
	b = binary.BigEndian.AppendUint32(b, token)
	b = binary.BigEndian.AppendUint32(b, s.ssrc)
	if cmd != "BY" {
		b = append(b, s.name...)
		b = append(b, 0)
	}
	c.WriteToUDP(b, to)
}

func (s *rtpMIDISession) sendClock(to *net.UDPAddr, count byte, ts [3]uint64) {
	b := make([]byte, 0, 36)
	b = append(b, 0xFF, 0xFF, 'C', 'K')
	b = binary.BigEndian.AppendUint32(b, s.ssrc)
	b = append(b, count, 0, 0, 0)
	for _, t := range ts {
		b = binary.BigEndian.AppendUint64(b, t)
	}
	s.data.WriteToUDP(b, to)
}

func (s *rtpMIDISession) readLoop(c *net.UDPConn, isData bool) {
	defer s.done.Done()
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		pkt := buf[:n]
		if n >= 4 && pkt[0] == 0xFF && pkt[1] == 0xFF {
			s.handleCommand(c, isData, string(pkt[2:4]), pkt, from)
		} else if isData {
			s.handleRTP(pkt, from)
		}
	}
}

func (s *rtpMIDISession) handleCommand(c *net.UDPConn, isData bool, cmd string, pkt []byte, from *net.UDPAddr) {
	switch cmd {
	case "IN", "OK", "NO", "BY":
		if len(pkt) < 16 {
			return
		}
		token := binary.BigEndian.Uint32(pkt[8:12])
		ssrc := binary.BigEndian.Uint32(pkt[12:16])
		name := ""
		if len(pkt) > 16 {
			name = string(pkt[16:])
			if i := indexByte(name, 0); i >= 0 {
				name = name[:i]
			}
		}
		s.exchange(c, isData, cmd, token, ssrc, name, from)
	case "CK":
		if len(pkt) < 36 || !isData {
			return
		}
		ssrc := binary.BigEndian.Uint32(pkt[4:8])
		count := pkt[8]
		var ts [3]uint64
		for i := range ts {
			ts[i] = binary.BigEndian.Uint64(pkt[12+8*i:])
		}
		s.mu.Lock()
		p := s.peers[ssrc]
		if p != nil {
			p.lastSeen = time.Now()
		}
		s.mu.Unlock()
		if p == nil {
			return
		}
		switch count {
		case 0:
			ts[1] = s.now()
			s.sendClock(from, 1, ts)
		case 1:
			ts[2] = s.now()
			s.sendClock(from, 2, ts)
		}
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func (s *rtpMIDISession) exchange(c *net.UDPConn, isData bool, cmd string, token, ssrc uint32, name string, from *net.UDPAddr) {
	var notify func()
	s.mu.Lock()
	p := s.peers[ssrc]
	switch cmd {
	case "IN":
		// Accept every invitation. The control invitation creates the
		// peer, the data one completes it.
		if p == nil {
			if isData {
				s.mu.Unlock()
				s.sendExchange(c, from, "NO", token)
				return
			}
			p = &rtpMIDIPeer{ssrc: ssrc, name: name, ctrl: from}
			s.peers[ssrc] = p
		}
		p.token, p.lastSeen = token, time.Now()
		if isData {
			p.data = from
			if !p.connected {
				p.connected = true
				notify = func() { s.peerEvent(p, true) }
			}
		} else {
			p.ctrl = from
		}
		s.mu.Unlock()
		s.sendExchange(c, from, "OK", token)
	case "OK":
		// An answer to this end's invitation.
		if token != s.inviteToken {
			s.mu.Unlock()
			return
		}
		if !isData {
			if p == nil {
				p = &rtpMIDIPeer{ssrc: ssrc, name: name, invited: true, token: token}
				s.peers[ssrc] = p
			}
			p.ctrl, p.lastSeen = from, time.Now()
			dataAddr := &net.UDPAddr{IP: from.IP, Port: from.Port + 1}
			s.mu.Unlock()
			s.sendExchange(s.data, dataAddr, "IN", token)
			return
		}
		if p != nil {
			p.data, p.lastSeen = from, time.Now()
			if !p.connected {
				p.connected = true
				notify = func() { s.peerEvent(p, true) }
			}
		}
		s.mu.Unlock()
		if p != nil {
			s.sendClock(from, 0, [3]uint64{s.now(), 0, 0})
		}
	case "NO":
		s.mu.Unlock()
		logWarnf("RTP-MIDI: %s refused the invitation", from)
	case "BY":
		if p != nil {
			delete(s.peers, ssrc)
			if p.connected {
				notify = func() { s.peerEvent(p, false) }
			}
		}
		s.mu.Unlock()
	default:
		s.mu.Unlock()
	}
	if notify != nil {
		notify()
	}
}

func (s *rtpMIDISession) peerEvent(p *rtpMIDIPeer, up bool) {
	if up {
		logInfof("RTP-MIDI: %s connected", p)
	} else {
		logInfof("RTP-MIDI: %s left", p)
	}
	if s.onPeer != nil {
		s.onPeer(p.String(), up)
	}
}

// maintain invites the configured peer until it answers, syncs clocks with
// peers this end invited, and drops silent peers.
func (s *rtpMIDISession) maintain() {
	defer s.done.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	lastSync := time.Time{}
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
		now := time.Now()
		var gone []*rtpMIDIPeer
		var syncTo []*net.UDPAddr
		s.mu.Lock()
		inviting := s.invite != ""
		for ssrc, p := range s.peers {
			if now.Sub(p.lastSeen) > rtpMIDIPeerTimeout {
				delete(s.peers, ssrc)
				if p.connected {
					gone = append(gone, p)
				}
				continue
			}
			if p.invited && p.connected && now.Sub(lastSync) >= 10*time.Second {
				syncTo = append(syncTo, p.data)
			}
			if p.invited && p.token == s.inviteToken {
				inviting = inviting && !p.connected
			}
		}
		addr, token := s.invite, s.inviteToken
		s.mu.Unlock()
		for _, p := range gone {
			s.peerEvent(p, false)
		}
		for _, a := range syncTo {
			s.sendClock(a, 0, [3]uint64{s.now(), 0, 0})
		}
		if len(syncTo) > 0 {
			lastSync = now
		}
		if inviting && addr != "" {
			if ua, err := net.ResolveUDPAddr("udp4", addr); err == nil {
				s.sendExchange(s.ctrl, ua, "IN", token)
			}
		}
	}
}

// send transmits one MIDI message (a complete status + data, or a whole
// SysEx) to every connected peer.
func (s *rtpMIDISession) send(msg []byte) {
	s.mu.Lock()
	var to []*net.UDPAddr
	for _, p := range s.peers {
		if p.connected && p.data != nil {
			to = append(to, p.data)
		}
	}
	if len(to) == 0 {
		s.mu.Unlock()
		return
	}
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	b := make([]byte, 0, 16+len(msg))
	b = append(b, 0x80, rtpMIDIPayloadType)
	b = binary.BigEndian.AppendUint16(b, seq)
	b = binary.BigEndian.AppendUint32(b, uint32(s.now()))
	b = binary.BigEndian.AppendUint32(b, s.ssrc)
	if len(msg) <= 15 {
		b = append(b, byte(len(msg)))
	} else {
		b = append(b, 0x80|byte(len(msg)>>8&0x0F), byte(len(msg)))
	}
	b = append(b, msg...)
	for _, a := range to {
		s.data.WriteToUDP(b, a)
	}
}

// handleRTP parses an RTP-MIDI packet's command section and hands each
// message to onMIDI.
func (s *rtpMIDISession) handleRTP(pkt []byte, from *net.UDPAddr) {
	if len(pkt) < 13 || pkt[0]>>6 != 2 || pkt[1]&0x7F != rtpMIDIPayloadType {
		return
	}
	ssrc := binary.BigEndian.Uint32(pkt[8:12])
	s.mu.Lock()
	p := s.peers[ssrc]
	if p != nil {
		p.lastSeen = time.Now()
	}
	s.mu.Unlock()
	if p == nil || !p.connected {
		return
	}
	for _, m := range parseRTPMIDICommands(pkt[12:]) {
		if s.onMIDI != nil {
			s.onMIDI(m, p.String())
		}
	}
}

// parseRTPMIDICommands splits an RTP-MIDI command section into MIDI
// messages (running status expanded). A malformed list yields what parsed
// before the fault.
func parseRTPMIDICommands(b []byte) [][]byte {
	if len(b) < 1 {
		return nil
	}
	flags := b[0]
	n := int(flags & 0x0F)
	b = b[1:]
	if flags&0x80 != 0 {
		if len(b) < 1 {
			return nil
		}
		n = n<<8 | int(b[0])
		b = b[1:]
	}
	if n > len(b) {
		return nil
	}
	list := b[:n]
	var out [][]byte
	var running byte
	first := true
	for len(list) > 0 {
		if !first || flags&0x20 != 0 {
			// delta time: up to 4 bytes, the last without the top bit
			i := 0
			for i < 4 && i < len(list) && list[i]&0x80 != 0 {
				i++
			}
			if i >= len(list) {
				return out
			}
			list = list[i+1:]
			if len(list) == 0 {
				return out
			}
		}
		first = false
		status := list[0]
		if status < 0x80 {
			if running == 0 {
				return out
			}
			status = running
		} else {
			list = list[1:]
		}
		var size int
		switch {
		case status == 0xF0 || status == 0xF7:
			// SysEx (or a continuation segment): to the closing F7 (or
			// segment end F0/F4). Only complete F0..F7 messages are kept.
			end := -1
			for i, c := range list {
				if c == 0xF7 || c == 0xF0 || c == 0xF4 {
					end = i
					break
				}
			}
			if end < 0 {
				return out
			}
			if status == 0xF0 && list[end] == 0xF7 {
				m := append([]byte{0xF0}, list[:end+1]...)
				out = append(out, m)
			}
			list = list[end+1:]
			continue
		case status >= 0xF8 || status == 0xF6:
			size = 0
		case status == 0xF1 || status == 0xF3:
			size = 1
		case status == 0xF2:
			size = 2
		case status >= 0xF4:
			size = 0
		case status >= 0xC0 && status < 0xE0:
			size = 1
		default:
			size = 2
		}
		if status < 0xF0 {
			running = status
		} else if status < 0xF8 {
			running = 0
		}
		if size > len(list) {
			return out
		}
		m := append([]byte{status}, list[:size]...)
		out = append(out, m)
		list = list[size:]
	}
	return out
}
