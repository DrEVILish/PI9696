// Blackmagic HyperDeck Ethernet Protocol server: a transport-control
// subset (TCP 9993) so switchers, stream decks and automation can drive the
// unit like a HyperDeck - record, play, stop, seek - and poll/subscribe to
// transport state. Reference: HyperDeck Ethernet Protocol manual (line
// oriented text, CRLF server responses, LF-or-CRLF client lines).
//
// Only single-line commands are parsed (verb: key: value on one line); each
// non-empty line is one command and blank lines are ignored. The multiline
// block form some clients use for parameters is not supported - Companion,
// atem and nc-style manual use all send single-line commands.
//
// The protocol has no authentication, so the server only listens while the
// HyperDeck Control settings toggle is on (default off, persisted). Every
// transport command funnels through the same gates as the physical buttons
// (onButtonPress / startRecordingGuarded) so a remote client can never start
// a take the hardware would refuse.
package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"pi9696/hardware"
)

const hyperdeckPort = "9993"
const hyperdeckProtoVersion = "1.11"

// hyperdeckBindAddr is the production wildcard bind; tests override it with
// 127.0.0.1:0 so the suite never touches the real port (or a live unit's).
var hyperdeckBindAddr = ":" + hyperdeckPort

// hyperdeckEnabled is the HyperDeck Control settings toggle (default off,
// persisted): the protocol has no authentication, so the port only listens
// while explicitly enabled.
var hyperdeckEnabled bool

// hyperdeckListener is the live server socket, nil when the toggle is off.
// Guarded by hyperdeckMu; transport state itself stays under the app mutex.
var (
	hyperdeckMu       sync.Mutex
	hyperdeckListener net.Listener
)

// setHyperdeckEnabledLocked flips the settings toggle; caller holds the app
// mutex. A failed bind leaves the toggle off so the switch reflects reality.
func setHyperdeckEnabledLocked(on bool) {
	if on == hyperdeckEnabled && hyperdeckRunning() == on {
		return
	}
	if on {
		if err := startHyperdeckServerOn(hyperdeckBindAddr); err != nil {
			logWarnf("HyperDeck control failed to bind :%s: %v", hyperdeckPort, err)
			hyperdeckEnabled = false
			return
		}
		hyperdeckEnabled = true
		logInfof("HyperDeck control listening on :%s", hyperdeckPort)
		return
	}
	hyperdeckEnabled = false
	stopHyperdeckServer()
	logInfof("HyperDeck control stopped")
}

func hyperdeckRunning() bool {
	hyperdeckMu.Lock()
	defer hyperdeckMu.Unlock()
	return hyperdeckListener != nil
}

// startHyperdeckServerOn binds addr and serves until stopHyperdeckServer.
// Split out so tests can bind 127.0.0.1:0 without touching port 9993.
func startHyperdeckServerOn(addr string) error {
	hyperdeckMu.Lock()
	defer hyperdeckMu.Unlock()
	if hyperdeckListener != nil {
		return nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	hyperdeckListener = l
	go hyperdeckAcceptLoop(l)
	return nil
}

func stopHyperdeckServer() {
	hyperdeckMu.Lock()
	l := hyperdeckListener
	hyperdeckListener = nil
	hyperdeckMu.Unlock()
	if l != nil {
		l.Close() // accept loop exits; open conns drain on next read/write
	}
}

func hyperdeckAcceptLoop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go handleHyperdeckConn(c)
	}
}

// hyperdeckConn is one controller session: writes from the command loop and
// the notify ticker share the socket under wmu.
type hyperdeckConn struct {
	c               net.Conn
	wmu             sync.Mutex
	notifyTransport bool
	lastSig         string
}

func (h *hyperdeckConn) write(s string) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	h.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := h.c.Write([]byte(s))
	return err
}

