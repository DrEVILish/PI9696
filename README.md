# PI9696

A 1U rack-mounted multichannel audio recorder for Raspberry Pi. It records an
AES67 network stream (via [inferno](https://github.com/DrEVILish/inferno)) to
uncompressed WAV, plays takes back out onto the network, and is operated from
a 256×64 OLED front panel or a token-protected web dashboard.

This README is the project's single design document: features, UI,
architecture, decisions and status. [WIRING.md](WIRING.md) covers GPIO and
power; [AGENTS.md](AGENTS.md) holds the working rules for contributors and
coding agents.

---

## Install

On Raspberry Pi OS Lite **64-bit** (Debian 13 "trixie") on a **Raspberry Pi 4 or 5**:

```bash
curl -fsSL https://drevilish.github.io/pi9696/install.sh | bash
```

The installer ([docs/install.sh](docs/install.sh)) installs every dependency
(apt packages, Go, Rust), enables SPI, builds pi9696, the inferno ALSA plugin
and the statime PTP clock from pinned sources, installs the services and
starts them. It needs about 2 GB of free space and 15-30 minutes on a Pi 4
(the Rust builds take most of it). Re-run it to update: it rebuilds in place
and keeps settings (`/etc/pi9696`), recordings (`/rec`) and the access token.

| Option (environment) | Default | Meaning |
|---|---|---|
| `PI9696_DIR` | `/opt/pi9696` | install location |
| `PI9696_REF` | `main` | pi9696 branch, tag or commit |
| `PI9696_CLOCK` | `statime` | `statime`: follow the network's PTP leader (required to record); `stub`: single-host clock for a LAN with no leader (monitor and playback only) |
| `PI9696_PORT` | `80` | WebUI port (written to `$PI9696_DIR/.env` on first install) |
| `PI9696_FORCE` | unset | `1` skips the OS and hardware checks |
| `PI9696_NO_START` | unset | `1` installs without (re)starting services |

e.g. `curl -fsSL https://drevilish.github.io/pi9696/install.sh | PI9696_CLOCK=stub bash`.

After the first install, **reboot** if the installer says SPI was just
enabled. Then open `http://<hostname>.local` and log in with the access code
shown on the OLED (or `journalctl -u pi9696 -b | grep 'access code'`).

What gets installed:

| Piece | Where | Notes |
|---|---|---|
| pi9696 | `$PI9696_DIR/pi9696`, `pi9696.service` | the app (Go), runs as root |
| inferno ALSA plugin | `/usr/lib/aarch64-linux-gnu/alsa-lib/libasound_module_pcm_inferno.so`, `/etc/asound.conf` | the app's one inferno instance, in-process; built from the DrEVILish fork at a pinned commit |
| statime | `$PI9696_DIR/statime`, `statime.service` | PTP clock (teodly `inferno-dev`, pinned), config `deploy/statime.toml` |
| clock stub | `$PI9696_DIR/fake_usrvclock_server`, `pi9696-clock.service` | alternative to statime when no PTP leader exists; the two conflict |
| hostapd drop-in | `/etc/systemd/system/hostapd.service.d/pi9696.conf` | optional Wi-Fi AP, off until enabled in the app |
| avahi | `avahi-daemon` | `<name>.local` |
| logs | journald + `/var/log/pi9696/app.log` (logrotate) | install log: `/var/log/pi9696-install.log` |

---

## Specifications

| Parameter | Value |
|---|---|
| Hardware | Raspberry Pi 5 (Ethernet PTP hardware clock) or Pi 4 (software timestamping); SSD1322 OLED on SPI, EC11 encoder, Record/Stop/Play buttons with lamps |
| Audio I/O | AES67 via inferno, Ethernet only (no analog/USB audio, by design) |
| Rates | 44.1 / 48 / 96 / 192 kHz |
| Channels | 1-128, RX and TX always equal. On a Pi 4 at 48 kHz, 1-128 ch record bit-exact (about 58% CPU at 128 ch) |
| Format | WAV PCM 24-bit (32-bit internal), `-rf64 auto`: a take past 4 GiB is finalised as RF64 |
| Files | `/rec/YYYY-MM-DD/prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav` (+ `.channels.json` sidecar), about 17.3 MB/min at 48 kHz stereo |
| Remote | HTTP (token + session auth, no HTTPS), port from `PI9696_REMOTE_PORT` (installer default 80, app default 8080) |
| Deck control | Blackmagic HyperDeck protocol on TCP 9993 (Settings → Transport, default off, no auth) |
| Logging | Error/Warn/Info/Debug (default Error), journald + `/var/log/pi9696/app.log` |

---

## Architecture

```
  inferno network (AES67)
        │  in-process inferno ALSA plugin (txholder.go): one device, RX = TX
        ▼
  capture loop ──► FIFO /rec/raw/*.raw ──► ffmpeg ──► /rec/YYYY-MM-DD/*.wav
                          │
                          └──► ffmpeg astats (meter chain) ──► OLED + WebUI meters

  playback: ffmpeg (decode, -ss seek) ──► pump ──► the same plugin's TX side
```

Key decisions:

- One Go process owns everything behind one app mutex. The inferno lifecycle
  belongs to `infernoWorker`, the only goroutine that starts or stops it.
- **Exactly one inferno instance**, in-process, with equal RX and TX channel
  counts, always visible on the network.
- **Recording requires a network clock.** A take only starts while statime
  reports this unit as a PTP slave within 1 ms of its master on 5 consecutive
  polls (`clocksync.go`). The stub never qualifies. Demo mode is exempt.
- Recording starts only from idle, never over a take. Playback and recording
  are mutually exclusive.
- ffmpeg is the capture/playback converter; no native rewrite planned.
- inferno changes live in the [DrEVILish/inferno](https://github.com/DrEVILish/inferno)
  fork (`dev`), pinned by commit in the installer; nothing is filed upstream
  without the maintainer's consent.

---

## Features

### Recording

- Manual start/stop from the panel, the WebUI or HyperDeck.
- Refused unless the clock is synced (`NO CLOCK SYNC`), with less than 30 min
  of space left, while inferno restarts for a new rate/channel count
  (`AUDIO RESTARTING - WAIT`) or with no input (`NO AUDIO INPUT`).
- Auto-stops with less than 1 min of space left (graceful finalise, LOW DISK).
- A take starts contiguous: the input monitor is stopped before the recorder
  opens the FIFO.
- Faults are reported, not swallowed: a dying ffmpeg shows
  `RECORDING STOPPED - SEE LOG` with its last stderr lines logged; a crashed
  inferno is reaped, its take finalised and the server retried with backoff.
- Tag presets (Show/Rehearsal/Soundcheck/Interview/Backup/None) and a filename
  prefix.
- Channel names: each take saves its channel names when it starts, in
  `<take>.channels.json`: the unit's channel labels (see Level meters) over
  inferno's RX channel names, with each channel's source at the time (e.g.
  `Left@AVIO-USB`). The recordings table renames them per take
  (`GET/POST /api/recordings/channels`). pi9696 never writes channel names to
  inferno.

### Playback

- Plays out through inferno TX, bit-perfect end to end (a take played back
  reaches a second Pi sample-identical, `test/interop/e2e_bitperfect.py`).
  Takes go out undithered; an idle feeder keeps the transmitter fed with
  silence so playback never starts with an underrun. Local ALSA is the
  fallback where no inferno device exists (dev/sim).
- What plays: the take loaded with a row's button in the WebUI recordings
  table (marked "loaded"), else the newest. Loading only queues a take, even
  while another plays; PLAY starts it (`play=1` on `POST /api/playback/select`
  starts one for API clients).
- A take whose format differs from the device's is refused, naming the cause:
  `SAMPLE RATE MISMATCH`, `CHANNEL COUNT MISMATCH` or `RATE + CHANNEL MISMATCH`.
- Keys: Play = play / pause / resume; Stop = stop (the next Play starts from
  the beginning), also from pause. The Play lamp flashes while paused.
- Encoder: rotate while paused = 5 s scrub; push does nothing during playback
  (only Play pauses); hold = exit.

### Level meters

- Peak/RMS per channel from ffmpeg's `astats`, green/amber/red at -18 / -6
  dBFS, configurable range and peak hold (hold time, then 20 dB/s release).
- OLED: 16 meters per page in idle-browse, 4 px under the status bar.
- WebUI: banks of 8 meters (each with its own dBFS scale) that wrap to the
  screen width. Channel number above each meter, name below; a peak-hold bar
  in its zone colour over the RMS fill and a thin live-peak line.
  Double-click a name (or Enter/F2) to rename the channel: this sets the
  unit's own label (`POST /api/channels/label`, persisted; empty restores
  inferno's name), used by the meters and new takes.

### Files and USB

- Copy selected/all takes to USB (per-day folders), delete with confirmation,
  format a USB drive (exFAT, FAT32 fallback with a 4 GB file-limit notice).
- WebUI: per-file download and Download ALL as a streaming ZIP with a manifest.
- Config export/import (non-secret JSON on USB; no Wi-Fi password or token).

### Front panel (OLED)

Fixed 256×64 layout, FiraCode TTF in named contexts (`statusbar`, `header`,
`menu`, `selected`, `details`, `alert`, `recording`). Every line must fit 256
px or it silently overflows (`TestSysNoticesFitOneOLEDLine` measures
notices). Single-parameter settings are edited in place on their row. Lamps:
REC (GPIO12) lit while recording, PLAY (GPIO16) solid while playing and
pulsing while paused (see WIRING.md).

### Web dashboard

- **Header**: live OLED mirror, on-screen encoder, transport keys that behave
  like the panel lamps (dim in their colour when off, glowing when on), the
  same size in icon or text mode.
- **Level meters** band, collapsible to a label and caret at the header's left.
- **Transport Status**: a reel-to-reel deck and the Status table (rate,
  channels, format, tag, inferno, inferno TX, clock, network, uptime, version).
- **Recordings**: one row per take with load, channel names and download.
- **Footer**: disk space and record time left, short notices, and the System
  pane (CPU, app CPU by subsystem, RAM, temperature and disk graphs).
- Settings modal for every persisted setting. Themes from
  [ftl-themes](https://github.com/DrEVILish/ftl-themes) (submodule, v5; the
  app uses the library's components and tokens and declares none itself).
- `<device>.local` resolves on the LAN (an avahi address record for the device
  name). The OLED info page's QR links to `http://<device>.local/#t=<token>`.

### Inferno controller view

A network controller (e.g. netaudio) sees one device with equal RX/TX,
Product Version (the app version), sample rate and encoding, latency, clock
role and sync status. Renaming the device from a controller renames the unit.
A sample rate set on the unit restarts inferno and is announced to open
controllers. Audio only flows once a controller subscribes the unit:
inferno never auto-connects.

### Security

- 8-character access token (on the OLED, and in the journal at startup and
  rotation) exchanged for a 12 h server-side session; login rate-limited per
  client; token compared in constant time.
- Behind a reverse proxy, set `PI9696_TRUSTED_PROXIES` (addresses or CIDR) so
  rate limiting keys on the forwarded client.
- WebSocket streams re-check the session on every push; downloads are
  whitelisted against the recording list.
- **Plain HTTP only**, and no CSRF token (same-origin cookies): keep the unit
  on a trusted network.

---

## Clock

inferno only transmits with a clock overlay on `/tmp/ptp-usrvclock`, and the
app only records while statime reports a lock.

- **statime** (default): follows the network's PTPv1 leader (normally a
  hardware inferno-network device) with `deploy/statime.toml`
  (`hardware-clock = "auto"`: the Pi 5's `/dev/ptp0`, software timestamping on
  a Pi 4). Check the Status table's Clock row or `systemctl status statime`.
- **stub** (`pi9696-clock.service`): publishes this host's own clock. Single
  host only, never counts as synced: monitor and playback, no recording.
- Two inferno hosts and no hardware leader: statime cannot be a PTPv1 master;
  run one host as PTPv2 master (`protocol-version = "PTPv2"`, `priority1`
  below 251, `usrvclock-export = false`) and the unit as PTPv2 slave.
- Every device exchanging audio with the unit must follow the same leader, or
  received audio is silent.

---

## Operations

| Task | Command |
|---|---|
| Logs | `journalctl -u pi9696 -f`, `/var/log/pi9696/app.log` |
| Access code | `journalctl -u pi9696 -b \| grep 'access code'` |
| Restart | `sudo systemctl restart pi9696` |
| Update | re-run the installer |
| Settings file | `/etc/pi9696/config.json` |
| Per-host options | `$PI9696_DIR/.env`: `PI9696_REMOTE_PORT`, `PI9696_TRUSTED_PROXIES`, `PI9696_PPROF` (loopback pprof), `PI9696_SIM` |

The service runs as root (GPIO, SPI, ALSA, USB mounts, the FIFO). **Do not add
a `CapabilityBoundingSet`**: the 4 MB recording FIFO needs
`CAP_SYS_RESOURCE`, and without it the recorder silently falls back to the 64
KB default (about 2.6 ms at 128 ch).

| Symptom | Check |
|---|---|
| Display blank | SPI enabled (reboot after install)? Wiring per WIRING.md? |
| Recording refused | `NO CLOCK SYNC`: is a PTP leader on the LAN and statime locked? Low disk? |
| No audio / silent meters | Has a controller subscribed the unit? Same PTP leader as the source? |
| Gaps in a take | `journalctl -u pi9696 \| grep 'capture overrun'` |
| WebUI unreachable | `systemctl status pi9696`; port in `.env`; `ss -tlnp` |

---

## Development

```bash
go build -o pi9696 .     # build (cgo: needs libasound2-dev)
go vet ./...
test/gotest.sh           # vet + the whole suite under -race (dev machine only)
PI9696_SIM=1 ./pi9696    # full app without SPI/GPIO; token printed to stderr
```

- **Never run the test suite on a unit**: it starts ffmpeg children and
  inferno clients that do not belong on a live recorder. The suite is
  hermetic (temp recordings tree, no real inferno device, the clock gate off).
- Sim mode writes every OLED frame to `/tmp/pi9696_sim_frame.png` and uses
  `/tmp/pi9696-config.json`; drive the panel through
  `POST /api/input/encoder/left|right|click|hold` and
  `POST /api/input/button/record|stop|play`.
- `test/ui/settings-roundtrip.js` (Playwright, against a sim instance) checks
  every settings control; `test/interop/` holds the two-host checks.
- Tests share one `infernoWorker`: keep them mutex-safe and restore the
  globals they change.

### Repository layout

```
main.go           state machine, menus, recording/playback, inferno lifecycle
remote.go         web server: auth, dashboard, settings, downloads, meter push
txholder.go       the in-process inferno instance (capture → FIFO, TX holder) + pump
clocksync.go      statime observation poller (the recording clock gate)
channelnames.go   per-take channel names; channellabels.go: the unit's labels
devicename.go     controller renames; mdnsaddr.go: <device>.local
hyperdeck.go      Blackmagic HyperDeck server (TCP 9993)
logging.go        log/slog (stderr + app.log)
hardware/         SSD1322 display, encoder, buttons, lamps, network detection
alsapcm/          cgo ALSA wrapper
deploy/           systemd units, statime.toml, asound.conf, hostapd drop-in
docs/install.sh   the installer (served by GitHub Pages)
test/             gotest.sh, ui/ (Playwright), interop/ (two-host checks)
third_party/      ftl-themes (submodule)
```

Not tracked (created by the installer): `inferno/`, `statime/`, `web/` assets,
`fonts/`, `.env`, the binary.

---

## Known limitations

1. **No HTTPS** and no CSRF token: trusted networks only.
2. **Wi-Fi AP** brings up hostapd on `wlan0` but configures no address or
   DHCP for clients.
3. **Power loss**: take content is fsync'd but the directory entry is not,
   and the config's temp file is not fsync'd before its rename.
4. **TX after the first playback** keeps streaming silence rather than
   nothing (stopping the stream goes through the plugin's deadlock-prone stop
   path).
5. **Pi 5** GPIO, SPI and the PTP hardware clock are supported by the code
   and installer but have not yet been verified on hardware; the Pi 4 is.

---

**Version:** 1.20.0
