#!/usr/bin/env python3
"""Channel-count sweep: record 60 s at each channel count and measure it.

Runs on the unit under test (stdlib only - nothing is installed there). For
each N: generate an N-channel source on the second host, (re)start ITEST-SRC
with N TX channels, set the unit to N channels via the WebUI, bulk-subscribe
PI9696 <- ITEST-SRC, wait for signal on every channel, record a take while
sampling system metrics once a second, then compare the WAV against the source
on the second host (identity mapping, every channel, every sample).

Prerequisites (see test/interop/README.md): the app running with a WebUI
session in $WORK/cookies.txt and its stderr in $WORK/app-sim.err; statime
locked on both hosts; the second host's inferno ALSA plugin undithered so the
reference is bit-exact.
"""
import argparse, glob, http.cookiejar, json, os, re, subprocess, threading, time, urllib.parse, urllib.request

p = argparse.ArgumentParser()
p.add_argument("--channels", default="1,2,4,8,16,32,64,128")
p.add_argument("--seconds", type=int, default=60)
p.add_argument("--src-seconds", type=int, default=300)
p.add_argument("--rx-host", default="root@192.168.10.162")
p.add_argument("--work", default="/var/tmp/pi9696-work")
p.add_argument("--base", default="http://127.0.0.1")
p.add_argument("--keep-wav", action="store_true")
p.add_argument("--control-bin", default="", help="inferno2pipe on the second host for a control capture of the same source (above 16 ch it needs the U13 paging patch)")
a = p.parse_args()

W, RW = a.work, "/var/tmp/pi9696-work"
jar = http.cookiejar.MozillaCookieJar(f"{W}/cookies.txt")
jar.load(ignore_discard=True, ignore_expires=True)
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def http(path, data=None):
    body = urllib.parse.urlencode(data).encode() if data is not None else None
    with opener.open(urllib.request.Request(a.base + path, data=body, method="POST" if data is not None else "GET"), timeout=30) as r:
        return r.read().decode(errors="replace")


def ssh(cmd, timeout=3600):
    return subprocess.run(["ssh", "-o", "BatchMode=yes", a.rx_host, cmd], capture_output=True, text=True, timeout=timeout)


def log(*x):
    print(time.strftime("%H:%M:%S"), *x, flush=True)


# ---- metrics ---------------------------------------------------------------
CLK = os.sysconf("SC_CLK_TCK")


def pids():
    out = {}
    for d in glob.glob("/proc/[0-9]*"):
        try:
            cmd = open(f"{d}/cmdline", "rb").read().replace(b"\0", b" ").decode(errors="replace")
        except OSError:
            continue
        pid = int(d.split("/")[-1])
        if "pi9696" in cmd and ("PI9696_SIM" in cmd or cmd.split(" ")[0].endswith(("/pi9696", "pi9696-sweep"))):
            out.setdefault("app", pid)
        elif cmd.startswith("inferno/target/release/inferno2pipe"):
            out["inferno2pipe"] = pid
        elif cmd.startswith("ffmpeg") and "pcm_s24le" in cmd:
            out["ffmpeg_rec"] = pid
    return out


def proc_cpu(pid):
    try:
        f = open(f"/proc/{pid}/stat").read().rsplit(")", 1)[1].split()
        return int(f[11]) + int(f[12])
    except (OSError, IndexError):
        return None


def proc_rss_mb(pid):
    try:
        for line in open(f"/proc/{pid}/status"):
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) / 1024
    except OSError:
        return None


def sysstat():
    cpu = [int(x) for x in open("/proc/stat").readline().split()[1:]]
    mem = {l.split(":")[0]: int(l.split()[1]) for l in open("/proc/meminfo")}
    disk = 0
    for l in open("/proc/diskstats"):
        f = l.split()
        if f[2] == "mmcblk0":
            disk = int(f[9])
    udp = open("/proc/net/snmp").read().split("Udp:")[2].split()
    temp = int(open("/sys/class/thermal/thermal_zone0/temp").read()) / 1000
    return {"cpu": cpu, "memavail_mb": mem["MemAvailable"] / 1024, "disk_sectors": disk,
            "udp_in": int(udp[0]), "udp_rcvbuf_err": int(udp[4]), "udp_in_err": int(udp[2]),
            "temp_c": temp, "load1": float(open("/proc/loadavg").read().split()[0])}


def throttled():
    try:
        return subprocess.run(["vcgencmd", "get_throttled"], capture_output=True, text=True, timeout=5).stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        return "n/a"