// hyperdeckBlock writes one framed response: "code title:\r\n" + lines +
// blank line, the shape every controller parses.
func (h *hyperdeckConn) block(code int, title string, lines []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s:\r\n", code, title)
	for _, l := range lines {
		b.WriteString(l + "\r\n")
	}
	b.WriteString("\r\n")
	if err := h.write(b.String()); err != nil {
		h.c.Close()
	}
}

func (h *hyperdeckConn) ok() {
	h.write("200 ok\r\n")
}

func (h *hyperdeckConn) fail(code int, msg string) {
	h.write(fmt.Sprintf("%d %s\r\n", code, msg))
}

func handleHyperdeckConn(c net.Conn) {
	defer c.Close()
	h := &hyperdeckConn{c: c}
	h.block(500, "connection info", []string{
		"protocol version: " + hyperdeckProtoVersion,
		"model: PI9696",
		"unique id: " + hyperdeckUniqueID(),
	})
	// Async transport notifications only fire on state *changes* (signature
	// excludes the running timecode, which would otherwise spam every tick);
	// command responses below also push one synchronously after each
	// state-changing command so a poll loop sees the edge immediately.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if h.notifyTransport {
					h.pushTransportNotify()
				}
			}
		}
	}()

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), 4096)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !h.dispatch(line) {
			return // quit
		}
	}
}

