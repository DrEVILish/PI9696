#!/usr/bin/env python3
"""Score a pi9696 take of real music against the source tracks.

Usage: music_score.py TAKE.wav SOURCE_DIR [--block SECONDS]

The playlist source (an inferno-network USB interface fed by a computer)
plays FLACs that the computer resamples (44.1 kHz -> 48 kHz) on the way, so
a bit-exact match is impossible; this measures what the recorder can be
held to instead. Per block of the take:

  track/offset  found by FFT cross-correlation of a 4 kHz mono copy against
                every source track, refined to the sample at 48 kHz
  match         1 - residual energy / take energy, after a least-squares
                gain fit (per 50 ms, interpolated, so volume automation or
                AGC on the playing computer is not counted against the
                recorder; --gain-window 0 for one gain per block), in band
                (20 Hz - 16 kHz, where the two resamplers agree). Target > 99%.
  lag           the matched source offset advances exactly one block per
                block when no sample is dropped or repeated
  clicks        samples where the high-passed (2 kHz) residual exceeds 12x
                its block RMS: impulses in the take that the source lacks
  dropouts      runs of >= 48 exact-zero frames while the source plays

Needs numpy, scipy and ffmpeg: run it on the dev server, never the unit.
Exit 0 when every non-quiet block matches > 99% with no clicks or dropouts.
"""
import argparse
import glob
import os
import subprocess
import sys

import numpy as np
from scipy import signal

SR = 48000
DEC = 12  # 4 kHz search copy


def load(path):
    raw = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", path, "-af", "aresample=resampler=soxr:precision=28",
         "-ar", str(SR), "-ac", "2", "-f", "f32le", "-"],
        capture_output=True, check=True).stdout
    return np.frombuffer(raw, np.float32).reshape(-1, 2)


def decimate(x):
    return signal.decimate(x.mean(axis=1), DEC, ftype="fir").astype(np.float32)


class Library:
    """4 kHz mono copies of every track for the search; full-rate tracks are
    decoded on demand and only the last two kept (the whole playlist at
    48 kHz would be several GB)."""

    def __init__(self, folder):
        self.paths = sorted(glob.glob(os.path.join(folder, "*.flac")))
        self.names = [os.path.basename(p) for p in self.paths]
        self.offsets, parts, pos = [], [], 0
        for p in self.paths:
            d = decimate(load(p))
            self.offsets.append(pos)
            parts.append(d)
            pos += len(d)
        self.cat = np.concatenate(parts)
        self.cache = {}

    def full(self, i):
        if i not in self.cache:
            if len(self.cache) >= 2:
                self.cache.pop(next(iter(self.cache)))
            self.cache[i] = load(self.paths[i]).astype(np.float64)
        return self.cache[i]

    def search(self, q):
        """Best (track index, 48 kHz offset) for the 4 kHz query q."""
        c = signal.correlate(self.cat, q, mode="valid", method="fft")
        energy = signal.correlate(self.cat.astype(np.float64) ** 2, np.ones(len(q)), mode="valid", method="fft")
        k = int(np.argmax(c / np.sqrt(energy.clip(1e-12))))
        i = int(np.searchsorted(self.offsets, k, side="right") - 1)
        return i, (k - self.offsets[i]) * DEC


