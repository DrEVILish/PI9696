# Two-host inferno interop checks

Measures pi9696 against a second inferno host. Nothing here installs anything
on the unit: generation and analysis run on the second host or a dev machine.
Test signals are audible (owner rule): a person can listen to every recording.

| File | Runs on | Purpose |
|---|---|---|
| `e2e_bitperfect.py` | second host | Two-Pi bit-perfect test. The second host transmits an audible deterministic signal (a tone per channel, 200 + 97*ch Hz, plus a melody stepping every second, 1-11 kHz, about -12 dBFS RMS); pi9696 records a take through the WebUI and plays it back; the second host records the playback. The take is checked bit for bit against the regenerated source, and the playback recording bit for bit against the take. Exit 0 only if both are identical. `--stall-at` checks underrun recovery |
| `music_score.py` | dev machine (numpy/scipy, ffmpeg) | Real-music check: scores a take of a music source (an inferno-network interface fed by a computer) against the source FLACs, per 1 s block: track and offset by cross-correlation, gain fit, in-band match % (target > 99%), clicks and dropouts. Not bit-exact: the playing computer resamples |
| `view_devices.sh` | either | Controller view: netaudio device/channel list plus each channel's mDNS `rate`/`nchan`/`enc` records |

## Setup that matters

- **One clock.** Both hosts must follow the same PTP leader (see README
  "Clock"); on different clocks received audio lands outside the window the
  capture reads and recordings are silent.
- **netaudio** (0.3.14 or later) drives subscriptions: `pip3 install
  --break-system-packages netaudio` on the second host.
- **Never run the Go test suite on a unit**: use `test/gotest.sh` on a dev
  machine.

Example (second host, with the inferno ALSA plugin built there):

```bash
python3 e2e_bitperfect.py --pia <unit-address> --plugin /path/to/libasound_module_pcm_inferno.so
```
