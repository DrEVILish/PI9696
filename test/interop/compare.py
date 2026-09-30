#!/usr/bin/env python3
"""Compare a capture (WAV pcm_s24le/s32le or raw s32le) against the source signal.

Reports: header facts, channel mapping, per-segment sample alignment (so drops,
repeats and discontinuities show up as offset changes), bit-exact match rate,
null-test residual, and tone-burst level/frequency.
"""
import argparse, json, struct, sys
import numpy as np

p = argparse.ArgumentParser()
p.add_argument("source", help="raw s32le source")
p.add_argument("capture", help=".wav or raw s32le capture")
p.add_argument("--rate", type=int, default=48000)
p.add_argument("--channels", type=int, default=2)
p.add_argument("--seg", type=float, default=1.0, help="alignment segment length, s")
p.add_argument("--json", help="write summary JSON here")
a = p.parse_args()


def read_wav(path):
    with open(path, "rb") as f:
        data = f.read()
    if data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise SystemExit(f"{path}: not RIFF/WAVE")
    pos, fmt, pcm = 12, None, None
    while pos + 8 <= len(data):
        cid, size = data[pos:pos + 4], struct.unpack("<I", data[pos + 4:pos + 8])[0]
        body = data[pos + 8:pos + 8 + size]
        if cid == b"fmt ":
            fmt = struct.unpack("<HHIIHH", body[:16])
        elif cid == b"data":
            pcm = body if 0 < size <= len(data) - pos - 8 else data[pos + 8:]
            break
        pos += 8 + size + (size & 1)
    tag, ch, rate, _, align, bits = fmt
    n = len(pcm) // align
    raw = np.frombuffer(pcm[: n * align], dtype=np.uint8)
    if bits == 24:
        b = raw.reshape(-1, 3).astype(np.int32)
        v = (b[:, 0] << 8) | (b[:, 1] << 16) | (b[:, 2] << 24)
    elif bits == 32:
        v = raw.view("<i4").astype(np.int32)
    elif bits == 16:
        v = raw.view("<i2").astype(np.int32) << 16
    else:
        raise SystemExit(f"unsupported bits {bits}")
    info = {"format_tag": tag, "channels": ch, "rate": rate, "bits": bits, "frames": n,
            "declared_data_bytes": struct.unpack("<I", data[pos + 4:pos + 8])[0]}
    return v.reshape(-1, ch), info


src = np.fromfile(a.source, dtype="<i4").reshape(-1, a.channels).astype(np.int64)
if a.capture.endswith(".wav"):
    cap, info = read_wav(a.capture)
else:
    cap = np.fromfile(a.capture, dtype="<i4").reshape(-1, a.channels)
    info = {"channels": a.channels, "rate": a.rate, "bits": 32, "frames": len(cap)}
cap = cap.astype(np.int64)
R = info["rate"]
out = {"capture": a.capture, "header": info, "capture_seconds": round(len(cap) / R, 3)}
print(f"capture: {info}  ({len(cap)/R:.3f} s)")

nz = np.flatnonzero(np.any(cap != 0, axis=1))
if len(nz) == 0:
    out["verdict"] = "SILENT"
    print("capture is entirely digital silence")
    if a.json:
        json.dump(out, open(a.json, "w"), indent=1)
    sys.exit(1)
first, last = nz[0], nz[-1]
out["leading_silence_s"] = round(first / R, 4)
out["trailing_silence_s"] = round((len(cap) - 1 - last) / R, 4)
print(f"leading silence {first/R:.4f}s, trailing silence {(len(cap)-1-last)/R:.4f}s")


def xcorr_offset(needle, hay, predicted=None):
    """Index in hay where needle best matches (FFT cross-correlation).

    Periodic content (the tone bursts) matches at every period, so near-ties
    resolve to the candidate closest to `predicted`.
    """
    n = len(hay) + len(needle)
    nfft = 1 << (n - 1).bit_length()
    H = np.fft.rfft(hay.astype(np.float64), nfft)
    N = np.fft.rfft(needle[::-1].astype(np.float64), nfft)
    c = np.fft.irfft(H * N, nfft)[len(needle) - 1: len(hay)]
    k = int(np.argmax(c))
    if predicted is not None:
        cand = np.flatnonzero(c >= c[k] * (1 - 1e-6))
        k = int(cand[np.argmin(np.abs(cand - predicted))])
    denom = np.sqrt(np.sum(needle.astype(np.float64) ** 2) * np.sum(hay[k:k + len(needle)].astype(np.float64) ** 2)) or 1
    return k, float(c[k] / denom)


