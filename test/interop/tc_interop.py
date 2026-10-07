#!/usr/bin/env python3
"""Two-host timecode test: LTC and MTC in and out of pi9696, recording and chase.

Run on the second host (needs numpy, netaudio, aplay/arecord, the inferno
ALSA plugin, and ssh + HTTP access to the unit). Both hosts must follow
the same PTP leader. The unit's own audio inputs are left as they are (its
current source - music - is what the takes record); only its TIMECODE
channels are routed.

  1. LTC in: the second host transmits LTC (25 fps, from 10:00:00:00) as an
     inferno device TCSRC, routed to the unit's TIMECODE receive channel; the
     unit must lock to it.
  2. Recording: a take with timecode as audio and as metadata. Its last
     channel must decode to continuous LTC, the BWF time reference must
     name the timecode of the take's first sample (checked against the
     recorded code, in samples), and the audio channels must hold sound.
  3. LTC out: the unit's TIMECODE transmit channel is captured (TCSINK
     channel 1) next to the source itself (channel 2, through the same
     network): with the unit idle and locked it relays the input, so both
     decode to continuous code a constant offset apart (reported).
  4. Chase: the take is armed and the source jumps to 3 s before the
     take's start. The unit must start playing as the code enters the take
     and hold it: during the chase its TIMECODE output (the take's code at
     the playhead) must match the source within a frame. Stopping the
     source must stop playback.
  5. MTC: an RTP-MIDI peer here invites the unit, sends MTC from
     11:00:00:00, and receives the unit's MTC output: the unit must lock to
     it, and relay it back with continuous labels.

The unit's timecode settings are restored afterwards. Exit 0 = all checks
passed, 1 = a check failed, 2 = setup failure.

Usage (on the second host):
  tc_interop.py --pia <unit-address> --plugin /path/libasound_module_pcm_inferno.so
"""
import argparse, json, os, re, socket, struct, subprocess, sys, threading, time
import urllib.parse, urllib.request

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from e2e_bitperfect import PiA, fail_setup, inferno_env, log, netaudio, wait_device  # noqa: E402

RATE = 48000
FPS = 25
SPF = RATE // FPS          # samples per frame at 25 fps / 48 kHz
LTC_AMP = 0.25             # about -12 dBFS, like the unit's own generator
SYNC = [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1]


# --- LTC (25 fps) -------------------------------------------------------------

