#!/usr/bin/env python3
"""Drift timeline of a timecode capture: the unit's output against the source.

Reads tc_interop.py's capture (TCSINK: channel 1 = the unit's TIMECODE
output, channel 2 = the source looped through the same network), decodes
LTC on both a window at a time, and reports, per window, the offset of the
output's code from the source's at the same capture samples - where both
carry running code. A chase shows as a flat line; drift as a slope.

Usage: tc_drift.py /var/tmp/pi9696-tc/tcsink.raw [--window 10] [--from-s 0]
"""
import argparse, sys

import numpy as np

from tc_interop import FPS, RATE, capture, decode_ltc, offsets


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("capture")
    p.add_argument("--window", type=float, default=10, help="seconds per report line")
    p.add_argument("--from-s", type=float, default=0, help="skip this much of the capture")
    p.add_argument("--max-ms", type=float, default=40, help="ignore offsets beyond this (another code on the output)")
    a = p.parse_args()
    cap = capture(a.capture)
    win = int(a.window * RATE)
    rows = []
    for start in range(int(a.from_s * RATE), len(cap) - win + 1, win):
        seg = cap[start:start + win]
        o = [x * 1000 / FPS for _, x in offsets(decode_ltc(seg[:, 0]), decode_ltc(seg[:, 1]))]
        o = [x for x in o if abs(x) <= a.max_ms]
        if len(o) < a.window * FPS / 2:
            continue
        d = np.array(o)
        rows.append((start / RATE, float(np.median(d)), float(d.min()), float(d.max()), len(d)))
    if not rows:
        print("no window with the output running with the source")
        return 1
    print(f"{'t (s)':>8} {'median ms':>10} {'min ms':>8} {'max ms':>8} frames")
    for t, med, lo, hi, n in rows:
        print(f"{t:8.0f} {med:+10.3f} {lo:+8.3f} {hi:+8.3f} {n:6d}")
    meds = np.array([r[1] for r in rows])
    ts = np.array([r[0] for r in rows])
    slope = np.polyfit(ts, meds, 1)[0] * 3600 if len(rows) > 2 else float("nan")
    print(f"\n{len(rows)} windows over {ts[-1] - ts[0] + a.window:.0f} s: median {np.median(meds):+.3f} ms, "
          f"window medians {meds.min():+.3f}..{meds.max():+.3f} ms, "
          f"all frames {min(r[2] for r in rows):+.3f}..{max(r[3] for r in rows):+.3f} ms, "
          f"trend {slope:+.3f} ms/hour")
    return 0


if __name__ == "__main__":
    sys.exit(main())
