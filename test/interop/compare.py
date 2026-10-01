#!/usr/bin/env python3
"""Compare a capture (WAV pcm_s24le/s32le or raw s32le) against the source.

Reports header facts, channel mapping, per-segment sample alignment (drops,
repeats and discontinuities show up as offset changes, digital-silence gaps
inside the audio as silent segments), bit-exact ratio per channel and overall,
and tone-burst level/frequency. Built for 1-128 channels: the source is
memory-mapped and WAVs are converted in chunks.

--identity skips the all-pairs channel search (128^2 full-length FFTs) and
verifies capture channel k against source channel k at the aligned offset.
"""
import argparse, json, mmap, struct, sys
import numpy as np

p = argparse.ArgumentParser()
p.add_argument("source", help="raw s32le source")
p.add_argument("capture", help=".wav or raw s32le capture")
p.add_argument("--rate", type=int, default=48000)
p.add_argument("--channels", type=int, default=2, help="source channel count")
p.add_argument("--seg", type=float, default=1.0, help="alignment segment length, s")
p.add_argument("--identity", action="store_true", help="assume capture ch k = source ch k")
p.add_argument("--json", help="write summary JSON here")
a = p.parse_args()


def read_wav(path):
    f = open(path, "rb")
    mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
    if mm[:4] != b"RIFF" or mm[8:12] != b"WAVE":
        raise SystemExit(f"{path}: not RIFF/WAVE")
    pos, fmt, data_off, data_len = 12, None, None, None
    while pos + 8 <= len(mm):
        cid, size = mm[pos:pos + 4], struct.unpack("<I", mm[pos + 4:pos + 8])[0]
        if cid == b"fmt ":
            fmt = struct.unpack("<HHIIHH", mm[pos + 8:pos + 24])
        elif cid == b"data":
            data_off = pos + 8
            data_len = size if 0 < size <= len(mm) - data_off else len(mm) - data_off
            break
        pos += 8 + size + (size & 1)
    tag, ch, rate, _, align, bits = fmt
    n = data_len // align
    out = np.empty((n, ch), dtype=np.int32)
    step = max(1, (64 << 20) // align)
    raw = np.frombuffer(mm, dtype=np.uint8, count=n * align, offset=data_off)
    for i in range(0, n, step):
        j = min(n, i + step)
        r = raw[i * align:j * align]
        if bits == 24:
            b = r.reshape(-1, 3)
            v = (b[:, 0].astype(np.int32) << 8) | (b[:, 1].astype(np.int32) << 16) | (b[:, 2].astype(np.int32) << 24)
        elif bits == 32:
            v = r.view("<i4")
        elif bits == 16:
            v = r.view("<i2").astype(np.int32) << 16
        else:
            raise SystemExit(f"unsupported bits {bits}")
        out[i:j] = v.reshape(-1, ch)
    info = {"format_tag": tag, "channels": ch, "rate": rate, "bits": bits, "frames": n,
            "declared_data_bytes": struct.unpack("<I", mm[data_off - 4:data_off])[0]}
    return out, info


src = np.memmap(a.source, dtype="<i4", mode="r").reshape(-1, a.channels)
if a.capture.endswith(".wav"):
    cap, info = read_wav(a.capture)
else:
    cap = np.fromfile(a.capture, dtype="<i4").reshape(-1, a.channels)
    info = {"channels": a.channels, "rate": a.rate, "bits": 32, "frames": len(cap)}
R, C = info["rate"], cap.shape[1]
out = {"capture": a.capture, "header": info, "capture_seconds": round(len(cap) / R, 3)}
print(f"capture: {info}  ({len(cap)/R:.3f} s)")

nzrows = np.flatnonzero(np.any(cap != 0, axis=1))
if len(nzrows) == 0:
    out["verdict"] = "SILENT"
    print("capture is entirely digital silence")
    if a.json:
        json.dump(out, open(a.json, "w"), indent=1)
    sys.exit(1)
first, last = int(nzrows[0]), int(nzrows[-1])
out["leading_silence_s"] = round(first / R, 4)
out["trailing_silence_s"] = round((len(cap) - 1 - last) / R, 4)
print(f"leading silence {first/R:.4f}s, trailing silence {(len(cap)-1-last)/R:.4f}s")


def xcorr_offset(needle, hay, predicted=None):
    """Index in hay where needle best matches (FFT cross-correlation).
    Periodic content (tone bursts) matches at every period, so near-ties
    resolve to the candidate closest to `predicted`."""
    n = len(hay) + len(needle)
    nfft = 1 << (n - 1).bit_length()
    H = np.fft.rfft(np.asarray(hay, dtype=np.float64), nfft)
    N = np.fft.rfft(np.asarray(needle[::-1], dtype=np.float64), nfft)
    c = np.fft.irfft(H * N, nfft)[len(needle) - 1: len(hay)]
    k = int(np.argmax(c))
    if predicted is not None:
        cand = np.flatnonzero(c >= c[k] * (1 - 1e-6))
        k = int(cand[np.argmin(np.abs(cand - predicted))])
    denom = np.sqrt(np.sum(np.asarray(needle, np.float64) ** 2) * np.sum(np.asarray(hay[k:k + len(needle)], np.float64) ** 2)) or 1
    return k, float(c[k] / denom)


# Align on capture channel 1 against source channel 1 (identity) or the best match.
anchor = cap[first:min(first + 4 * R, last + 1), 0]
k0, r0 = xcorr_offset(anchor, src[:, 0])
offset0 = k0 - first

if a.identity:
    mapping = {}
    for ci in range(C):
        x = cap[first:first + min(R, last + 1 - first), ci].astype(np.float64)
        y = src[first + offset0:first + offset0 + len(x), ci].astype(np.float64)
        d = np.sqrt(np.sum(x * x) * np.sum(y * y)) or 1
        mapping[ci + 1] = {"source_channel": ci + 1, "corr": round(float(np.sum(x * y) / d), 6)}
else:
    probe = cap[first + R: first + 2 * R] if last - first > 3 * R else cap[first:last]
    mapping = {}
    for ci in range(C):
        best = None
        for si in range(src.shape[1]):
            k, r = xcorr_offset(probe[:, ci], src[:, si])
            if best is None or r > best[2]:
                best = (si, k, r)
        mapping[ci + 1] = {"source_channel": best[0] + 1, "corr": round(best[2], 6)}
bad_map = {k: v for k, v in mapping.items() if v["corr"] < 0.99 or (a.identity and v["source_channel"] != k)}
out["channel_mapping_problems"] = bad_map
out["channel_mapping"] = mapping if C <= 8 else f"{C - len(bad_map)}/{C} channels map 1:1 (corr >= 0.99)"
print("channel mapping:", out["channel_mapping"], "problems:", bad_map or "none")

seg = int(a.seg * R)
sc = mapping[1]["source_channel"] - 1
segments, prev = [], None
pos = first
while pos + seg <= last + 1:
    needle = cap[pos:pos + seg, 0]
    if not np.any(needle):
        segments.append({"cap_start": int(pos), "silent": True})
        pos += seg
        continue
    if prev is None:
        k, r = offset0 + pos, 1.0
        if not np.array_equal(src[k:k + seg, sc], needle):
            r = float(np.corrcoef(src[k:k + seg, sc].astype(np.float64), needle.astype(np.float64))[0, 1])
    else:
        predicted = pos + prev
        if 0 <= predicted and predicted + seg <= len(src) and np.array_equal(src[predicted:predicted + seg, sc], needle):
            k, r = predicted, 1.0
        else:
            lo = max(0, predicted - R // 2)
            hi = min(len(src), predicted + seg + R // 2)
            k, r = xcorr_offset(needle, src[lo:hi, sc], predicted - lo)
            k += lo
    segments.append({"cap_start": int(pos), "src_start": int(k), "corr": r})
    prev = k - pos
    pos += seg

aligned = [s for s in segments if "src_start" in s]
offsets = [s["src_start"] - s["cap_start"] for s in aligned]
jumps = [{"at_capture_s": round(aligned[i]["cap_start"] / R, 3), "samples": int(offsets[i] - offsets[i - 1])}
         for i in range(1, len(aligned)) if offsets[i] != offsets[i - 1]]
out["segments"] = len(segments)
out["silent_segments_inside_audio"] = sum(1 for s in segments if s.get("silent"))
out["offset_changes"] = jumps
out["min_segment_corr"] = round(min(s["corr"] for s in aligned), 6)
print(f"{len(segments)} segments; offset changes (drops/repeats): {jumps if jumps else 'none'}")

srcmap = np.array([mapping[ci + 1]["source_channel"] - 1 for ci in range(C)])
total = exact = 0
max_err = 0
per_ch_bad = np.zeros(C, dtype=np.int64)
worst = []
for s in aligned:
    c = cap[s["cap_start"]:s["cap_start"] + seg].astype(np.int64)
    o = s["src_start"]
    ref = np.asarray(src[o:o + len(c)], dtype=np.int64)[:, srcmap]
    c = c[:len(ref)]
    d = c != ref
    total += d.size
    nbad = int(d.sum())
    exact += d.size - nbad
    if nbad:
        per_ch_bad += d.sum(axis=0)
        m = int(np.max(np.abs(c - ref)))
        max_err = max(max_err, m)
        worst.append({"at_capture_s": round(s["cap_start"] / R, 3), "max_abs_err_lsb24": m >> 8, "mismatched_samples": nbad})
out["samples_compared"] = total
out["bit_exact_ratio"] = exact / total if total else 0
out["max_abs_error_lsb24"] = max_err >> 8
out["channels_with_errors"] = int(np.sum(per_ch_bad > 0))
out["mismatch_segments"] = worst[:20]
print(f"bit-exact: {exact}/{total} = {100*exact/total:.6f}%  max |err| = {max_err>>8} LSB(24-bit)  channels with errors: {out['channels_with_errors']}/{C}")
if worst:
    print("mismatching segments:", worst[:10])

blk, tone = 10 * R, 2 * R
src0 = first + offsets[0] if offsets else first
cstart = ((src0 // blk) + 1) * blk - (offsets[0] if offsets else 0) + R // 4
if cstart + R // 2 <= len(cap):
    tones = {}
    for ci in list(range(min(C, 4))) + ([C - 1] if C > 4 else []):
        x = cap[cstart:cstart + R // 2, ci].astype(np.float64) / 2 ** 31
        f = np.fft.rfftfreq(len(x), 1 / R)[np.argmax(np.abs(np.fft.rfft(x * np.hanning(len(x)))))]
        tones[ci + 1] = {"freq_hz": round(float(f), 1), "expect_hz": 200 + 150 * int(srcmap[ci]),
                         "rms_dbfs": round(float(20 * np.log10(np.sqrt(np.mean(x ** 2)) + 1e-30)), 2)}
    out["tone_burst"] = tones
    print("tone burst:", tones, "(sine -6 dBFS peak = -9.03 dBFS RMS)")

out["verdict"] = "PASS" if (not jumps and max_err == 0 and out["silent_segments_inside_audio"] == 0 and not bad_map) else "FAIL"
print("VERDICT:", out["verdict"])
if a.json:
    json.dump(out, open(a.json, "w"), indent=1)