class Sampler(threading.Thread):
    def __init__(self):
        super().__init__(daemon=True)
        self.rows, self.stop = [], threading.Event()

    def run(self):
        prev, prevp, t0 = sysstat(), {}, time.time()
        ps = pids()
        for k, pid in ps.items():
            prevp[k] = proc_cpu(pid)
        while not self.stop.wait(1.0):
            now, ps = sysstat(), pids()
            dc = [x - y for x, y in zip(now["cpu"], prev["cpu"])]
            busy = 1 - (dc[3] + dc[4]) / max(1, sum(dc))
            row = {"t": round(time.time() - t0, 1), "cpu_total_pct": round(100 * busy, 1),
                   "iowait_pct": round(100 * dc[4] / max(1, sum(dc)), 1),
                   "softirq_pct": round(100 * dc[6] / max(1, sum(dc)), 1),
                   "memavail_mb": round(now["memavail_mb"]), "disk_write_mb_s": round((now["disk_sectors"] - prev["disk_sectors"]) * 512 / 1e6, 2),
                   "udp_in_per_s": now["udp_in"] - prev["udp_in"], "udp_rcvbuf_err": now["udp_rcvbuf_err"] - prev["udp_rcvbuf_err"],
                   "udp_in_err": now["udp_in_err"] - prev["udp_in_err"], "temp_c": now["temp_c"], "load1": now["load1"]}
            for k, pid in ps.items():
                c = proc_cpu(pid)
                if c is not None and prevp.get(k) is not None:
                    row[f"{k}_cpu_pct"] = round(100 * (c - prevp[k]) / CLK, 1)
                prevp[k] = c
                row[f"{k}_rss_mb"] = round(proc_rss_mb(pid) or 0, 1)
            self.rows.append(row)
            prev = now


def summarise(rows):
    s = {}
    for k in sorted({k for r in rows for k in r if k != "t"}):
        v = [r[k] for r in rows if k in r]
        if v:
            s[k] = {"avg": round(sum(v) / len(v), 2), "max": max(v), "min": min(v)}
    return s


def loss_total():
    tot = 0
    try:
        for m in re.finditer(r"inferno2pipe: \d+ sample-loss events \((\d+) samples total", open(f"{W}/app-sim.err", errors="replace").read()):
            tot = int(m.group(1))
    except OSError:
        pass
    return tot


