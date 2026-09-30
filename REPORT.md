# PI9696 inferno interop test — report

**Date:** 2026-09-30 → 2026-10-01 · **Unit under test:** pi9696-test (192.168.10.69, Raspberry Pi 4, the target hardware) · **Build:** `main` at `5e4f395`, plus the fixes listed below.

## Summary

| Area | Result |
|---|---|
| Record path (inferno RX → FIFO → ffmpeg → WAV), second host → pi9696 | **Works.** Bit-exact: 70 s take 100% (6,720,000/6,720,000 samples); 187 s take under PTP 99.973%, the only defect being a 50 ms jump at the start (F6) |
| Network path alone (inferno RX, no app) | **Works.** Bit-exact for 60.9 s from an undithered source; ±1 LSB on 25% of samples from a stock (dithering) source (F4) |
| Channel count visible to a remote controller | **Works.** RX and TX both follow the WebUI setting (2 → 8 channels) |
| Sample rate visible to a remote controller | **Partial.** The TX channels advertise it in mDNS (48000 → 96000). netaudio's device-level sample rate is blank for every inferno device, and the RX device's rate is not published anywhere (F8) |
| Playback path (app → inferno TX → second host) | **Fails as shipped.** 73–160 transmitter restarts per 60 s file (F2). With the cause removed in a diagnostic build: continuous and correct within dither, but it loses the first ~5 s of every playback (F3) and has packet-sized late dropouts (F5) |
| Unit tests | `go test ./...` **deleted every recording on the unit** (F1, fixed). 4 pre-existing order-dependent failures (F12) |

Four commits came out of the run, each revertible on its own:

| Commit | Change |
|---|---|
| `0049ab5` | fix: the test suite no longer touches the real `/rec` (F1) |
| `5e8ecb7` | fix: the overall meter is reset when a monitor starts (F7) |
| `28e0ed8` | docs: inferno terminology throughout |
| `f9ce79c` | test: two-host harness in `test/interop/` |

## ⚠ Incident: recordings deleted on the test unit

Running the full `go test ./...` on the Pi (23:23–23:24, 2026-09-30) deleted
`/rec/2026-09-27/`, `/rec/2026-09-29/` and every take in `/rec/2026-09-30/`.
The takes known to be lost are `recording_20260930_153952_ch2_48kHz.wav`,
`recording_20260930_161456_ch2_48kHz.wav` (both from an earlier session) and
this run's own 70 s take (a copy survives on the dev server). The contents of
the two older folders were never listed, and the journal does not name them
(logging is Error-only).

The cause is `TestDownloadAll` calling `os.RemoveAll(RecordPath)` while
`RecordPath` was the constant `/rec` (F1). It is fixed in `0049ab5`. No
recovery was attempted: ext4 zeroes extents on delete, and recovery tools would
have to be installed on the test unit, which isn't allowed there.

## How it was tested

**Deviations from the requested setup:**

- **192.168.10.55 was never reachable.** It refuses this Pi's SSH key (`root@pi9696`,
  ed25519) and no key was added. The dev server (192.168.10.162, x86_64 VM, same
  /24, mDNS reaches across) stood in as the second host. Everything below that
  says ".162" was intended for .55.
- **WebUI login.** The real service shows its token only on the OLED. Reading it
  from process memory was refused. The WebUI-driven steps therefore used the
  documented audio-path method (DEPLOYMENT.md "End-to-end through the app"):
  the same binary on the Pi with `PI9696_SIM=1`, the real `/etc/pi9696/config.json`
  and port 80, with `pi9696.service` stopped. The audio path is identical; only
  the display output differs.
