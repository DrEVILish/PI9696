#!/usr/bin/env python3
"""The unit's inferno receive latency, read and set from a network controller.

Run on the second host (needs netaudio, ssh + HTTP access to the unit):

  1. netaudio reads the unit's latency: active = configured = the unit's
     setting, range 0.5-10 ms.
  2. netaudio sets 2 ms and verifies it; the unit adopts it as its own
     setting (persisted), restarts inferno with it, and still reports 2 ms
     afterwards.
  3. A latency outside 0.5-10 ms is refused and changes nothing.
  4. The unit sets it back from its WebUI (the original preset); netaudio
     then reads that value.

Exit 0 = all checks passed.

Usage: latency_check.py --pia <unit-address>
"""
import argparse, json, os, re, subprocess, sys, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from e2e_bitperfect import PiA, log, netaudio  # noqa: E402

PRESETS_NS = [10_000_000, 6_000_000, 4_000_000, 2_000_000, 1_000_000, 500_000]


def read_latency(name):
    out = netaudio("-n", name, "device", "config", "latency")
    vals = {}
    for key, label in (("active", "Active latency"), ("configured", "Configured latency"), ("range", "Reported latency range")):
        m = re.search(label + r": ([0-9.\-]+) ms", out)
        if m:
            vals[key] = m.group(1)
    return vals, out


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--pia", required=True)
    p.add_argument("--pia-ssh", default=None)
    p.add_argument("--pia-name", default="PI9696-test")
    a = p.parse_args()
    pia = PiA(a.pia, a.pia_ssh or f"root@{a.pia}", a.pia_name)
    pia.login()
    results = []

    def check(name, ok, detail):
        results.append(ok)
        log(("PASS " if ok else "FAIL ") + name + ": " + detail)

    def unit_setting():
        out = pia.sh("python3 -c \"import json;print(json.load(open('/etc/pi9696/config.json')).get('rxLatencyNs'))\"").stdout.strip()
        return int(out) if out.isdigit() else 10_000_000

    def wait_active(ms, timeout=40):
        t = time.time()
        while time.time() - t < timeout:
            v, _ = read_latency(a.pia_name)
            if v.get("active") == ms:
                return v, time.time() - t
            time.sleep(2)
        return read_latency(a.pia_name)[0], None

    orig = unit_setting()
    v, out = read_latency(a.pia_name)
    want = f"{orig / 1e6:g}"
    check("controller reads the latency", v.get("active") == want and v.get("configured") == want and v.get("range") == "0.5-10",
          f"active {v.get('active')} ms, configured {v.get('configured')} ms, range {v.get('range')} ms (unit setting {want} ms)")

    out = netaudio("-n", a.pia_name, "device", "config", "latency", "2")
    check("controller sets 2 ms (verified)", "verified" in out, out.strip().splitlines()[-1] if out.strip() else "no output")
    time.sleep(3)
    check("the unit adopts it", unit_setting() == 2_000_000,
          f"unit setting {unit_setting()} ns; log: " + pia.sh("journalctl -u pi9696 --since '-1min' --no-pager | grep -i 'receive latency' | tail -1").stdout.strip()[-120:])
    v, took = wait_active("2")
    check("after the restart the device runs at 2 ms", took is not None, f"active {v.get('active')} ms")

    out = netaudio("-n", a.pia_name, "device", "config", "latency", "0.15")
    time.sleep(3)
    check("0.15 ms (outside the range) is refused", "verified" not in out and unit_setting() == 2_000_000,
          f"unit setting {unit_setting()} ns; netaudio: {out.strip().splitlines()[-1][:120] if out.strip() else ''}")

    idx = PRESETS_NS.index(orig) if orig in PRESETS_NS else 0
    pia.post("/api/settings/rx-latency", {"idx": str(idx)})
    v, took = wait_active(f"{PRESETS_NS[idx] / 1e6:g}")
    check("WebUI sets it back; the controller reads it", took is not None,
          f"active {v.get('active')} ms, configured {v.get('configured')} ms")
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