def label(frame):
    frame %= 24 * 3600 * FPS
    return (frame // (3600 * FPS), frame // (60 * FPS) % 60, frame // FPS % 60, frame % FPS)


def fmt(frame):
    return "%02d:%02d:%02d:%02d" % label(frame)


def frame_of(h, m, s, f):
    return ((h * 60 + m) * 60 + s) * FPS + f


def ltc_bits(frame):
    h, m, s, f = label(frame)
    bits = [0] * 80
    def put(at, v, n):
        for i in range(n):
            bits[at + i] = (v >> i) & 1
    put(0, f % 10, 4); put(8, f // 10, 2)
    put(16, s % 10, 4); put(24, s // 10, 3)
    put(32, m % 10, 4); put(40, m // 10, 3)
    put(48, h % 10, 4); put(56, h // 10, 2)
    bits[64:80] = SYNC
    if sum(bits) % 2:
        bits[59] = 1        # polarity correction bit at 25 fps
    return bits


def ltc_levels(frame):
    lvl, out = -1, []
    for b in ltc_bits(frame):
        lvl = -lvl
        out.append(lvl)
        if b:
            lvl = -lvl
        out.append(lvl)
    return out


class LTCGen:
    """LTC as s32 samples from a label, continuous across chunks; jump()
    moves the code (a locate) at the next frame boundary; stop() silences."""
    def __init__(self, start):
        self.frame, self.phase, self.running = start, 0, True
        self.lock = threading.Lock()
        self.cache = {}

    def jump(self, frame):
        with self.lock:
            self.frame, self.phase, self.running = frame, 0, True

    def stop(self):
        with self.lock:
            self.running = False

    def position(self):
        with self.lock:
            return self.frame + self.phase / SPF if self.running else None

    def chunk(self, n):
        out = np.zeros(n)
        with self.lock:
            if self.running:
                i = 0
                while i < n:
                    if self.frame not in self.cache:
                        # 1920 samples a frame / 160 half-bits = 12 each
                        self.cache = {self.frame: np.repeat(np.array(ltc_levels(self.frame), dtype=float), SPF // 160)}
                    lv = self.cache[self.frame]
                    take = min(n - i, SPF - self.phase)
                    out[i:i + take] = lv[self.phase:self.phase + take]
                    i += take
                    self.phase += take
                    if self.phase == SPF:
                        self.phase, self.frame = 0, self.frame + 1
        # one halfway sample at each transition, like the unit's encoder
        prev = np.concatenate(([getattr(self, "last", 0.0)], out[:-1]))
        self.last = out[-1]
        smooth = (out + prev) / 2
        return ((smooth * LTC_AMP * (2 ** 31 - 1)).astype(np.int64) & ~0xFF).astype("<i4")


def decode_ltc(x, thr=1 << 24):
    """[(end_sample, frame)] for every LTC frame in x (int s32 samples)."""
    x = np.asarray(x, dtype=np.int64)
    t = np.where(x > thr, 1, np.where(x < -thr, -1, 0))
    nz = np.flatnonzero(t)
    if len(nz) < 2:
        return []
    filled = t[nz]
    flips = nz[1:][np.diff(filled) != 0]
    edges = []
    for e in flips:
        edges.append(e - 1 if abs(x[e - 1]) <= abs(x[e]) / 2 else e)
    out, period, half, word, nbits = [], RATE / 2000, False, [], 0
    last = None
    for e in edges:
        if last is None:
            last = e
            continue
        g, last = e - last, e
        if g > period * 1.6 or g < period * 0.3:
            half, word = False, []
            if RATE / 2600 <= g <= RATE / 1800:
                period = g
            continue
        if g > period * 0.75:
            period = 0.8 * period + 0.2 * g
            if half:
                half, word = False, []
                continue
            bit = 0
        else:
            period = 0.8 * period + 0.4 * g
            if not half:
                half = True
                continue
            half, bit = False, 1
        word.append(bit)
        if len(word) > 80:
            word.pop(0)
        if len(word) == 80 and word[64:] == SYNC:
            b = word
            v = lambda at, n: sum(b[at + i] << i for i in range(n))
            fr = v(0, 4) + 10 * v(8, 2); s = v(16, 4) + 10 * v(24, 3)
            m = v(32, 4) + 10 * v(40, 3); h = v(48, 4) + 10 * v(56, 2)
            if fr < FPS and s < 60 and m < 60 and h < 24:
                out.append((int(e), frame_of(h, m, s, fr)))
    return out


def runs(decoded):
    """Split decoded frames into runs of consecutive labels."""
    rs, cur = [], []
    for d in decoded:
        if cur and d[1] != cur[-1][1] + 1:
            rs.append(cur)
            cur = []
        cur.append(d)
    if cur:
        rs.append(cur)
    return rs


def code_at(decoded, sample):
    """The source position (frames, fractional) at a capture sample, from
    the decoded frame ending nearest it; None if none within a second."""
    if not decoded:
        return None
    ends = np.array([d[0] for d in decoded])
    i = int(np.argmin(np.abs(ends - sample)))
    if abs(ends[i] - sample) > RATE:
        return None
    return decoded[i][1] + 1 + (sample - ends[i]) / SPF


# --- RTP-MIDI peer --------------------------------------------------------------

class RTPMIDIPeer:
    """An AppleMIDI session initiator: invites the unit, sends MIDI, and
    collects the MIDI it receives."""
    def __init__(self, host, port=5004, name="TCTEST"):
        self.host, self.port, self.name = host, port, name
        self.ssrc = int.from_bytes(os.urandom(4), "big")
        self.token = int.from_bytes(os.urandom(4), "big")
        self.ctrl = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.data = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.ctrl.bind(("0.0.0.0", 15004)); self.data.bind(("0.0.0.0", 15005))
        self.ctrl.settimeout(1); self.data.settimeout(0.2)
        self.seq, self.received, self.alive = 0, [], True
        self.t0 = time.time()

    def exch(self, cmd):
        return (b"\xff\xff" + cmd + struct.pack(">III", 2, self.token, self.ssrc) + self.name.encode() + b"\0")

    def connect(self):
        for sock, port in ((self.ctrl, self.port), (self.data, self.port + 1)):
            for _ in range(10):
                sock.sendto(self.exch(b"IN"), (self.host, port))
                try:
                    pkt, _ = sock.recvfrom(1024)
                except socket.timeout:
                    continue
                if pkt[2:4] == b"OK":
                    break
                if pkt[2:4] == b"NO":
                    fail_setup("the unit refused the RTP-MIDI invitation")
            else:
                fail_setup(f"no RTP-MIDI answer from {self.host}:{port} (is MTC enabled?)")
        threading.Thread(target=self.rx, daemon=True).start()

    def now(self):
        return int((time.time() - self.t0) * 10000)

    def rx(self):
        while self.alive:
            try:
                pkt, addr = self.data.recvfrom(2048)
            except (socket.timeout, OSError):
                continue
            if pkt[:2] == b"\xff\xff":
                if pkt[2:4] == b"CK" and pkt[8] == 0:
                    ts1 = pkt[12:20]
                    self.data.sendto(b"\xff\xffCK" + struct.pack(">I", self.ssrc) + b"\x01\0\0\0" + ts1 + struct.pack(">Q", self.now()) + b"\0" * 8, addr)
                continue
            if len(pkt) > 13 and pkt[0] >> 6 == 2:
                body = pkt[12:]
                n = body[0] & 0x0F
                cmds = body[1:1 + n]
                if len(cmds) >= 2 and cmds[0] == 0xF1:
                    self.received.append((time.time(), cmds[1]))

    def send(self, msg):
        self.seq = (self.seq + 1) & 0xFFFF
        pkt = struct.pack(">BBHII", 0x80, 0x61, self.seq, self.now() & 0xFFFFFFFF, self.ssrc) + bytes([len(msg)]) + bytes(msg)
        self.data.sendto(pkt, (self.host, self.port + 1))

    def close(self):
        self.alive = False
        self.ctrl.sendto(b"\xff\xffBY" + struct.pack(">III", 2, self.token, self.ssrc), (self.host, self.port))


def mtc_qf(piece, frame):
    h, m, s, f = label(frame)
    v = [f & 15, f >> 4, s & 15, s >> 4, m & 15, m >> 4, h & 15, (h >> 4) | (1 << 1)][piece]  # rate code 1 = 25 fps
    return [0xF1, piece << 4 | v]


def mtc_labels(received):
    """Labels assembled from received quarter frames (forward 0..7)."""
    out, pieces, nxt = [], {}, 0
    for t, b in received:
        p, v = b >> 4, b & 15
        if p != nxt:
            nxt, pieces = 0, {}
            if p != 0:
                continue
        pieces[p] = v
        nxt = (p + 1) % 8
        if p == 7:
            f = pieces[0] | (pieces[1] & 1) << 4; s = pieces[2] | (pieces[3] & 3) << 4
            m = pieces[4] | (pieces[5] & 3) << 4; h = pieces[6] | (pieces[7] & 1) << 4
            out.append((t, frame_of(h, m, s, f)))
    return out


# --- the test ----------------------------------------------------------------------

class Checks:
    def __init__(self):
        self.results = []

    def check(self, name, ok, detail):
        self.results.append({"check": name, "ok": bool(ok), "detail": detail})
        log(("PASS " if ok else "FAIL ") + name + ": " + detail)


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--pia", required=True)
    p.add_argument("--pia-ssh", default=None)
    p.add_argument("--pia-name", default=None, help="the unit's inferno name (default: read from netaudio)")
    p.add_argument("--plugin", required=True)
    p.add_argument("--work", default="/var/tmp/pi9696-tc")
    a = p.parse_args()
    os.makedirs(a.work, exist_ok=True)
    asoundrc = f"{a.work}/asoundrc"
    open(asoundrc, "w").write(f'pcm_type.inferno {{ lib "{a.plugin}" }}\npcm.inferno {{ type inferno }}\n')
    pia = PiA(a.pia, a.pia_ssh or f"root@{a.pia}", a.pia_name or "")
    pia.login()
    ck = Checks()

    def tc():
        return json.loads(pia.get("/api/timecode"))

    def setting(name, **form):
        pia.post(f"/api/settings/timecode-{name}", {k: str(v) for k, v in form.items()})

    cfg = pia.text("/api/config")
    m = re.search(r"Channels (\d+)", cfg)
    n_audio = int(m.group(1)) if m else fail_setup("no channel count in the config")
    name = a.pia_name
    if not name:
        dl = netaudio("device", "list")
        cands = [l.split()[0] for l in dl.splitlines() if l.split() and l.split()[0].upper().startswith("PI9696")]
        name = cands[0] if cands else fail_setup("the unit is not in netaudio's device list")
    log(f"unit {name}: {n_audio} audio channels + TIMECODE")
    before = tc()
    orig = pia.sh("python3 -c \"import json;c=json.load(open('/etc/pi9696/config.json'));print(json.dumps({k:c.get(k) for k in ('tcSource','tcRecord','tcOutput','tcRate','tcMTCPeer')}))\" 2>/dev/null").stdout.strip()
    log(f"unit timecode settings before: {orig or '(defaults)'}; status {before}")

    gen = LTCGen(frame_of(10, 0, 0, 0))
    procs = []
    stop_feed = threading.Event()
    cap = f"{a.work}/tcsink.raw"
    try:
        src = subprocess.Popen(["aplay", "-q", "-D", "inferno", "-f", "S32_LE", "-r", str(RATE), "-c", "1", "-t", "raw", "-"],
                               stdin=subprocess.PIPE, env=inferno_env(asoundrc, "TCSRC", 1, 0, {"INFERNO_TX_SOURCE_BIT_DEPTH": "24"}),
                               stdout=subprocess.DEVNULL, stderr=open(f"{a.work}/src.log", "w"))
        procs.append(src)

        def feed():
            while not stop_feed.is_set():
                try:
                    src.stdin.write(gen.chunk(1024).tobytes())
                    src.stdin.flush()
                except (BrokenPipeError, ValueError):
                    return
        threading.Thread(target=feed, daemon=True).start()
        sink = subprocess.Popen(["arecord", "-q", "-D", "inferno", "-c", "2", "-r", str(RATE), "-f", "S32_LE", "-t", "raw", cap],
                                env=inferno_env(asoundrc, "TCSINK", 0, 2, {"INFERNO_ALT_PORT": "10300", "INFERNO_PROCESS_ID": "1"}),
                                stdout=subprocess.DEVNULL, stderr=open(f"{a.work}/sink.log", "w"))
        procs.append(sink)
        cap_t0 = time.time()
        wait_device("TCSRC")
        wait_device("TCSINK")
        for tx, rx in ((f"1@TCSRC", f"TIMECODE@{name}"), (f"TIMECODE@{name}", "1@TCSINK"), ("1@TCSRC", "2@TCSINK")):
            out = netaudio("subscription", "add", "--tx", tx, "--rx", rx)
            log(f"route {tx} -> {rx}: {out.strip()[-80:] or 'ok'}")

        # 1. LTC in
        setting("source", idx=1)
        setting("rate", idx=2)
        setting("output", enabled="on")
        setting("record", idx=2)
        t = time.time()
        while time.time() - t < 40 and not tc()["locked"]:
            time.sleep(1)
        st, pos = tc(), gen.position()
        ck.check("LTC in locks", st["locked"] and st["source"] == "LTC",
                 f"after {time.time() - t:.0f} s: {st['inLong']} (source now at {fmt(int(pos))})")
        if not st["locked"]:
            raise RuntimeError("no LTC lock")
        # (What the unit reads is checked to the sample through the take
        # below: the generator's own position runs ahead of the wire by
        # aplay's buffer, so it is no reference.)

        # 2. a take with timecode as audio and metadata
        newest = pia.sh("ls -t /rec/*/*.wav 2>/dev/null | head -1").stdout.strip()
        pia.post("/api/record/start")
        time.sleep(6)
        pia.post("/api/record/stop")
        time.sleep(3)
        take = pia.sh("ls -t /rec/*/*.wav 2>/dev/null | head -1").stdout.strip()
        if not take or take == newest:
            raise RuntimeError("no take was recorded (clock gate? see the unit's log)")
        local = f"{a.work}/take.wav"
        subprocess.run(["scp", "-q", f"{pia.ssh}:{take}", local], check=True)
        side = json.loads(pia.sh(f"cat '{os.path.splitext(take)[0]}.channels.json'").stdout or "{}")
        data = open(local, "rb").read()
        posd, nch, ref = 12, None, None
        while posd + 8 <= len(data):
            cid, csz = data[posd:posd + 4], struct.unpack("<I", data[posd + 4:posd + 8])[0]
            if cid == b"fmt ":
                nch = struct.unpack("<H", data[posd + 10:posd + 12])[0]
            if cid == b"bext":
                ref = struct.unpack("<Q", data[posd + 8 + 338:posd + 8 + 346])[0]
            if cid == b"data":
                start = posd + 8
                break
            posd += 8 + csz + (csz & 1)
        raw = np.frombuffer(data[start:start + (len(data) - start) // (3 * nch) * 3 * nch], dtype=np.uint8).reshape(-1, 3).astype(np.int64)
        v = (raw[:, 0] << 8 | raw[:, 1] << 16 | raw[:, 2] << 24)
        v = np.where(v >= 1 << 31, v - (1 << 32), v).reshape(-1, nch)
        ck.check("take has the TIMECODE channel last", nch == n_audio + 1 and side.get("channels", [{}])[-1].get("name") == "TIMECODE",
                 f"{os.path.basename(take)}: {nch} channels, last named {side.get('channels', [{}])[-1].get('name')}")
        dec = decode_ltc(v[:, -1])
        rs = runs(dec)
        ck.check("recorded TIMECODE is continuous LTC", len(rs) == 1 and len(dec) > 5 * FPS,
                 f"{len(dec)} frames in {len(rs)} run(s), {fmt(dec[0][1]) if dec else '-'} .. {fmt(dec[-1][1]) if dec else '-'}")
        if dec and ref is not None:
            e, fr = dec[0]
            recorded = (fr + 1) * SPF - e
            ck.check("BWF time reference = the recorded code at the first sample", abs(recorded - ref) <= 2,
                     f"bext {ref} ({fmt(ref // SPF)}), the code says {recorded} ({recorded - ref:+d} samples); sidecar {side.get('timecode')}")
        rms = 20 * np.log10(np.sqrt(np.mean((v[:, :n_audio] / 2 ** 31) ** 2)) + 1e-12)
        ck.check("the take's audio channels hold sound", rms > -50, f"audio RMS {rms:.1f} dBFS")

        # 3. LTC out (idle: relays the input)
        time.sleep(4)
        caps = np.fromfile(cap, dtype="<i4")
        caps = caps[: len(caps) // 2 * 2].reshape(-1, 2)
        win = caps[-RATE * 3:]
        d_out, d_src = decode_ltc(win[:, 0]), decode_ltc(win[:, 1])
        ok = len(runs(d_out)) == 1 and len(d_out) > 2 * FPS
        offs = [code_at(d_out, e) - code_at(d_src, e) for e, _ in d_src if code_at(d_out, e) is not None] if d_src else []
        off = float(np.median(offs)) if offs else float("nan")
        ck.check("LTC out relays the input, continuous", ok and abs(off) < 1.0,
                 f"{len(d_out)} frames, output - source = {off * 1000 / FPS:+.1f} ms (median)")

        # 4. chase
        rel = os.path.relpath(take, "/rec")
        pia.post("/api/playback/select", {"file": rel})
        r = json.loads(pia.post("/api/timecode/arm", {"arm": "1"}) or "{}")
        ck.check("chase arms the take", r.get("armed"), f"{r.get('take')}: {r.get('chase')}")
        take_start = ref // SPF
        mark = len(np.fromfile(cap, dtype="<i4")) // 2
        gen.jump(take_start - 3 * FPS)
        log(f"source jumped to {fmt(take_start - 3 * FPS)} (take starts at {fmt(take_start)})")
        t = time.time()
        while time.time() - t < 10 and "Playing" not in pia.text("/api/status") and tc().get("chase") != "chasing":
            time.sleep(0.25)
        started = time.time() - t
        ck.check("chase starts the take as the code enters it", 2 <= started <= 6, f"playing {started:.1f} s after the jump (code entered the take at 3 s)")
        time.sleep(4)
        caps = np.fromfile(cap, dtype="<i4")
        caps = caps[: len(caps) // 2 * 2].reshape(-1, 2)[mark:]
        win = caps[-RATE * 3:]
        d_out, d_src = decode_ltc(win[:, 0]), decode_ltc(win[:, 1])
        offs = [code_at(d_out, e) - code_at(d_src, e) for e, _ in d_src if code_at(d_out, e) is not None]
        off = float(np.median(offs)) if offs else float("nan")
        spread = (max(offs) - min(offs)) * 1000 / FPS if offs else float("nan")
        ck.check("chased playback runs with the code", offs and abs(off) < 1.0,
                 f"the take's code out - source = {off * 1000 / FPS:+.1f} ms (median), spread {spread:.1f} ms over 3 s")
        gen.stop()
        t = time.time()
        while time.time() - t < 5 and tc().get("chase") == "chasing":
            time.sleep(0.25)
        st = tc()
        ck.check("code stopping stops the chase, take stays armed", st.get("armed") and st.get("chase") != "chasing",
                 f"{time.time() - t:.1f} s: {st.get('chase')}")
        pia.post("/api/timecode/arm", {"arm": "0"})
        logs = pia.sh("journalctl -u pi9696 --since '-2min' --no-pager | grep -E 'Chase:|Timecode in' | tail -12").stdout
        log("unit log:\n" + logs)

        # 5. MTC in and out
        setting("source", idx=2)
        time.sleep(2)
        peer = RTPMIDIPeer(a.pia)
        peer.connect()
        start, q, t0 = frame_of(11, 0, 0, 0), 0, time.time()
        qdur = 1 / (4 * FPS)
        while time.time() - t0 < 6:
            fr = start + q // 4
            group = fr - (fr - start) % 2
            peer.send(mtc_qf(q % 8, group))
            q += 1
            time.sleep(max(0, t0 + q * qdur - time.time()))
        st = tc()
        mtc_pos = start + (time.time() - t0) * FPS
        got = re.search(r"(\d\d):(\d\d):(\d\d)[:;](\d\d)", st["in"])
        d = frame_of(*map(int, got.groups())) - mtc_pos if got else float("nan")
        ck.check("MTC in locks", st["locked"] and st["source"] == "MTC" and abs(d) <= 3, f"{st['inLong']} ({d:+.1f} frames from what was sent)")
        labels = mtc_labels(peer.received)
        steps = [b[1] - a_[1] for a_, b in zip(labels, labels[1:])]
        late = [x for x in labels if x[0] > t0 + 2]
        ck.check("MTC out relays it back, continuous", len(late) > 10 and all(s_ == 2 for s_ in steps[-20:]),
                 f"{len(labels)} labels received, last {fmt(labels[-1][1]) if labels else '-'}, steps {sorted(set(steps[-20:]))}")
        peer.close()
    except RuntimeError as e:
        ck.check("setup", False, str(e))
    finally:
        stop_feed.set()
        # put the unit's timecode settings back as they were
        o = json.loads(orig) if orig.startswith("{") else {}
        idx = lambda names, v, d: str(names.index(v)) if v in names else d
        pia.post("/api/settings/timecode-source", {"idx": idx(["Off", "LTC", "MTC"], o.get("tcSource"), "0")})
        pia.post("/api/settings/timecode-record", {"idx": idx(["Off", "Metadata", "Audio"], o.get("tcRecord"), "1")})
        pia.post("/api/settings/timecode-rate", {"idx": idx(["23.976", "24", "25", "29.97 DF", "29.97", "30"], o.get("tcRate"), "2")})
        pia.post("/api/settings/timecode-output", {"enabled": "on"} if o.get("tcOutput") else {})
        pia.post("/api/settings/timecode-peer", {"peer": o.get("tcMTCPeer") or ""})
        for pr in procs:
            if pr.poll() is None:
                pr.terminate()
        netaudio("--name", "TCSINK", "subscription", "remove", "--all")
    print(json.dumps(ck.results, indent=2))
    return 0 if ck.results and all(r["ok"] for r in ck.results) else 1


if __name__ == "__main__":
    sys.exit(main())