- **Clock.** A shared clock was needed (F10). statime ran on both hosts as
  PTPv2 master (.162, `priority1` 250) and slave (Pi, the repo's
  `inferno-ptpv1.toml` with only `protocol-version` changed). statime cannot
  act as a PTPv1 master ("trying to act as master in PTPv1, not implemented
  yet"). The Pi locked within 20 s and held ±130 µs.

**Signal and analysis** (`test/interop/`): a deterministic 24-bit-exact source
(1 kHz / 2 kHz bursts at −6 dBFS plus seeded noise at −12 dBFS, unique in every 10 s block)
played through inferno ALSA on .162. The capture is aligned to the source per
1 s segment, which reports drops/repeats, the bit-exact ratio, max error,
channel mapping and tone levels. It was validated by injecting a 1234-sample drop.
All analysis ran on .162, so nothing extra was installed on the test unit.

## Results

### Record path — second host → pi9696

| Run | Clock | Duration | Bit-exact | Drops | Notes |
|---|---|---|---|---|---|
| Probe `inferno2pipe` (no app), stock source | free-running¹ | 46.7 s | 75.01% | none | every mismatch is ±1 LSB: source dither (F4) |
| Probe, undithered source | free-running¹ | 60.9 s | **100%** | none | the network path is lossless |
| App take (WebUI Record/Stop) | free-running¹ | 70.23 s | **100%** | none | 48 kHz/2ch `pcm_s24le`, header correct, levels −9.01 dBFS (source −9.03) |
| App take | **PTP** | 187.22 s | 99.973% | none | first 2400 frames followed by a 2400-frame jump (F6) |

¹ The clock stub restarted unnoticed at 23:00:30 and displaced statime (F10). These
runs therefore had each host on its own uptime clock. The inferno receiver
re-bases on the incoming timestamps, so this still worked; the 187 s run is
the one with a genuinely shared clock.

Meters on the dashboard matched the source (−9.0 dBFS RMS on bursts, −12 on noise).

### Channel count and sample rate as seen from the network

Viewed with netaudio 0.3.14 from .162 (the remote controller) and cross-checked from the Pi:

| WebUI setting | `PI9696` (RX) | `PI9696-TX` (TX) | TX channel mDNS | netaudio "Sample Rate" |
|---|---|---|---|---|
| 2 ch / 48 kHz | 2 RX | 2 TX | `nchan=2 rate=48000 enc=24` | blank |
| 8 ch / 96 kHz | 8 RX | 8 TX | `nchan=8 rate=96000 enc=24` (all 8) | blank |

The app restarted inferno on each change (`inferno2pipe -c 8`, the monitor at 96 kHz)
and the dashboard showed "WAV 96kHz 8ch". With the only source at 48 kHz, the
subscription reported "Transmitter setup failed" and every channel metered
−100 dB. The mismatch is refused correctly, but the UI gives no reason.

### Playback path — pi9696 → second host

| Build | XRUNs / transmitter restarts per 60 s file | Wall time | What arrived |
|---|---|---|---|
| shipped, sim frame to SD | 73 | ~95 s² | flow torn down every ~8 s |
| shipped, sim frame to `/dev/null` | 160 | 71 s | as above |
| diagnostic (timing probes) | 91 | 68 s | as above |
| diagnostic, pump uses `TryLock` | **0** | **61 s** | continuous; see F3/F4/F5 |

² The WebUI playhead read 00:00:54 at about 95 s of wall time.

Under PTP, with the `TryLock` build, 54.7 s of the file arrived in order with
the correct channel mapping and exact levels. 74.99% of samples were bit-exact,
the rest ±1 LSB (dither, F4), plus 12 dropouts of one or two packets
(32/64 frames). The first ~4.5–4.9 s of the file never arrived (F3).

## Findings

Each finding lists the ranked causes, how they were checked, and the fix.

### F1 — `go test ./...` deletes the unit's recordings · **fixed `0049ab5`**
1. **Causes:** (a) `TestDownloadAll` runs `os.RemoveAll(RecordPath)` with `RecordPath` a const `/rec` ✔; (b) other tests cut takes into `/rec` ✔ (a 97 s demo take appeared mid-run).
2. **Check:** reproduced on the Pi, where `/rec` was emptied. After the fix a sentinel in `/rec` on .162 survived two full runs and the temp trees were cleaned up.
3. **Fix:** `RecordPath`/`RawPath` became vars; `TestMain` points them at `os.MkdirTemp`; `TestSuiteDoesNotTouchRealRecordings` guards the change.
4. **Related:** the suite also started fake-ffmpeg children that outlived it (killed by hand). DEPLOYMENT.md's warning covered only `inferno/`. Tests should still run on the dev server, never the unit.

### F2 — Playback transmit underruns: the pump waits on the app mutex while `render()` holds it · **root cause verified, fix proposed**
1. **Causes, ranked:** (a) the app mutex held too long by the 100 ms render tick, stalling the pump ✔; (b) ALSA buffer too small (40 ms, `LatencyUs`) ✘; (c) the sim's PNG write to SD ✘; (d) clock source ✘.
2. **Check:** a timing build measured `render()` holding the mutex up to 95.6 ms (per-second max: median 44 ms, p90 88 ms) and the pump waiting up to 91.8 ms (p90 79 ms) for it, against a 42 ms device buffer. Each stall means XRUN → re-prepare → "transmitter stopped/started" → the receiver's flow times out. `aplay` with the same 40 ms buffer had 1 XRUN in 20 s. `/dev/null` for the sim frame made it worse (160). The earlier session's run on the stub clock showed the same 116 restarts. `TryLock` in the pump took XRUNs from 91 to 0 and wall time from 68 s to 61 s.
3. **Fix (not committed; it needs review):** take the pump off the app mutex. Publish `alive`/`paused` to the pump atomically, or a per-pump channel, keeping one writer per ALSA handle; the `TryLock` experiment is not safe as-is, because after a seek a stale pump could write alongside the new one. Separately, shrink `render()`'s critical section: snapshot state under the lock, draw outside it. A bigger buffer alone does not scale, because the plugin caps the buffer at 524,288 bytes, i.e. 1024 frames (21 ms) at 128 channels.
4. **Related:** the same 95 ms holds delay every WebUI handler and the meter flush. Recording is unaffected: its path never takes the mutex.

### F3 — Transmit is silent while idle, loses the start of each playback, and loops stale audio after it
- **Idle:** `PI9696-TX` sends no media while nothing plays; the Pi sends about 3 UDP/s. A subscriber times out ("not receiving media packets") every ~8–10 s and resubscribes.
- **Start of playback:** the first write after idle XRUNs and restarts the transmitter, so the receiver re-cycles. The first 4.5–28 s of a file were lost (4.5 s with the `TryLock` build, 28 s with the shipped build).
- **After playback** (seen with the `TryLock` build): once the pump stops writing, nothing calls into ALSA, the underrun is never detected, and the transmitter keeps sending the last 2048-frame ring, a 42 ms loop of the take's end at about −12 dBFS, until the next write.
- **Causes:** (a) the holder only feeds the device during playback ✔ (pump code: it writes only while `playbackCmd == cmd`); (b) the plugin does not zero its ring on underrun ✔.
- **Fix:** keep the holder fed with silence whenever it isn't playing (a background silence pump), so the transmitter never idles, underruns or replays stale data. This is also what keeps subscribers locked.
- **Needs a design decision:** see Q1/Q2.

### F4 — Stock inferno always dithers 24-bit transmit
`flows_tx.rs:169-170` → `samples_utils.rs:58-63` add TPDF dither
(`rand_u8 − rand_u8 + 128`, then truncate) to every 16/24-bit packet. For
24-bit-exact input, 25% of samples come out ±1 LSB; measured 24.99%,
independent of value, sign or magnitude, including digital silence. The 32-bit branch
passes `None`, but the encoding is hardcoded to 24 (`settings.rs:127`, "TODO make it
configurable"). Impact: pi9696 playback is not bit-transparent, and silence
becomes ±1 LSB noise. An inferno receiver of pi9696 is transparent (the record path
above). **Needs a decision:** Q3. Per DEPLOYMENT.md, nothing is filed upstream without the maintainer's consent.

### F5 — Late packets from the Pi's transmitter
12 dropouts (384 frames) in 54.7 s at the receiver's 10 ms latency, and 235
frames in 45 s at 20 ms, with "reorder buffer timeout" errors falling from 16 to 2.
The other direction had none. **Causes, ranked:** (a) scheduling jitter in
the in-process transmitter on a Pi 4 with software timestamps; (b) VM/bridge
jitter on .162. **Next:** re-measure after F2 on real hardware at both ends, run
`cyclictest` (as the upstream README advises), and try `TX_LATENCY_NS` 20 ms.

### F6 — A take can start with a 50 ms discontinuity
In the 187 s take the first 2400 frames are real audio, and then 2400 frames are
missing: the outgoing monitor was still reading the FIFO (README Known
Limitation 4, "sub-100 ms window"). It is intermittent (the 70 s take was clean) and
now measured at 50 ms of lost audio plus an audible jump. **Fix:** stop the monitor
and drain before the recorder attaches, or let the recorder take over the
monitor's reader instead of opening a second one.

### F7 — Overall meter stuck at the previous level after a monitor restart · **fixed `5e8ecb7`**
`startMonitor` reset the channel meters but not `meterPeakDB`/`meterRMSDB`, and
the old monitor's reaper skips its reset. After the 8 ch/96 kHz change (no media)
`/api/meter` showed peak −7.8 dB while all channels read −100, and the OLED
waveform drew a phantom trace. The regression test fails on the old code.

### F8 — The sample rate isn't visible in netaudio
netaudio sends conmon sample-rate/encoding/AES67 probes and inferno never answers
("deadline reached while waiting for … sample rates"). The column was blank for
`ITEST-SRC` too, which is a lone device on its own IP, so the shared IP is ruled out. The
rate exists only in the TX channel mDNS records; an RX-only device such as `PI9696`
publishes none. **Fix:** upstream (inferno would need to answer the probe), or have
pi9696 publish its rate itself; see Q4.

### F9 — netaudio bulk unsubscribe removes only the first channel
`subscription remove --rx rx:1@PI9696 --rx rx:2@PI9696` left RX 2 subscribed,
which netaudio's own readback caught. Removing RX 2 on its own worked.

### F10 — Clock traps
- The stub publishes uptime (`CLOCK_MONOTONIC_RAW`, shift 0), so two stub hosts are days apart and inferno TX between them is unusable (packets stamped −159,446 s from the receiver's clock arrived as silence).
- `systemctl mask --runtime pi9696-clock` has no effect: the unit file is in `/etc/systemd/system`, which wins over `/run`. Any start of `pi9696.service` pulls the stub back (`Wants=`), and it replaces statime's `/tmp/ptp-usrvclock`. This silently invalidated part of this run and cost a false lead.
- statime cannot be a PTPv1 master, so two inferno hosts with no hardware device need PTPv2.
- The installed `statime.service` points at `/home/drevilish/statime/target/debug/statime`, which doesn't exist; it has failed since boot.

### F11 — Deployment drift found on the unit
| Item | Docs say | Unit has |
|---|---|---|
| `/etc/asound.conf` | upstream `@args` form, "must keep" it | bare device, keys supplied by `INFERNO_*` env (matches the code since `275c3c5`; **the docs are stale**) |
| `/var/log/pi9696/` | created at install; `app.log` there | missing, so there is no app.log |
| `pi9696.service` | template `TimeoutStopSec=30` | installed copy has 20 |
| `inferno2pipe` output | — | stdout/stderr discarded by the app, so subscription/media errors never reach the journal |
| `/opt/pi9696/pi9696-test` | — | untracked binary from an earlier session, which was running as an orphaned sim on port 8080 for 7 h and holding the inferno ports |

### F12 — Pre-existing test failures
`TestStopWhilePausedAwakensStoppedFFmpeg`, `TestSeekWhilePausedReapsOldFFmpeg`,
`TestAutoDimStateTransitions` and `TestInfernoUpGateAndAudioPath` fail in every full run, on
unmodified `HEAD` as well, on both .162 and the Pi, and all pass in isolation. The cause is shared
state between tests (README "Testing gotcha"), which is unresolved. `gofmt -l` also flags
`main.go` and `remote.go` on `HEAD`.

### F13 — UI strings
The dashboard's TX status and nine other user-facing strings (`txholder.go:186-357`,
`main.go:3329`) still use the old protocol vendor name. Uptime is shown
unrounded ("23.330199427s"). Empty-prefix takes are named `recording_…`; the
README pattern says `prefix_…` and doesn't state the default (Q6).

## What was changed and what's left

- **pi9696-test (.69):** `pi9696` rebuilt from `HEAD` and the service restarted (clean, port 80). Config byte-identical to the start. Stub clock restored. All test units, captures, takes, the probe and aplay-test inferno state (process IDs 4 and 6) and the diagnostic binaries removed. `/tmp` is back to 1% (it had been full of another session's raw captures; per your instruction these were deleted and their logs kept in `/var/tmp/pi9696-work/old-tmp-20260930/`). **Still installed:** `gh` (installed earlier for GitHub pushes, which conflicts with the no-extra-applications rule; remove with `apt-get purge gh` and `rm -r /root/.config/gh` when you're done pushing). Left alone because another session owns them: `pi9696-test`, inferno state `…0003`, and `netaudio` (pre-existing).
- **dev (.162):** installed rustup, inferno v0.5.4 (`/opt/inferno-src/inferno`, stock plugin restored), an undithered test build (`inferno-nodither`), statime, the stub, netaudio 0.3.14, tcpdump, python3-numpy, ALSA dev headers, the Go 1.26 toolchain (module cache) and `/etc/asound.conf` (a copy of the Pi's). All test services are stopped. Captures and logs are in `/var/tmp/pi9696-work/`.
- **.55:** untouched (no access).

## Next steps (priority order)

1. **Keep tests off units:** `0049ab5` fixes the deletion; also add a CI/Makefile target so the suite runs on dev only.
2. **F2:** a lock-free TX pump (one writer per handle) and a shorter render critical section; then re-run `test/interop/tx_test.sh`, expecting 0 XRUN.
3. **F3:** a continuous silence feed when not playing (after Q1/Q2), then re-measure the playback start: the first sample of a file should reach a subscriber.
4. **Repeat on .55** (add the key above) so the second host is real hardware rather than a VM, and re-measure F5 there.
5. **F6:** close the FIFO handoff window before the recorder reads.
6. **Unit housekeeping:** DEPLOYMENT.md and README.md were corrected in this run (bare `asound.conf`, the test hazard, the two-host clock recipe, the `Wants=`/mask trap, the drift items, and the test counts, which were stale at 78 vs 128). Still to do on the unit itself: `mkdir -p /var/log/pi9696`, reinstall `pi9696.service` from the template, and fix or remove `statime.service`.
7. **F12:** make the four tests order-independent.
8. **Run the app's own receiver past 2 channels and at high rates** (only 2 ch/48 kHz audio and 8 ch/96 kHz visibility were exercised).

## Questions for you (design behaviour not in the .md files)

- **Q1.** While idle, should `PI9696-TX` stream silence continuously, so subscribers stay locked and a playback is heard from its first sample? Today it sends nothing.
- **Q2.** When a playback ends or stops, should TX fall back to silence immediately? (Today the stale last ~42 ms can loop, F3.)
- **Q3.** Must playback be bit-transparent? If so, inferno's always-on TX dither (F4) needs a change in the inferno fork; otherwise document ±1 LSB TPDF as intended.
- **Q4.** Should the unit publish its sample rate for the RX side (which inferno doesn't expose), and should the UI say why a subscription failed on a rate mismatch?
- **Q5.** Should the user-facing UI strings also switch to inferno terminology (F13)?
- **Q6.** Is `recording_` the intended filename prefix when none is set?
