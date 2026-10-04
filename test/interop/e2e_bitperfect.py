#!/usr/bin/env python3
"""Two-Pi end-to-end bit-perfect test: record on Pi-A, play back, record on Pi-B.

Run on Pi-B (the second host; needs numpy, netaudio, aplay/arecord, the
inferno ALSA plugin and ssh + HTTP access to Pi-A, which runs the pi9696 app).

  1. Pi-B transmits a sample-accurate timecode on every channel: each sample is
     a 24-bit counter, v = ((frame + ch*OFF) mod M) + 1, so the frame number
     is recoverable from any sample of any channel. It is sent with
     INFERNO_TX_SOURCE_BIT_DEPTH=24, i.e. untouched.
  2. netaudio routes it to Pi-A, and Pi-A records a take through the app
     (WebUI record start/stop, same path as the button).
  3. netaudio routes Pi-A's transmit channels to a receiver on Pi-B, Pi-A
     plays the take back through the app (play button), Pi-B records it.
  4. The two recordings are aligned on the timecode and compared bit for bit.

Both hosts must share one PTP clock (Pi-A's recording gate refuses takes
otherwise; see DEPLOYMENT.md "Two inferno hosts"). With the same clock and
the same audio path the two recordings must be identical: every frame of the
take present in Pi-B's recording with the same 24-bit value on every channel.
Anything else is a bug, or a part of the path that is not bit-transparent.

Exit 0 = bit-perfect, 1 = differences found, 2 = setup failure.

Usage (on Pi-B):
  e2e_bitperfect.py --pia 192.0.2.69 --plugin /path/libasound_module_pcm_inferno.so
"""
import argparse, http.cookiejar, json, os, re, shlex, struct, subprocess, sys, time
import urllib.parse, urllib.request

import numpy as np

M = (1 << 23) - 1
OFF = 100003
RATE = 48000


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def fail_setup(msg):
    log(f"SETUP FAILED: {msg}")
    sys.exit(2)


# --- signal and analysis ----------------------------------------------------

def gen_timecode(path, ch, secs):
    offs = (np.arange(ch, dtype=np.int64) * OFF)[None, :]
    with open(path, "wb") as f:
        for start in range(0, secs * RATE, RATE):
            n = np.arange(start, min(start + RATE, secs * RATE), dtype=np.int64)[:, None]
            f.write((((n + offs) % M + 1).astype(np.int32) << 8).astype("<i4").tobytes())


def load_wav24(path, ch):
    data = open(path, "rb").read()
    if data[:4] not in (b"RIFF", b"RF64") or data[8:12] != b"WAVE":
        sys.exit(f"{path}: not a WAV file")
    pos, start, size, bits, nch = 12, None, None, None, None
    while pos + 8 <= len(data):
        cid, csz = data[pos:pos + 4], struct.unpack("<I", data[pos + 4:pos + 8])[0]
        if cid == b"fmt ":
            nch, = struct.unpack("<H", data[pos + 10:pos + 12])
            bits, = struct.unpack("<H", data[pos + 22:pos + 24])
        if cid == b"data":
            start = pos + 8
            size = len(data) - start if csz in (0, 0xFFFFFFFF) or start + csz > len(data) else csz
            break
        pos += 8 + csz + (csz & 1)
    if nch != ch or bits != 24:
        sys.exit(f"{path}: {nch} ch / {bits}-bit, expected {ch} ch / 24-bit")
    raw = np.frombuffer(data[start:start + size - size % (3 * ch)], dtype=np.uint8).reshape(-1, 3).astype(np.int32)
    v = raw[:, 0] | (raw[:, 1] << 8) | (raw[:, 2] << 16)
    return np.where(v & 0x800000, v - (1 << 24), v).reshape(-1, ch).astype(np.int64)