def fit(blk, src, around, radius):
    """Lag (within radius of around) and gain minimising the residual."""
    best = None
    for lag in range(max(0, around - radius), min(len(src) - len(blk), around + radius) + 1):
        s = src[lag:lag + len(blk)]
        g = (blk * s).sum() / max((s * s).sum(), 1e-12)
        e = ((blk - g * s) ** 2).sum()
        if best is None or e < best[0]:
            best = (e, lag, g)
    return best


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("take")
    ap.add_argument("sources")
    ap.add_argument("--block", type=float, default=1.0)
    ap.add_argument("--gain-window", type=float, default=0.05,
                    help="seconds per gain fit inside a block: absorbs volume automation, AGC or a limiter "
                         "on the playing computer (0 = one gain per block)")
    a = ap.parse_args()

    take = load(a.take).astype(np.float64)
    lib = Library(a.sources)
    print(f"take {len(take) / SR:.1f}s, {len(lib.names)} source tracks", flush=True)
    take_d = decimate(take)

    band = signal.butter(8, [20, 16000], "bandpass", fs=SR, output="sos")
    hp = signal.butter(4, 2000, "highpass", fs=SR, output="sos")
    B = int(a.block * SR)

    GW = int(a.gain_window * SR)

    def score(blk, src, lag, g):
        s = src[lag:lag + B]
        if GW:
            # A smoothly varying gain: per-window least squares, then
            # interpolated so a step between windows is not a "click".
            n = B // GW
            gs = np.array([(blk[k * GW:(k + 1) * GW] * s[k * GW:(k + 1) * GW]).sum()
                           / max((s[k * GW:(k + 1) * GW] ** 2).sum(), 1e-12) for k in range(n)])
            gs = np.where(np.isfinite(gs) & (gs > 0), gs, g)
            centers = (np.arange(n) + 0.5) * GW
            gain = np.interp(np.arange(B), centers, gs)[:, None]
        else:
            gain = g
        res = blk - gain * s
        rb = signal.sosfiltfilt(band, res, axis=0)
        xb = signal.sosfiltfilt(band, blk, axis=0)
        match = 1 - (rb ** 2).sum() / max((xb ** 2).sum(), 1e-12)
        rh = signal.sosfiltfilt(hp, res, axis=0)
        clicks = int((np.abs(rh) > 12 * (np.sqrt((rh ** 2).mean()) + 1e-9)).any(axis=1).sum())
        drop = 0
        z = (blk == 0).all(axis=1)
        if z.any() and np.sqrt((s ** 2).mean()) > 1e-4:
            edges = np.flatnonzero(np.diff(np.concatenate(([0], z.astype(int), [0]))))
            drop = int(sum(1 for r in edges[1::2] - edges[::2] if r >= 48))
        return match, clicks, drop

    rows, prev = [], None
    for b in range(0, len(take) - B, B):
        blk = take[b:b + B]
        if np.sqrt((blk ** 2).mean()) < 10 ** (-60 / 20):
            rows.append((b / SR, None))
            prev = None
            continue
        found = None
        if prev:  # same track, one block on: refine locally
            i, lag = prev
            best = fit(blk, lib.full(i), lag + B, 64)
            if best:
                m = score(blk, lib.full(i), best[1], best[2])
                if m[0] > 0.9:
                    found = (i, best[1], best[2], m)
        if not found:  # first block, or a track change: search everything
            i, coarse = lib.search(take_d[b // DEC:(b + B) // DEC])
            best = fit(blk, lib.full(i), coarse, 4 * DEC)
            if best is None:
                rows.append((b / SR, (lib.names[i] + " (no full window)", coarse, 0.0, 0.0, 0, 0)))
                prev = None
                continue
            found = (i, best[1], best[2], score(blk, lib.full(i), best[1], best[2]))
        i, lag, g, (match, clicks, drop) = found
        prev = (i, lag)
        rows.append((b / SR, (lib.names[i], lag, g, match, clicks, drop)))
        print(f"{b / SR:6.0f}s  {lib.names[i][:44]:44s} @{lag / SR:8.3f}s gain {g:.3f} "
              f"match {100 * match:7.3f}% clicks {clicks} dropouts {drop}", flush=True)

    scored = [r for _, r in rows if r]
    if not scored:
        print("FAIL: no music in the take")
        return 1
    worst = min(r[3] for r in scored)
    clicks = sum(r[4] for r in scored)
    drops = sum(r[5] for r in scored)
    print(f"\nblocks {len(scored)} (quiet {len(rows) - len(scored)})  match min {100 * worst:.3f}%"
          f"  mean {100 * np.mean([r[3] for r in scored]):.3f}%  clicks {clicks}  dropouts {drops}")
    ok = worst > 0.99 and clicks == 0 and drops == 0
    print("PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