// dispatch runs one command line; false means the session is over.
func (h *hyperdeckConn) dispatch(line string) bool {
	verb, params := hyperdeckParse(line)
	noteActivity()
	switch verb {
	case "ping":
		h.ok()
	case "quit":
		h.ok()
		return false
	case "help", "commands":
		h.ok()
	case "device info":
		mutex.Lock()
		name := deviceName
		mutex.Unlock()
		h.block(204, "device info", []string{
			"protocol version: " + hyperdeckProtoVersion,
			"model: PI9696",
			"unique id: " + hyperdeckUniqueID(),
			"slot count: 1",
			"software version: " + appVersion,
			"name: " + name,
		})
	case "record":
		name, named := params["name"]
		if named && !isValidFilePrefix(name) {
			h.fail(102, "invalid value")
			return true
		}
		mutex.Lock()
		// A named record applies the name to this take only: the filename
		// is baked synchronously inside startRecording, so the previous
		// prefix is restored before returning and later takes (and the
		// persisted setting) are unaffected.
		var prev string
		if named {
			prev = filePrefix
			filePrefix = name
		}
		ok := startRecordingGuarded()
		if named {
			filePrefix = prev
		}
		full := lowDisk()
		mutex.Unlock()
		if ok {
			h.ok()
			h.afterChange()
		} else if full {
			h.fail(104, "disk full")
		} else {
			h.fail(103, "unsupported")
		}
	case "play":
		speed := hyperdeckIntParam(params, "speed", 100)
		if speed == 0 {
			// Shuttle-to-zero pauses a running track; anything else is a
			// plain play through the physical PLAY key's toggle semantics.
			mutex.Lock()
			pausable := currentState == StatePlaying
			if pausable {
				pausePlayback()
			}
			playing := currentState == StatePlaying || currentState == StatePaused
			mutex.Unlock()
			if playing {
				h.ok()
				h.afterChange()
			} else {
				h.fail(103, "not playing")
			}
			return true
		}
		// onButtonPress locks internally - never call it holding the app
		// mutex (see handleInputButton, which calls it lock-free too).
		onButtonPress(hardware.PlayButton)
		mutex.Lock()
		playing := currentState == StatePlaying || currentState == StatePaused
		mutex.Unlock()
		if playing {
			h.ok()
			h.afterChange()
		} else {
			h.fail(103, "no recordings to play")
		}
	case "stop":
		onButtonPress(hardware.StopButton)
		h.ok()
		h.afterChange()
	case "jog", "shuttle":
		speed := hyperdeckIntParam(params, "speed", 0)
		dir := 1
		if speed < 0 {
			dir = -1
		}
		if verb == "shuttle" && speed == 0 {
			// Shuttle-to-zero is the deck idiom for "hold still": pause a
			// running track, otherwise a plain stop.
			mutex.Lock()
			if currentState == StatePlaying {
				pausePlayback()
			}
			mutex.Unlock()
			onButtonPress(hardware.StopButton)
			h.ok()
			h.afterChange()
			return true
		}
		mutex.Lock()
		seekable := currentState == StatePlaying || currentState == StatePaused
		if seekable {
			seekPlayback(dir)
		}
		mutex.Unlock()
		if seekable {
			h.ok()
			h.afterChange()
		} else {
			h.fail(103, "not playing")
		}
	case "goto":
		// Only rewind-to-top is mappable: the unit plays whole takes, not
		// clip timelines, so any other goto target is unsupported.
		if t, ok := params["timeline"]; ok && strings.TrimSpace(t) == "0" {
			mutex.Lock()
			seekable := currentState == StatePlaying || currentState == StatePaused
			if seekable {
				restartPlaybackAt(0)
			}
			mutex.Unlock()
			if seekable {
				h.ok()
				h.afterChange()
			} else {
				h.fail(103, "not playing")
			}
			return true
		}
		h.fail(103, "unsupported")
	case "transport info":
		h.block(208, "transport info", hyperdeckTransportBlock())
	case "slot info":
		id := hyperdeckIntParam(params, "slot id", 1)
		if id != 1 {
			h.fail(102, "invalid value")
			return true
		}
		h.block(202, "slot info", []string{
			"slot id: 1",
			"status: mounted",
			"volume name: PI9696",
		})
	case "clips count":
		mutex.Lock()
		n := len(recordingFiles())
		mutex.Unlock()
		h.block(214, "clips count", []string{fmt.Sprintf("count: %d", n)})
	case "clips get":
		mutex.Lock()
		files := recordingFiles()
		mutex.Unlock()
		lines := make([]string, 0, len(files)*3)
		for i, f := range files {
			lines = append(lines, "clip id: "+strconv.Itoa(i), "name: "+baseName(f))
		}
		h.block(206, "clips", lines)
	case "slot select":
		if hyperdeckIntParam(params, "slot id", 1) == 1 {
			h.ok()
		} else {
			h.fail(102, "invalid value")
		}
	case "notify":
		if v, ok := params["transport"]; ok {
			h.notifyTransport = (v == "true")
		}
		h.block(209, "notify", []string{
			"transport: " + boolStr(h.notifyTransport),
			"slot: false",
			"remote: false",
			"configuration: false",
			"dropped frames: false",
			"display timecode: false",
			"timeline position: false",
			"playrange: false",
		})
		h.snapshotSig()
	default:
		// Known verbs we deliberately don't implement (clip timelines,
		// spill, preview, configuration) get "unsupported"; garbage gets
		// "syntax error" so a mistyped controller sees the difference.
		if hyperdeckKnownVerb(verb) {
			h.fail(103, "unsupported")
		} else {
			h.fail(100, "syntax error")
		}
	}
	return true
}

// afterChange pushes a synchronous transport notification to a subscribed
// session right after a command moved the transport, so controllers see the
// edge without waiting for the 500ms ticker.
func (h *hyperdeckConn) afterChange() {
	if h.notifyTransport {
		h.pushTransportNotify()
	}
}

func (h *hyperdeckConn) snapshotSig() {
	h.lastSig = hyperdeckSig()
}

func (h *hyperdeckConn) pushTransportNotify() {
	sig := hyperdeckSig()
	if sig == h.lastSig {
		return
	}
	h.lastSig = sig
	h.block(508, "transport info", hyperdeckTransportBlock())
}

// hyperdeckSig is the change signature for async notifications: status,
// speed and clip id only - the running timecode is deliberately excluded so
// a recording doesn't spam subscribers every tick.
func hyperdeckSig() string {
	mutex.Lock()
	defer mutex.Unlock()
	status, speed := hyperdeckStatusLocked()
	return status + "|" + strconv.Itoa(speed) + "|" + hyperdeckClipIDLocked()
}