def load_raw32(path, ch):
    raw = np.fromfile(path, dtype="<i4")
    return (raw[: len(raw) // ch * ch] >> 8).reshape(-1, ch).astype(np.int64)


def timecode(x):
    """Frame number carried by each sample (valid only where x != 0)."""
    return (x - 1 - (np.arange(x.shape[1], dtype=np.int64) * OFF)[None, :]) % M


def describe(x, name):
    """Integrity of one recording: the longest run of frames whose timecode is
    identical across channels and rises by exactly 1 per frame."""
    full = (x != 0).all(axis=1)
    rows = np.flatnonzero(full)
    if len(rows) == 0:
        return {"name": name, "frames": len(x), "audio": False}
    k = timecode(x)
    agree = (k[rows] == k[rows, :1]).all(axis=1)
    kr = k[rows, 0]
    ok = (np.diff(rows) == 1) & (np.diff(kr) % M == 1) & agree[1:] & agree[:-1]
    breaks = np.flatnonzero(~ok)
    starts = np.concatenate(([0], breaks + 1))
    ends = np.concatenate((breaks, [len(rows) - 1]))
    best = int(np.argmax(ends - starts))
    s0, s1 = int(rows[starts[best]]), int(rows[ends[best]])
    return {
        "name": name, "frames": len(x), "audio": True,
        "stream_first_frame": s0, "stream_last_frame": s1,
        "stream_seconds": round((s1 - s0 + 1) / RATE, 3),
        "timecode_first": int(k[s0, 0]), "timecode_last": int(k[s1, 0]),
        "misaligned_frames": int((~agree).sum()),
        "runs": len(starts),
        "nonzero_frames_outside_stream": int((x[:s0] != 0).any(axis=1).sum() + (x[s1 + 1:] != 0).any(axis=1).sum()),
    }


def compare(a, b):
    """Align b to a on the timecode and compare every sample of a's stream."""
    da, db = describe(a, "Pi-A take"), describe(b, "Pi-B capture")
    result = {"pi_a_take": da, "pi_b_capture": db}
    if not da["audio"] or not db["audio"]:
        result["verdict"] = "no audio in " + ("the take" if not da["audio"] else "Pi-B's capture")
        return result, False
    a_seg = a[da["stream_first_frame"]: da["stream_last_frame"] + 1]
    # where does the take's first frame appear in Pi-B's recording?
    kb = timecode(b)[:, 0]
    full_b = (b != 0).all(axis=1)
    hits = np.flatnonzero(full_b & (kb == da["timecode_first"]))
    if len(hits) == 0:
        # take start missing in B: align on the first take frame B does have
        common = np.flatnonzero(full_b & (((kb - da["timecode_first"]) % M) < len(a_seg)))
        if len(common) == 0:
            result["verdict"] = "Pi-B's capture holds no frame of the take"
            return result, False
        bpos = int(common[0])
        apos = int((kb[bpos] - da["timecode_first"]) % M)
    else:
        bpos, apos = int(hits[0]), 0
    n = min(len(a_seg) - apos, len(b) - bpos)
    a_cmp, b_cmp = a_seg[apos:apos + n], b[bpos:bpos + n]
    diff = a_cmp != b_cmp
    bad_frames = np.flatnonzero(diff.any(axis=1))
    result.update({
        "take_frames": int(len(a_seg)),
        "take_frames_missing_at_start_in_b": apos,
        "take_frames_missing_at_end_in_b": int(len(a_seg) - apos - n),
        "frames_compared": int(n),
        "samples_compared": int(n * a.shape[1]),
        "samples_different": int(diff.sum()),
        "frames_different": int(len(bad_frames)),
    })
    if len(bad_frames):
        f = int(bad_frames[0])
        ch = int(np.flatnonzero(diff[f])[0])
        result["first_difference"] = {
            "take_frame": apos + f, "seconds": round((apos + f) / RATE, 4), "channel": ch + 1,
            "pi_a": int(a_cmp[f, ch]), "pi_b": int(b_cmp[f, ch]),
            "max_abs_diff": int(np.abs(a_cmp - b_cmp)[diff].max()),
        }
    perfect = (result["samples_different"] == 0 and apos == 0 and result["take_frames_missing_at_end_in_b"] == 0
               and da["misaligned_frames"] == 0 and da["runs"] == 1)
    result["verdict"] = "BIT-PERFECT" if perfect else "DIFFERENT"
    return result, perfect


# --- Pi-A control (pi9696 WebUI) ----------------------------------------------

class PiA:
    def __init__(self, host, ssh, name):
        self.host, self.ssh, self.name = host, ssh, name
        self.jar = http.cookiejar.CookieJar()
        self.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar),
                                                NoRedirect())

    def sh(self, cmd):
        return subprocess.run(["ssh", "-o", "BatchMode=yes", self.ssh, cmd], capture_output=True, text=True, timeout=120)

    def login(self):
        out = self.sh("journalctl -u pi9696 --no-pager | grep 'remote access code' | tail -1").stdout
        m = re.search(r"code[^:]*:\s*([A-Z0-9 ]+)", out)
        if not m:
            fail_setup("no access code in Pi-A's journal")
        token = m.group(1).replace(" ", "")
        self.post("/login", {"token": token})
        if not any(c.name == "pi9696_session" for c in self.jar):
            fail_setup("login to Pi-A's WebUI failed")

    def post(self, path, form=None):
        data = urllib.parse.urlencode(form or {}).encode()
        req = urllib.request.Request(f"http://{self.host}{path}", data=data, method="POST",
                                     headers={"Origin": f"http://{self.host}"})
        try:
            return self.http.open(req, timeout=30).read().decode()
        except urllib.error.HTTPError as e:
            return e.read().decode()

    def get(self, path):
        return self.http.open(f"http://{self.host}{path}", timeout=30).read().decode()

    def text(self, path):
        return re.sub(r"\s+", " ", re.sub(r"<[^>]+>", " ", self.get(path)))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