# Channel mapping: 1 s of each capture channel against each source channel.
probe = cap[first + R: first + 2 * R] if last - first > 3 * R else cap[first:last]
mapping = {}
for ci in range(cap.shape[1]):
    best = None
    for si in range(src.shape[1]):
        k, r = xcorr_offset(probe[:, ci], src[:, si])
        if best is None or r > best[2]:
            best = (si, k, r)
    mapping[ci + 1] = {"source_channel": best[0] + 1, "corr": round(best[2], 6)}
out["channel_mapping"] = mapping
print("channel mapping (capture -> source):", mapping)

# Segment-wise alignment on ch1 against its mapped source channel.
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
        # 4 s always spans past a 2 s tone burst into unique noise.
        anchor = cap[pos:min(pos + 4 * R, last + 1), 0]
        k, r = xcorr_offset(anchor, src[:, sc])
        if not np.array_equal(src[k:k + seg, sc], needle):
            r = float(np.corrcoef(src[k:k + seg, sc], needle)[0, 1])
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
jumps = []
for i in range(1, len(aligned)):
    d = offsets[i] - offsets[i - 1]
    if d != 0:
        jumps.append({"at_capture_s": round(aligned[i]["cap_start"] / R, 3), "samples": int(d)})
out["segments"] = len(segments)
out["silent_segments_inside_audio"] = sum(1 for s in segments if s.get("silent"))
out["offset_changes"] = jumps
out["min_segment_corr"] = round(min(s["corr"] for s in aligned), 6)
print(f"{len(segments)} segments; offset changes (drops/repeats): {jumps if jumps else 'none'}")
print(f"min segment correlation {out['min_segment_corr']}")

# Bit-exact / null test per aligned segment, all channels.
total = exact = 0
max_err = 0
worst = []
for s in aligned:
    c = cap[s["cap_start"]:s["cap_start"] + seg]
    o = s["src_start"]
    ref = np.stack([src[o:o + seg, mapping[ci + 1]["source_channel"] - 1] for ci in range(cap.shape[1])], axis=1)
    if len(ref) < len(c):
        c = c[: len(ref)]
    d = c - ref
    total += d.size
    exact += int(np.sum(d == 0))
    m = int(np.max(np.abs(d)))
    if m > max_err:
        max_err = m
    if m:
        worst.append({"at_capture_s": round(s["cap_start"] / R, 3), "max_abs_err_lsb24": m >> 8,
                      "mismatched_samples": int(np.sum(d != 0))})
out["samples_compared"] = total
out["bit_exact_ratio"] = exact / total if total else 0
out["max_abs_error_lsb24"] = max_err >> 8
out["mismatch_segments"] = worst[:20]
print(f"bit-exact: {exact}/{total} = {100*exact/total:.6f}%  max |err| = {max_err>>8} LSB(24-bit)")
if worst:
    print("mismatching segments:", worst[:10])

# Tone burst level/frequency from the first complete burst in the capture.
lat = offsets[0] if offsets else 0
blk, tone = 10 * R, 2 * R
src0 = first + lat
bstart = ((src0 // blk) + 1) * blk
cstart = bstart - lat + R // 4
if cstart + R // 2 <= len(cap):
    tones = {}
    for ci in range(cap.shape[1]):
        x = cap[cstart:cstart + R // 2, ci].astype(np.float64) / 2 ** 31
        rms = np.sqrt(np.mean(x ** 2))
        f = np.fft.rfftfreq(len(x), 1 / R)[np.argmax(np.abs(np.fft.rfft(x * np.hanning(len(x)))))]
        tones[ci + 1] = {"freq_hz": round(float(f), 1), "rms_dbfs": round(20 * np.log10(rms), 2)}
    out["tone_burst"] = tones
    print("tone burst:", tones, "(source: 1000/2000 Hz, sine -6 dBFS peak = -9.03 dBFS RMS)")

out["latency_note"] = "src_start - cap_start is the source index offset, not network latency"
out["verdict"] = "PASS" if (not jumps and max_err == 0 and out["silent_segments_inside_audio"] == 0) else "FAIL"
print("VERDICT:", out["verdict"])
if a.json:
    json.dump(out, open(a.json, "w"), indent=1)
