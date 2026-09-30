#!/usr/bin/env python3
"""Deterministic 24-bit-exact test signal for the inferno interop test.

Output: raw s32le interleaved, 48 kHz, N channels (default 2), low 8 bits zero
so a lossless PCM24 path must reproduce every sample exactly.

Layout, per 10 s block: 2 s sine burst (ch k: 1000*(k+1) Hz, -6 dBFS),
then 8 s seeded white noise (-12 dBFS RMS, unique per block and channel).
"""
import argparse
import numpy as np

p = argparse.ArgumentParser()
p.add_argument("out")
p.add_argument("--seconds", type=int, default=180)
p.add_argument("--rate", type=int, default=48000)
p.add_argument("--channels", type=int, default=2)
p.add_argument("--seed", type=int, default=9696)
a = p.parse_args()

rng = np.random.default_rng(a.seed)
n = a.seconds * a.rate
t = np.arange(n) / a.rate
blk = 10 * a.rate
tone = 2 * a.rate
out = np.empty((n, a.channels), dtype=np.float64)
for ch in range(a.channels):
    x = rng.standard_normal(n) * 10 ** (-12 / 20)
    s = np.sin(2 * np.pi * 1000 * (ch + 1) * t) * 10 ** (-6 / 20)
    in_tone = (np.arange(n) % blk) < tone
    x[in_tone] = s[in_tone]
    out[:, ch] = x
full = 2 ** 23 - 1
q = np.clip(np.round(out * full), -full, full).astype(np.int32) << 8
q.astype("<i4").tofile(a.out)
print(f"wrote {a.out}: {a.seconds}s {a.rate}Hz {a.channels}ch s32le (24-bit exact), seed {a.seed}")