# --- inferno instances on Pi-B -------------------------------------------------

def inferno_env(asoundrc, name, tx, rx, extra=None):
    env = dict(os.environ, ALSA_CONFIG_PATH=f"/usr/share/alsa/alsa.conf:{asoundrc}", RUST_LOG="warn",
               INFERNO_NAME=name, INFERNO_TX_CHANNELS=str(tx), INFERNO_RX_CHANNELS=str(rx),
               INFERNO_SAMPLE_RATE=str(RATE))
    env.update(extra or {})
    return env


def netaudio(*args, check=False):
    r = subprocess.run(["netaudio", *args], capture_output=True, text=True, timeout=120)
    out = re.sub(r"\x1b\[[0-9;]*m", "", r.stdout + r.stderr)
    if check and r.returncode != 0:
        fail_setup(f"netaudio {' '.join(args)}: {out.strip()[-300:]}")
    return out


def wait_device(name, timeout=40):
    t = time.time()
    while time.time() - t < timeout:
        if re.search(rf"^{re.escape(name)}\s+online", netaudio("device", "list"), re.M):
            return
        time.sleep(2)
    fail_setup(f"device {name} never appeared in netaudio")


def subscriptions_on(rx_device):
    out = netaudio("subscription", "list")
    return [l for l in out.splitlines() if re.search(rf"\b{re.escape(rx_device)}\b", l) and re.match(r"\s*RX ", l)]


def route(tx_device, rx_device, ch):
    """Route tx 1..ch -> rx 1..ch with one bulk netaudio call (one call per
    channel took ~6 s each) and wait until all are connected."""
    out = netaudio("subscription", "add", "--tx", tx_device, "--rx", rx_device)
    if "rror" in out:
        log("  netaudio: " + out.strip()[-300:])
    t = time.time()
    while time.time() - t < 30:
        lines = subscriptions_on(rx_device)
        if len(lines) >= ch and not any("Unresolved" in l for l in lines):
            return
        time.sleep(2)
    lines = subscriptions_on(rx_device)
    log(f"  warning: {len(lines)} of {ch} subscriptions on {rx_device} after 30 s; status: "
        + ", ".join(sorted(set(re.sub(r'.*\s', '', l) for l in lines))))


def unroute(rx_device, ch):
    netaudio("--name", rx_device, "subscription", "remove", "--all")


# --- the test ---------------------------------------------------------------------