# ---- sweep -----------------------------------------------------------------
results = []
for n in [int(x) for x in a.channels.split(",")]:
    r = {"channels": n}
    try:
        srcf = f"{RW}/sweep_src_{n}ch_{a.src_seconds}s.raw"
        log(f"[{n}ch] source"); g = ssh(f"cd {RW} && ([ -s {srcf} ] || python3 gen_signal.py {srcf} --channels {n} --seconds {a.src_seconds})")
        if g.returncode:
            raise RuntimeError("gen_signal: " + g.stderr[-300:])
        ssh(f"systemctl stop itest-src 2>/dev/null; systemctl reset-failed itest-src 2>/dev/null; systemd-run -q --unit=itest-src -p WorkingDirectory={RW} "
            f"-E INFERNO_NAME=ITEST-SRC -E INFERNO_TX_CHANNELS={n} -E INFERNO_RX_CHANNELS=0 -E INFERNO_SAMPLE_RATE=48000 -E RUST_LOG=warn "
            f"aplay -D inferno -f S32_LE -r 48000 -c {n} {srcf}")
        t_src = time.time()
        log(f"[{n}ch] set unit channels")
        http("/api/settings/channels", {"count": n})
        deadline = time.time() + 60
        while time.time() < deadline and f"{n}ch" not in http("/api/status"):
            time.sleep(1)
        time.sleep(5)
        if a.control_bin:
            ssh(f"systemctl stop itest-ctl 2>/dev/null; systemctl reset-failed itest-ctl 2>/dev/null; rm -f {RW}/sweep_ctl_{n}ch.raw; "
                f"systemd-run -q --unit=itest-ctl -p RuntimeMaxSec=900 -E INFERNO_NAME=ITEST-CTL -E INFERNO_PROCESS_ID=7 -E INFERNO_ALT_PORT=10700 "
                f"-E INFERNO_RX_CHANNELS={n} -E INFERNO_TX_CHANNELS=0 -E INFERNO_SAMPLE_RATE=48000 -E RUST_LOG=warn {a.control_bin} -c {n} -o {RW}/sweep_ctl_{n}ch.raw; "
                f"sleep 5; timeout 180 netaudio --no-color subscription add --tx ITEST-SRC --rx ITEST-CTL >/dev/null 2>&1")
        log(f"[{n}ch] subscribe")
        # Raw ARC, not netaudio: netaudio's pre-read of PI9696's subscriptions
        # fails above 32 channels (see arc_subscribe.py); the meters confirm it.
        sub = subprocess.run(["python3", os.path.join(os.path.dirname(os.path.abspath(__file__)), "arc_subscribe.py"),
                              "192.168.10.69", "4440", "ITEST-SRC", str(n)], capture_output=True, text=True, timeout=120)
        r["subscribe"] = (sub.stdout or sub.stderr).strip().splitlines()[-1:]
        deadline, live = time.time() + 150, 0
        while time.time() < deadline:
            m = json.loads(http("/api/meter"))
            ch = m.get("channels") or []
            live = sum(1 for c in ch if c.get("rmsDB", -100) > -60)
            if len(ch) == n and live == n:
                break
            time.sleep(2)
        r["channels_with_signal_before_take"] = live
        if live < n:
            log(f"[{n}ch] only {live}/{n} channels show signal; recording anyway")
        clock = re.search(r"<td>Clock</td><td>([^<]*)", http("/api/config"))
        r["clock"] = clock.group(1) if clock else "?"
        if (time.time() - t_src) + a.seconds + 15 > a.src_seconds:
            log(f"[{n}ch] WARNING: source may loop during the take")
        loss0 = loss_total()
        samp = Sampler(); samp.start()
        log(f"[{n}ch] record {a.seconds}s (clock: {r['clock']})")
        st = http("/api/record/start", {})
        if "RECORDING" not in st:
            raise RuntimeError("record refused: " + re.sub(r"<[^>]+>", " ", st)[:200])
        time.sleep(a.seconds)
        http("/api/record/stop", {})
        time.sleep(3)
        samp.stop.set(); samp.join()
        if a.control_bin:
            ssh("systemctl stop itest-ctl")
        r["throttled"] = throttled()
        r["inferno_lost_samples_during_take"] = loss_total() - loss0
        r["metrics"] = summarise(samp.rows)
        json.dump(samp.rows, open(f"{W}/sweep_metrics_{n}ch.json", "w"))
        wav = max(glob.glob(f"/rec/*/recording_*_ch{n}_48kHz.wav"), key=os.path.getmtime)
        r["wav"], r["wav_bytes"] = wav, os.path.getsize(wav)
        log(f"[{n}ch] copy {r['wav_bytes']/1e6:.0f} MB + compare")
        subprocess.run(["scp", "-q", wav, f"{a.rx_host}:{RW}/sweep_take_{n}ch.wav"], check=True, timeout=1800)
        c = ssh(f"cd {RW} && python3 compare.py {srcf} sweep_take_{n}ch.wav --channels {n} --identity --json sweep_cmp_{n}ch.json >/dev/null 2>&1; cat sweep_cmp_{n}ch.json; rm -f sweep_take_{n}ch.wav")
        cmp_ = json.loads(c.stdout) if c.stdout.strip().startswith("{") else {"error": c.stderr[-300:]}
        r["compare"] = {k: cmp_.get(k) for k in ("verdict", "capture_seconds", "bit_exact_ratio", "max_abs_error_lsb24", "channels_with_errors",
                                                 "offset_changes", "silent_segments_inside_audio", "leading_silence_s", "channel_mapping_problems", "error")}
        if a.control_bin:
            cc = ssh(f"cd {RW} && python3 compare.py {srcf} sweep_ctl_{n}ch.raw --channels {n} --identity --json sweep_ctl_cmp_{n}ch.json >/dev/null 2>&1; cat sweep_ctl_cmp_{n}ch.json; rm -f sweep_ctl_{n}ch.raw")
            ctl = json.loads(cc.stdout) if cc.stdout.strip().startswith("{") else {"error": cc.stderr[-300:]}
            r["control"] = {k: ctl.get(k) for k in ("verdict", "capture_seconds", "bit_exact_ratio", "channels_with_errors", "offset_changes", "silent_segments_inside_audio", "error")}
        if not a.keep_wav:
            os.remove(wav)
        log(f"[{n}ch] {r['compare'].get('verdict')}  exact={r['compare'].get('bit_exact_ratio')}  drops={r['compare'].get('offset_changes')}  "
            f"cpu={r['metrics'].get('cpu_total_pct', {}).get('avg')}%  disk={r['metrics'].get('disk_write_mb_s', {}).get('avg')}MB/s"
            + (f"  control={r['control'].get('verdict')} exact={r['control'].get('bit_exact_ratio')}" if a.control_bin else ""))
    except Exception as e:
        r["error"] = repr(e)
        log(f"[{n}ch] ERROR {e!r}")
        try:
            http("/api/record/stop", {})
        except Exception:
            pass
    results.append(r)
    json.dump(results, open(f"{W}/sweep_results.json", "w"), indent=1)
log("done:", f"{W}/sweep_results.json")
