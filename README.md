# PI9696 — Design Guide

A 1U rack-mounted multichannel audio recorder: AES67 over Ethernet via inferno, uncompressed
WAV to SD card, operated from a 256×64 OLED front panel or a token-auth web dashboard.

**Docs:** [WIRING.md](WIRING.md) · [AGENTS.md](AGENTS.md) · [DEPLOYMENT.md](DEPLOYMENT.md) · [REPORT.md](REPORT.md) (two-host inferno test, 2026-09-30)

---

## Quick Start

```bash
# Build and run
go build -o pi9696 . && sudo ./pi9696
```

---

## Specifications

| Parameter | Value |
|-----------|-------|
| Target | Raspberry Pi 5 deployment (`/dev/ptp0` hardware timestamping); must also run error-free on Pi 4 (software-timestamping fallback) |
| Input | AES67 via Inferno (Ethernet only; no analog/USB audio) |
| Rates | 44.1 / 48 / 96 / 192 kHz |
| Channels | 1–128. Measured on a Pi 4 at 48 kHz: 1–128 ch bit-exact, 58% CPU at 128 ch (REPORT.md round 3). Above 16 ch needs the inferno U13 patch (Known Limitations #11) |
| Format | WAV PCM 24-bit on disk (32-bit internal) |
| File naming | `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav` in `/rec/YYYY-MM-DD/` |
| Display | SSD1322 256×64 OLED (SPI), FiraCode TTF |
| Controls | EC11 rotary encoder + Record/Stop/Play buttons |
| Remote | HTTP on port 8080 (token + session auth, no HTTPS); `PI9696_REMOTE_PORT` overrides |
| Deck control | Blackmagic HyperDeck protocol on TCP 9993 (Settings → Transport toggle, default off, no auth) |
| Logging | Error/Warn/Info/Debug (default Error-only), journald + app.log |
| File size | ~17.3 MB/min at 48 kHz stereo 24-bit |

---

## System Architecture

```
                    Inferno (AES67)
                           │
                    ┌──────▼──────┐
                    │    FIFO     │
                    │  rec/raw/   │
                    └──────┬──────┘
                           │
              ┌────────────▼────────────┐
              │        ffmpeg           │
              │  s32le → WAV 24-bit     │
              └────────────┬────────────┘
                           │
                           ▼
                   /rec/YYYY-MM-DD/
                           │
              ┌────────────┴────────────┐
              │       meterReader       │
              │  astats → peak/RMS      │
              └────────────┬────────────┘
                           │
              ┌────────────▼────────────┐
              │     OLED / WebUI        │
              │  VU meters + meters     │
              └─────────────────────────┘

Playback path:
  ffmpeg -f s32le → pump → TX holder (`inferno` ALSA, inferno TX out); pause writes
  silence, seek via -ss restart. Local `-f alsa default` only where no
  inferno device exists (dev/sim fallback).
```

**Key design decisions:**
- Single Go process owns everything behind one app mutex
- Inferno lifecycle is `infernoWorker`-owned (the only goroutine that mutates inferno state)
- Recording starts only from idle, never over an active take
- **Recording requires a network-synced clock.** A take only starts while statime reports this unit as a PTP *slave* within 1 ms of its master on 5 consecutive 1 s polls of its observation socket (`clocksync.go`). The single-host stub never qualifies, and neither does a PTP master. Demo mode (synthetic source) is exempt
- Playback and recording are mutually exclusive in both directions
- Inferno is bidirectional (sends + receives); the app currently drives receive (recording) while transmit/playback-out moves to the app's ALSA client (`alsapcm/`, one process holding capture + playback so a single instance does both)
- TX is real scope, not a stretch goal: the unit has two modes, RECORDING and PLAYBACK, and its TX and RX channels stay visible on the inferno/AES67 network in both modes. TX and RX channel counts are always equal (one `channelCount` drives both). The clock source is a hard TX gate — Inferno aborts transmit without the usrvclock overlay — so statime (PTPv1, locked to the network's PTP leader) replaces the stub the moment a hardware inferno-network device is on the LAN
- ffmpeg is the capture/playback converter (tried and tested); no native rewrite planned

---

## Features

### Recording

- Manual start/stop
- Start refused unless the clock is synced to the network (OLED flashes `NO CLOCK SYNC / cannot record`, web notice, log line); losing sync mid-take is logged and the take continues
- Start refused when <30 min space remains at the current rate
- Take starts contiguous: the input monitor is killed and reaped before the recorder opens the FIFO
- Take auto-stops when <1 min space remains (graceful finalize, LOW DISK warning)
- Tag presets (Show/Rehearsal/Soundcheck/Interview/Backup/None) + filename prefix
- Real-time elapsed/remaining, storage, Peak/RMS on OLED

### Playback

- Target: plays out through Inferno ALSA (pause writes silence to inferno TX so the playhead holds without gaps or SIGSTOP choreography). The app holds the `inferno` device persistently (`txholder.go`: TX-only, own NAME/PROCESS_ID/ALT_PORT), pumping ffmpeg-decoded s32le through it; local ALSA (`default`) remains the fallback where no inferno device exists (dev/sim). Sample rate/channel mismatches are refused with a log + UI error; a present-but-clockless holder refuses inferno playback with a notice instead of misrouting locally
- TX behaviour (owner decision): **nothing is sent while the unit is not playing**, and **TX goes silent at pause, end and stop**. A fresh holder sends no media. Pause writes silence. End and stop overwrite the plugin's whole ring with silence, because the plugin keeps re-sending its ring when nothing writes. Current limit: after the first playback TX keeps streaming that silence rather than nothing, because stopping the stream goes through the inferno plugin's deadlock-prone stop path (INFERNO-UPSTREAM.md U5); inferno also dithers silence to ±1 LSB (U3)
- Encoder: click = play/pause, rotate while paused = 5 s scrub, hold = exit
- Progress bar + elapsed/total with [PAUSED] marker

### Level Metering

- Peak/RMS per channel from ffmpeg's `astats` pass-through filter
- WebUI: green/yellow/red by level (−18 dBFS / −6 dBFS breakpoints)
- OLED: grayscale (shading + segments)
- Configurable meter range and peak-hold decay (hold + 20 dB/s decay)

### Files

- Copy selected/all takes to USB (per-day structure preserved)
- Delete with confirmation
  - Format USB drive: exFAT or FAT32, user-selectable (currently exFAT-first with FAT32 fallback; explicit choice in progress — exFAT has no 4 GB file ceiling, which matters at high channel counts)
- WebUI: per-file download + Download-ALL as streaming ZIP with manifest

### Button Lamps

- REC (GPIO12): lit while recording
- PLAY (GPIO16): solid while playing, 250ms pulse while paused
- STOP: no lamp (nothing a user waits on)

### Config Export/Import

- Export: non-secret JSON profile to USB (no WiFi password, no access token)
- Import: applied + persisted, Inferno restart if rate/channels changed
- OLED: System Options menu; WebUI: settings modal Config group

### WebUI Dashboard

- Live OLED mirror (PNG)
- On-screen encoder/buttons driving same handlers as hardware
- Settings modal (all persisted settings)
- Per-channel VU meters over 100 ms WebSocket push
- INFERNO-LINK lamp reflects Inferno state (runs in meter payload)
- Status panel shows the clock state (`Synced (PTP slave, 8µs)` / `Locking` / `Not synced (…)`) and the TX state in inferno terms (`Inferno TX ready (PI9696-TX)`)
- Sample rate must be visible to an inferno controller (netaudio) for both TX and RX (owner requirement). Not yet met: needs inferno to answer the rate probe (INFERNO-UPSTREAM.md U2, patch verified) and RX+TX as one inferno instance (U8)
- Theming: ftl-themes bundles (32 themes, `third_party/ftl-themes` submodule @ `03ebb1e`, v4.0.0 — always track latest upstream; `html[data-theme]` slugs unchanged) —
  one linked stylesheet + `html[data-theme]`; the `third_party/ftl-themes/CONTRACT.md`
  is the integration spec. Markup uses the library's own components (`.btn`, `.table`, `.modal`, `.meter`, `.scroll` — v4 dropped the `ftl-` prefix everywhere),
  the shared icon sprite (`/static/themes/icons/<slug>.svg`, per-theme art with a
  generic fallback) and the app-shell hooks. Density/Motion/Contrast display
  options persist device-wide beside the theme choice. The app reads the library's tokens directly and declares none of them itself (see `TestDefaultThemeIsFTL`).

### Auth & Security

- Token (8-char, shown on OLED) → session cookie (12 h, server-side)
- Login rate-limited per IP; token compared in constant time
- Downloads whitelisted against recording list (no arbitrary file access)

**⚠ Known security limitation:** Plain HTTP only — treat as unencrypted admin page. Anyone sniffing the LAN can see the token and hijack the session. Do not expose beyond a trusted network without adding HTTPS.

**⚠ CSRF:** The WebUI relies on same-origin cookie scoping; there is no explicit CSRF token. Any site a logged-in browser visits could POST to the control port (default `:8080`, `PI9696_REMOTE_PORT` overrides; e.g. `logger` on the recorder) and trigger state changes. Acceptable on a trusted LAN with a token that is never exposed in browser JS.

---

## OLED UI

Fixed 256×64 layout with FiraCode TTF rendering in named contexts:
- `statusbar`: `[ETH] [INF] [USB]` + time/remaining/storage
- `header`: transport state + recording metadata
- `menu`: menu items (scrollable, max 4 visible)
- `selected`: highlighted menu item
- `details`: info text
- `alert`: confirmation dialogs (14 pt)
- `recording`: live elapsed/remaining/PVU during takes

**Layout constraint:** Each line is 256 pixels wide. New menu text must fit or it silently overflows. Use `cmd/simcheck` to render PNGs for visual verification.

---

## Building

### Prerequisites

- Raspberry Pi 5, Raspberry Pi OS 64-bit (Trixie or newer)
- Go 1.26+ (build), Rust/Cargo (Inferno AoIP server), libasound2-dev (`pkg-config alsa` — required: `alsapcm/` uses cgo, so any `go build ./...` / `go test ./...` needs the headers)
- Root access for GPIO/SPI/ALSA/USB mounting

### On the Pi

Full install record, including the clock service and the kernel limits, is in
[DEPLOYMENT.md](DEPLOYMENT.md). The order matters:

```bash
# 1. SPI must be enabled or the app exits at startup (display init opens SPI)
sudo sed -i 's/^#dtparam=spi=on/dtparam=spi=on/' /boot/firmware/config.txt && sudo reboot

# 2. Inferno (pinned v0.5.4; note the submodules, and that the binary the app
#    runs is target/release/inferno2pipe, not "inferno")
sudo apt install -y build-essential pkg-config libasound2-dev libudev-dev
git clone https://github.com/DrEVILish/inferno inferno
cd inferno && git checkout v0.5.4 && git submodule update --init --recursive
cargo build --release && cd ..

# 3. A clock source must be exporting the usrvclock overlay, or Inferno starts
#    but never transmits (deploy/pi9696-clock.service)

# 4. Unit files, generated from the repo template rather than hand-written
sudo sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696.service \
    | sudo tee /etc/systemd/system/pi9696.service > /dev/null
sudo sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696-clock.service \
    | sudo tee /etc/systemd/system/pi9696-clock.service > /dev/null
sudo systemctl daemon-reload && sudo systemctl enable --now pi9696-clock pi9696
sudo systemctl status pi9696 && sudo journalctl -u pi9696 -f
```

Audio only flows once an inferno controller (netaudio) subscribes the app's device
to a transmitter — Inferno never auto-connects. `./inferno-loopback.sh` proves
the path end to end without any hardware inferno-network device present.

### Without a Pi (simulator)

```bash
PI9696_SIM=1 ./pi9696        # full app, no SPI/GPIO
go run ./cmd/simcheck        # render all OLED screens to /tmp/pi9696_shots/
```

Sim facts:
- Every frame dumped to `/tmp/pi9696_sim_frame.png` (override: `PI9696_SIM_OUT`)
- Token printed to stderr: `sim mode: remote access token XX XX XXXX`
- Config path: `/tmp/pi9696-config.json` (vs `/etc/pi9696/config.json` on real Pi)
- Recording requires `inferno/target/release/inferno2pipe` (build with `cargo build --release` in `inferno/`) and a running clock source — see DEPLOYMENT.md

### Rebuilding

```bash
go build -o pi9696 .          # rebuild
sudo systemctl restart pi9696 # restart service
sudo journalctl -u pi9696 -f  # tail logs
sudo systemctl status pi9696  # service status
```

---

## Development

### Commands

```bash
go build ./...       # compile
go vet ./...         # static analysis
go test ./...        # run all tests
```

### Testing

- 132 tests in `main_test.go` (+ 3 `clocksync_test.go`, 1 `inferno_log_test.go`, 10 `theme_test.go`, 9 `hyperdeck_test.go`, 8 `alsapcm/`, 8 `hardware/`, 2 `logging_test.go`)
- The suite is hermetic: a temp recordings tree, no real inferno TX device (a host with the inferno ALSA plugin used to get a real holder, which broke 4 tests), the clock gate off, and per-test restore of audio settings, web notice and monitor (`initTestHardware`). It passes in source order and under `go test -shuffle=on`. Still run it on the dev server, never on a unit - see DEPLOYMENT.md
- `test/interop/` measures a unit against a second inferno host sample-for-sample (REPORT.md)
- Tests run the real HTTP handlers over `httptest` (auth, recordings API, ZIP download, settings)
- Playback/seek tested against a fake `ffmpeg` via PATH shim
- Inferno worker concurrency tested against a stub server
- `cmd/simcheck` renders every OLED screen to PNG for layout checks (hardcodes its own menu items)

**⚠ Testing gotcha:** Tests share a single `infernoWorker`. Keep tests mutex-safe and restore package globals (e.g. reset `usbMounted` in cleanup). Tests run together; order-independence matters.

### Simulator

- Drive the real OLED menus via WebUI encoder endpoints:
  - `POST /api/input/encoder/left|right` — rotate
  - `POST /api/input/encoder/click|hold` — press
  - `POST /api/input/button/record|stop|play` — transport

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Display blank | SPI enabled? Wiring per WIRING.md? Running as root? |
| `[INF]` never lights | Ethernet up? `ip addr show eth0`? Inferno binary built? |
| Recording fails | `NO CLOCK SYNC`? statime running and locked (`systemctl status statime`, Status panel Clock row)? Low disk (<30 min)? Already recording? OLED flashes the reason |
| Gaps in a take | `journalctl -u pi9696 \| grep inferno2pipe` - receive-side losses are logged as `sample-loss events` |
| WebUI unreachable | Any interface with IP? `ss -tlnp \| grep ${PI9696_REMOTE_PORT:-8080}` |
| USB not detected | `mount -t tmpfs none /media/usb` for testing; real USB: `lsblk` |
| Logs | `sudo journalctl -u pi9696 -f` + `/var/log/pi9696/app.log` |

---

## Known Limitations & Design Debt

Design debt worth flagging here:

1. **Playback via Inferno/AoIP** — done for TX (persistent holder + pump, `txholder.go`), local ALSA kept as fallback. Interim: two inferno devices on the wire (`<name>` RX-only via inferno2pipe, `<name>-TX` TX-only via the holder) until the RX side moves in-process and unifies them. Measured against a second host (REPORT.md): audible and visible in both modes, but the pump stalls on the app mutex while `render()` holds it, so the transmitter restarts many times a minute (F2, open); receivers time out while TX is idle, so the first seconds of a playback can be lost at a subscriber (F3, a consequence of the no-TX-while-idle decision); inferno dithers every 24-bit transmit (F4/U3). Stale-audio replay after a take is fixed.
2. **No HTTPS** — plain HTTP on port 8080. Do not expose beyond trusted LAN.
3. **Directory fsync** — take content fsync'd, but parent directory entry fsync is unimplemented (power loss can lose directory entry).
4. **FIFO handoff window** — fixed (`7ca4d12`): the monitor's read used to race the new recorder for the first frames (measured: 50 ms missing 50 ms into a take, REPORT.md F6). The recorder now opens the FIFO only after the monitor (SIGKILLed - a graceful exit takes ~300 ms, longer than the FIFO holds at 128 ch) has exited.
5. **Config persistence** — atomic rename, but temp file not fsync'd before rename (power loss can truncate config).
6. **Meter race on monitor→record** — fixed via the `meterGen` generation counter (stale reapers can't touch the new session); kept here as history of the hazard.
7. **No analog/USB audio I/O** — Ethernet only (product decision).
8. **Sim config path** — `PI9696_SIM=1` writes to `/tmp/pi9696-config.json`; real Pi writes to `/etc/pi9696/config.json`. Resolved once in `init()`: set `PI9696_CONFIG` before startup to override (tests reassign `ConfigPath` directly).
9. **FIFO buffer needs `CAP_SYS_RESOURCE`** — the 4 MB raw FIFO needs the capability to grow; a `CapabilityBoundingSet` on the unit silently costs it, and the recorder keeps working at the 64 KB default. See DEPLOYMENT.md.
10. **Stuck takes are always stoppable** — `stopRecording`/`stopMonitor` escalate from SIGTERM to SIGKILL after 10 s (`ffmpegStopGrace`): an ffmpeg blocked reading an empty FIFO never acts on SIGTERM, which used to wedge the transport. The grace is long enough for ffmpeg to finalize a partial WAV on slow storage.
11. **Above 16 channels needs patched inferno** — stock inferno pages its receive-channel list 32 at a time and pads short pages, so netaudio cannot read or subscribe PI9696 beyond 16 channels (INFERNO-UPSTREAM.md U13). With `inferno-patches/0001-…` applied it routes and records 1–128 ch (verified on the unit). Until that lands in the inferno fork, run the receiver from a patched build (`PI9696_INFERNO_BIN`).

---

## Repository Layout

```
main.go            app: state machine, menus, recording/playback, Inferno lifecycle
remote.go          web server: auth, dashboard, settings, downloads, meter push
hyperdeck.go       Blackmagic HyperDeck control server (TCP 9993)
txholder.go        persistent inferno TX holder + playback pump
clocksync.go       statime observation poller; the recording clock gate
inferno_log.go     counts inferno2pipe receive faults into the app log
logging.go         log/slog (stderr + app.log, default Error-only)
hardware/          SSD1322 display, encoder, buttons, lamps, network detection
alsapcm/           cgo ALSA wrapper so the app can be the single Inferno client (RX + TX)
cmd/simcheck/      renders OLED screens to PNG via PI9696_SIM
deploy/            systemd units (pi9696, pi9696-clock stub, statime) + statime.toml
test/interop/      two-host accuracy harness: signal, compare, TX run, channel sweep
inferno-patches/   verified prototype patches for the inferno fork (INFERNO-UPSTREAM.md)
inferno-loopback.sh  proves inferno TX→RX on one host (tone in, tone out)
DEPLOYMENT.md      install record (this file defers to it); WIRING.md is hardware
REPORT.md          interop test results; INFERNO-UPSTREAM.md upstream issues
inferno/           Inferno AoIP server (Rust) — install-time checkout, NOT tracked
fonts/ rec/ web assets  install-time/runtime paths, NOT tracked (see .gitignore)
```

---

**Version:** 1.20.0 · **Status:** recording/playback/WebUI live; Inferno RX live, TX via app ALSA client in progress; OLED seen only in sim.