def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--pia", default="192.0.2.69", help="Pi-A address (pi9696 WebUI)")
    p.add_argument("--pia-ssh", default=None, help="ssh target for Pi-A (default root@<pia>)")
    p.add_argument("--pia-name", default="PI9696", help="Pi-A's inferno device name")
    p.add_argument("--plugin", required=True, help="inferno ALSA plugin (.so) for Pi-B's instances")
    p.add_argument("--secs", type=int, default=20, help="take length")
    p.add_argument("--work", default="/var/tmp/pi9696-e2e")
    p.add_argument("--keep", action="store_true", help="keep the recordings and signal")
    a = p.parse_args()
    os.makedirs(a.work, exist_ok=True)
    asoundrc = f"{a.work}/asoundrc"
    open(asoundrc, "w").write(f'pcm_type.inferno {{ lib "{a.plugin}" }}\npcm.inferno {{ type inferno }}\n')

    pia = PiA(a.pia, a.pia_ssh or f"root@{a.pia}", a.pia_name)
    pia.login()
    cfg = pia.text("/api/config")
    m = re.search(r"Sample Rate (\d+)kHz Channels (\d+)", cfg)
    if not m or int(m.group(1)) * 1000 != RATE:
        fail_setup(f"Pi-A config not 48 kHz: {cfg[:120]}")
    ch = int(m.group(2))
    clock = re.search(r"Clock (.*?) Network", cfg)
    log(f"Pi-A: {ch} ch, 48 kHz, clock: {clock.group(1) if clock else '?'}")
    if not clock or not clock.group(1).startswith("Synced"):
        fail_setup("Pi-A's clock is not synced to the network PTP leader (recording would be refused)")
    if subscriptions_on(a.pia_name):
        fail_setup(f"{a.pia_name} already has subscriptions; refusing to change them")

    sig = f"{a.work}/timecode.raw"
    # long enough to outlast discovery and routing; stopped explicitly
    log(f"generating {ch}-ch timecode ({a.secs + 300} s)")
    gen_timecode(sig, ch, a.secs + 300)

    procs = []
    try:
        # 1-2: Pi-B transmits the timecode, Pi-A records it
        src = subprocess.Popen(["aplay", "-q", "-D", "inferno", "-f", "S32_LE", "-r", str(RATE), "-c", str(ch), sig],
                               env=inferno_env(asoundrc, "E2ESRC", ch, 0, {"INFERNO_TX_SOURCE_BIT_DEPTH": "24"}),
                               stdout=subprocess.DEVNULL, stderr=open(f"{a.work}/src.log", "w"))
        procs.append(src)
        wait_device("E2ESRC")
        log(f"routing E2ESRC -> {a.pia_name} ({ch} ch)")
        route("E2ESRC", a.pia_name, ch)
        time.sleep(3)
        newest_before = pia.sh("ls -t /rec/*/*.wav 2>/dev/null | head -1").stdout.strip()
        log("Pi-A: record")
        pia.post("/api/record/start")
        time.sleep(1)
        if "Recording" not in pia.text("/api/status") and "REC" not in pia.text("/api/status"):
            log("  warning: status does not show recording: " + pia.text("/api/status")[:160])
        time.sleep(a.secs)
        pia.post("/api/record/stop")
        log("Pi-A: stop")
        time.sleep(3)
        take = pia.sh("ls -t /rec/*/*.wav 2>/dev/null | head -1").stdout.strip()
        if not take or take == newest_before:
            fail_setup("Pi-A produced no new take (clock gate? see Pi-A's log)")
        log(f"take: {take}")
        unroute(a.pia_name, ch)
        src.terminate(); src.wait(timeout=10)

        # 3: Pi-A plays the take back, Pi-B records it
        cap = f"{a.work}/pib_capture.raw"
        # no fixed duration: stopped once the playback has finished
        sink = subprocess.Popen(["arecord", "-q", "-D", "inferno", "-c", str(ch), "-r", str(RATE),
                                 "-f", "S32_LE", "-t", "raw", cap],
                                env=inferno_env(asoundrc, "E2ESINK", 0, ch),
                                stdout=subprocess.DEVNULL, stderr=open(f"{a.work}/sink.log", "w"))
        procs.append(sink)
        wait_device("E2ESINK")
        log(f"routing {a.pia_name} -> E2ESINK ({ch} ch)")
        route(a.pia_name, "E2ESINK", ch)
        time.sleep(3)
        log("Pi-A: play")
        pia.post("/api/input/button/play")
        time.sleep(1)
        log("  status: " + pia.text("/api/status")[:120])
        t = time.time()
        while time.time() - t < a.secs + 60 and "Playing back" in pia.text("/api/status"):
            time.sleep(1)
        log(f"Pi-A: playback ended after {time.time() - t + 1:.0f} s")
        time.sleep(2)
        sink.send_signal(2)  # SIGINT: arecord closes the file cleanly
        sink.wait(timeout=20)
        unroute("E2ESINK", ch)

        # 4: fetch the take, align, compare
        local_take = f"{a.work}/pia_take.wav"
        r = subprocess.run(["scp", "-q", f"{pia.ssh}:{take}", local_take], capture_output=True, text=True)
        if r.returncode:
            fail_setup(f"fetching the take: {r.stderr}")
        result, perfect = compare(load_wav24(local_take, ch), load_raw32(cap, ch))
        result["take"] = take
        print(json.dumps(result, indent=2))
        log("RESULT: " + result["verdict"])
        return 0 if perfect else 1
    finally:
        for pr in procs:
            if pr.poll() is None:
                pr.terminate()
        if not a.keep:
            for f in ("timecode.raw",):
                try:
                    os.remove(f"{a.work}/{f}")
                except OSError:
                    pass


if __name__ == "__main__":
    sys.exit(main())
