#!/usr/bin/env python3
"""Deterministic 24-bit-exact test signal for the inferno interop tests.

Output: raw s32le interleaved, N channels, low 8 bits zero, so a lossless
PCM24 path must reproduce every sample exactly.

Per 10 s block and channel k: 2 s sine burst at 200 + 150*k Hz (-6 dBFS;
stays under Nyquist up to 128 ch at 48 kHz), then 8 s white noise at
-12 dBFS RMS seeded by (seed, k, block). Every block is unique, and a block's
content does not depend on the file length or channel count, so captures can
be aligned to the exact sample. Written block by block: a 128 ch / 200 s file
(4.9 GB) never has to fit in RAM.
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

R, C = a.rate, a.channels
blk, tone = 10 * R, 2 * R
full = 2 ** 23 - 1
with open(a.out, "wb") as f:
    for b in range((a.seconds * R + blk - 1) // blk):
        n = min(blk, a.seconds * R - b * blk)
        t = (b * blk + np.arange(n)) / R
        out = np.empty((n, C), dtype=np.int32)
        for ch in range(C):
            x = np.random.default_rng([a.seed, ch, b]).standard_normal(n) * 10 ** (-12 / 20)
            m = min(tone, n)
            x[:m] = np.sin(2 * np.pi * (200 + 150 * ch) * t[:m]) * 10 ** (-6 / 20)
            out[:, ch] = np.clip(np.round(x * full), -full, full).astype(np.int32) << 8
        out.astype("<i4").tofile(f)
print(f"wrote {a.out}: {a.seconds}s {R}Hz {C}ch s32le (24-bit exact), seed {a.seed}")