// hyperdeckParse splits "verb: key: value: key2: value2" into the verb and a
// param map. Values may themselves contain colons (timecodes), so a
// "timecode" key swallows the rest of the line instead of splitting it.
func hyperdeckParse(line string) (string, map[string]string) {
	params := map[string]string{}
	head, rest, _ := strings.Cut(line, ":")
	verb := strings.ToLower(strings.TrimSpace(head))
	if strings.TrimSpace(rest) == "" {
		return verb, params
	}
	toks := strings.Split(rest, ":")
	for i := 0; i < len(toks); i++ {
		key := strings.ToLower(strings.TrimSpace(toks[i]))
		if key == "" {
			continue
		}
		if key == "timecode" {
			params[key] = strings.TrimSpace(strings.Join(toks[i+1:], ":"))
			break
		}
		if i+1 < len(toks) {
			params[key] = strings.TrimSpace(toks[i+1])
			i++
		}
	}
	return verb, params
}

func hyperdeckIntParam(params map[string]string, key string, def int) int {
	v, ok := params[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// hyperdeckKnownVerb lists real protocol verbs this server answers 103 to
// (rather than 100) so controllers can tell "valid but unimplemented" from
// a typo.
func hyperdeckKnownVerb(verb string) bool {
	switch verb {
	case "preview", "playrange", "play option", "play on startup",
		"record spill", "spill order", "clips clear", "clips rebuild",
		"clip info", "disk list", "cache info", "configuration",
		"remote", "watchdog", "slot unblock", "format", "identify":
		return true
	}
	return false
}

// hyperdeckStatusLocked maps the app state to the deck status/speed pair;
// caller holds the app mutex.
func hyperdeckStatusLocked() (string, int) {
	switch {
	case isRecording:
		return "record", 100
	case currentState == StatePlaying:
		return "play", 100
	case currentState == StatePaused:
		return "stopped", 0
	default:
		return "stopped", 0
	}
}

// hyperdeckClipIDLocked is the 0-based index of the active take in the
// recordings list, or "none" at idle; caller holds the app mutex.
func hyperdeckClipIDLocked() string {
	files := recordingFiles()
	if isRecording {
		return strconv.Itoa(len(files)) // take still being written sorts last
	}
	if playbackFile != "" {
		for i, f := range files {
			if f == playbackFile {
				return strconv.Itoa(i)
			}
		}
	}
	return "none"
}

// hyperdeckElapsedLocked is the record/play position for the timecode
// fields; caller holds the app mutex.
func hyperdeckElapsedLocked() time.Duration {
	if isRecording {
		return time.Since(recordStart)
	}
	if currentState == StatePlaying || currentState == StatePaused {
		return playbackPosition()
	}
	return 0
}

// hyperdeckTimecode formats d as HH:MM:SS:FF at 25fps, the deck convention.
func hyperdeckTimecode(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalFrames := int(d.Seconds() * 25)
	ff := totalFrames % 25
	s := totalFrames / 25
	return fmt.Sprintf("%02d:%02d:%02d:%02d", s/3600, (s/60)%60, s%60, ff)
}

// hyperdeckTransportBlock builds the 208/508 body, locking the app mutex
// for the state read.
func hyperdeckTransportBlock() []string {
	mutex.Lock()
	status, speed := hyperdeckStatusLocked()
	clip := hyperdeckClipIDLocked()
	tc := hyperdeckTimecode(hyperdeckElapsedLocked())
	mutex.Unlock()
	return []string{
		"status: " + status,
		fmt.Sprintf("speed: %d", speed),
		"slot id: 1",
		"clip id: " + clip,
		"single clip: true",
		"display timecode: " + tc,
		"timecode: " + tc,
		"video format: none",
		"loop: false",
		"timeline: 0",
	}
}

func hyperdeckUniqueID() string {
	mutex.Lock()
	defer mutex.Unlock()
	var b strings.Builder
	for _, r := range deviceName {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "PI9696"
	}
	return b.String()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
