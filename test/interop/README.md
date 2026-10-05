# Two-host inferno interop harness

Measures pi9696 against a second inferno host sample-for-sample. Results and
method are written up in [REPORT.md](../../REPORT.md). Nothing here installs
anything on the test unit: the analysis runs on the second host.

| File | Runs on | Purpose |
|---|---|---|
| `gen_signal.py` | second host | Deterministic source: 24-bit-exact s32le, per-channel bursts at 200 + 150·k Hz + noise seeded by (seed, channel, block); streamed, so 128 ch × 300 s never sits in RAM |
| `compare.py` | second host | Aligns a capture (WAV or raw s32le) to the source per segment; reports drops/repeats, bit-exact ratio (overall and channels with errors), max error, channel mapping, tone level. `--identity` verifies channel k against source k instead of an all-pairs search; the source is memory-mapped and WAVs are read in chunks |
| `view_devices.sh` | either | Remote-controller view: netaudio device/channel list plus the per-channel mDNS `rate`/`nchan`/`enc` records |
| `tx_test.sh` | pi9696 | One playback-out run: fresh `ITEST-RX` capture on the second host, Play, wait for the take, count TX XRUNs/restarts |
| `channel_sweep.py` | pi9696 | Channel-count sweep: per N, source + subscribe + 60 s take + per-second metrics (CPU per process, RAM, SD writes, UDP drops, temperature, throttling) + every-sample compare; optional control capture of the same source on the second host |
| `arc_subscribe.py` | either | Subscribe RX 1..N with raw ARC requests (netaudio's encoder), for receivers whose subscription read-back netaudio cannot parse |
| `channel_list_check.sh` | second host | Channel-list round trip: one RX (`inferno2pipe`) or TX (ALSA plugin) device per count from a given inferno tree, `netaudio channel list`, check every channel 1..N came back (U13) |
| `arc_page_check.py` | second host | Feeds every RX/TX channel-list page in a pcap to netaudio's own page parser and prints which pages it accepts or rejects (U13) |
| `e2e_bitperfect.py` | second host | Two-Pi bit-perfect test: the second host transmits an audible deterministic test signal (a tone per channel, 200 + 97*ch Hz, plus a melody stepping every second, 1-11 kHz, about -12 dBFS RMS: a person can listen to every recording); pi9696 records a take through the WebUI and plays it back; the second host records the playback. The take is checked bit for bit against the regenerated source, and the second host's recording (aligned by exact sample search) bit for bit against the take. Exit 0 only if both are identical. `--stall-at` checks underrun recovery (blocks of 0.1 s located in the take) |
| `music_score.py` | dev server (numpy/scipy) | Real-music check: scores a pi9696 take of the LAN's playlist source (an inferno-network USB interface fed by a computer) against the source FLACs. Per 1 s block: track and offset by cross-correlation, gain fit, in-band match % (target > 99%), clicks (impulses absent from the source) and dropouts. Not bit-exact: the computer resamples 44.1 kHz to 48 kHz |

## Setup that matters

1. **One shared clock.** The clock stub (`fake_usrvclock_server`) publishes each
   host's own `CLOCK_MONOTONIC_RAW` (uptime), so two hosts on stubs are days
   apart. The LAN now has a hardware PTPv1 leader (the USB interface): run
   statime as a PTPv1 slave with export on (`deploy/statime.toml`) on **both**
   hosts, so they also share the interface's clock. Without a hardware leader,
   the second host can be a PTPv2 master instead (`priority1` below the
   unit's 251, `usrvclock-export = false`, plus the stock stub for its own
   inferno), but then neither host can exchange audio with the interface.
2. **Keep the stub out of the way on the unit.** `pi9696.service` only orders
   after `pi9696-clock` (no `Wants=`), and the stub `Conflicts=statime`, so
   with `statime.service` enabled a restart of the app leaves statime in
   charge. Check `ss -xp | grep ptp-usrvclock` shows only statime.
3. **Instance ports.** Every extra inferno instance on a host needs its own
   `INFERNO_PROCESS_ID` + `INFERNO_ALT_PORT` block (probe 4/10400, aplay test
   6/10600, `ITEST-RX` 5/10500 were used here).
4. **Dither.** inferno TPDF-dithers a 24-bit transmit unless told the source
   is already 24-bit (expect ~25% of samples at ±1 LSB otherwise). Since the
   fork includes upstream's configurable dither (`ef39a28`), set
   `INFERNO_TX_SOURCE_BIT_DEPTH=24` on a 24-bit-exact source; the old
   test-only patch is no longer needed. pi9696's own transmitter sets it
   (takes are 24-bit), so its playback is undithered.

## Known limits of `compare.py`

- The first alignment uses a 4 s window from the start of audio; a
  discontinuity inside that first window is reported as mismatched samples in
  segment 0, not as an offset change.
- Since the streaming rewrite, a block's content depends only on (seed,
  channel, block), so any source length/channel count works, but captures
  made before it need the old generator.

## Above 16 channels

Stock inferno pages its receive-channel list 32 entries at a time, where
netaudio accepts 16, and pads every short last page, which netaudio rejects.
So netaudio cannot read or subscribe a stock receiver with more than 16
channels, nor read a stock transmitter whose count is not a multiple of 32
(33, 65, …). Transmitters of 1–32, 64, 96 and 128 channels work
(INFERNO-UPSTREAM.md U13). For more than 16 channels, run the unit's receiver
from a scratch build with `inferno-patches/0001-…` applied
(`PI9696_INFERNO_BIN=...`, the installed inferno is untouched). With it,
`netaudio subscription add` works directly at any count. The round-2 sweep
predates the corrected patch and subscribed with `arc_subscribe.py`, which is
kept for receivers that netaudio cannot read. Wait up to ~60 s for every flow
to come up before recording: the subscriber resolves them one at a time.
