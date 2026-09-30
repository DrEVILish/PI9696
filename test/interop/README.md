# Two-host inferno interop harness

Measures pi9696 against a second inferno host sample-for-sample. Results and
method are written up in [REPORT.md](../../REPORT.md). Nothing here installs
anything on the test unit: the analysis runs on the second host.

| File | Runs on | Purpose |
|---|---|---|
| `gen_signal.py` | second host | Deterministic source: 24-bit-exact s32le, 1 kHz / 2 kHz bursts + seeded noise, unique per 10 s block |
| `compare.py` | second host | Aligns a capture (WAV or raw s32le) to the source per segment; reports drops/repeats, bit-exact ratio, max error, channel mapping, tone level |
| `view_devices.sh` | either | Remote-controller view: netaudio device/channel list plus the per-channel mDNS `rate`/`nchan`/`enc` records |
| `tx_test.sh` | pi9696 | One playback-out run: fresh `ITEST-RX` capture on the second host, Play, wait for the take, count TX XRUNs/restarts |

## Setup that matters

1. **One shared clock.** The clock stub (`fake_usrvclock_server`) publishes each
   host's own `CLOCK_MONOTONIC_RAW` (uptime), so two hosts on stubs are days
   apart. Use statime on both: the second host as PTPv2 master
   (`priority1` below the unit's 251, `usrvclock-export = false`, plus the stock
   stub for its own inferno), pi9696 as slave with export on. Statime has no
   PTPv1 master, so PTPv2 is the only option without hardware on the LAN.
2. **Keep the stub out of the way on the unit.** `pi9696.service` has
   `Wants=pi9696-clock`, and `systemctl mask --runtime` does not override a unit
   file in `/etc/systemd/system`, so any `start`/`restart` of the app brings
   the stub back and it takes over `/tmp/ptp-usrvclock`. Stop `pi9696-clock`
   and run the app outside `pi9696.service` for the duration, then check
   `ss -xp | grep ptp-usrvclock` shows only statime.
3. **Instance ports.** Every extra inferno instance on a host needs its own
   `INFERNO_PROCESS_ID` + `INFERNO_ALT_PORT` block (probe 4/10400, aplay test
   6/10600, `ITEST-RX` 5/10500 were used here).
4. **Dither.** Stock inferno TPDF-dithers every 24-bit transmit, so a stock
   source can never be bit-exact (expect ~25% of samples at ±1 LSB). For a
   bit-exact reference, build the second host's ALSA plugin with the `Some(dither_rng)`
   on the 24-bit branch of `flows_tx.rs` replaced by `None`.

## Known limits of `compare.py`

- The first alignment uses a 4 s window from the start of audio; a
  discontinuity inside that first window is reported as mismatched samples in
  segment 0, not as an offset change.
- Captures must come from the same `--seconds`/`--seed` source file: the noise
  on channel 2 depends on the total length.
