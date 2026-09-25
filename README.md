# PI9696 — Design Guide

A 1U rack-mounted multichannel audio recorder: AES67/Dante over Ethernet, uncompressed
WAV to SD card, operated from a 256×64 OLED front panel or a token-auth web dashboard.

**Docs:** [WIRING.md](WIRING.md) · [PROJECT_STATUS.md](PROJECT_STATUS.md) · [AGENTS.md](AGENTS.md)

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
| Input | AES67/Dante via Inferno (Ethernet only; no analog/USB audio) |
| Rates | 44.1 / 48 / 96 / 192 kHz |
| Channels | 1–128 (Pi 5 throughput at top end) |
| Format | WAV PCM 24-bit on disk (32-bit internal) |
| File naming | `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav` in `/rec/YYYY-MM-DD/` |
| Display | SSD1322 256×64 OLED (SPI), FiraCode TTF |
| Controls | EC11 rotary encoder + Record/Stop/Play buttons |
| Remote | HTTP on port 8080 (token + session auth, no HTTPS) |
| Deck control | Blackmagic HyperDeck protocol on TCP 9993 (Settings → Transport toggle, default off, no auth) |
| Logging | Error/Warn/Info/Debug (default Error-only), journald + app.log |
| File size | ~8.3 MB/min at 48 kHz stereo 24-bit |

---

## System Architecture

```
                    Inferno (AES67/Dante)
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
  ffmpeg -f alsa default ← pause via SIGSTOP, seek via -ss restart
```

**Key design decisions:**
- Single Go process owns everything behind one app mutex
- Inferno lifecycle is `infernoWorker`-owned (the only goroutine that mutates inferno state)
- Recording starts only from idle, never over an active take
- Playback and recording are mutually exclusive in both directions

---

## Features

### Recording

- Manual start/stop
- Start refused when <30 min space remains at the current rate
- Take auto-stops when <1 min space remains (graceful finalize, LOW DISK warning)
- Tag presets (Show/Rehearsal/Soundcheck/Interview/Backup/None) + filename prefix
- Real-time elapsed/remaining, storage, Peak/RMS on OLED

### Playback

- Plays to Inferno ALSA (pause/resume via SIGSTOP/SIGCONT preserves position without gaps)
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
- Format USB drive (FAT32 or exFAT)
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
- Theming: ftl-themes bundles (22 themes, `third_party/ftl-themes` submodule) —
  one linked stylesheet + `html[data-theme]`; the `third_party/ftl-themes/CONTRACT.md`
  is the integration spec. Markup uses the library's own components (`.ftl-*`),
  the shared icon sprite (`/static/themes/icons/<slug>.svg`, per-theme art with a
  generic fallback) and the app-shell hooks. Density/Motion/Contrast display
  options persist device-wide beside the theme choice.

### Auth & Security

- Token (8-char, shown on OLED) → session cookie (12 h, server-side)
- Login rate-limited per IP; token compared in constant time
- Downloads whitelisted against recording list (no arbitrary file access)

**⚠ Known security limitation:** Plain HTTP only — treat as unencrypted admin page. Anyone sniffing the LAN can see the token and hijack the session. Do not expose beyond a trusted network without adding HTTPS.

**⚠ CSRF:** The WebUI relies on same-origin cookie scoping; there is no explicit CSRF token. Any site a logged-in browser visits could POST to `:8080` (e.g. `logger` on the recorder) and trigger state changes. Acceptable on a trusted LAN with a token that is never exposed in browser JS.

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
- Go 1.26+ (build), Rust/Cargo (Inferno AoIP server)
- Root access for GPIO/SPI/ALSA/USB mounting

### On the Pi

```bash
git clone <repo> /opt/PI9696 && cd /opt/PI9696
# system prep: enable SPI, install fonts, build inferno/, install systemd unit
sudo systemctl start pi9696
sudo systemctl status pi9696
sudo journalctl -u pi9696 -f
```

### Without a Pi (simulator)

```bash
PI9696_SIM=1 ./pi9696        # full app, no SPI/GPIO
go run ./cmd/simcheck        # render all OLED screens to /tmp/pi9696_shots/
```

Sim facts:
- Every frame dumped to `/tmp/pi9696_sim_frame.png` (override: `PI9696_SIM_OUT`)
- Token printed to stderr: `sim mode: remote access token XX XX XXXX`
- Config path: `/tmp/pi9696-config.json` (vs `/etc/pi9696/config.json` on real Pi)
- Recording requires `inferno/target/release/inferno` (build it with `cargo build --release` in `inferno/`)

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

- 45 tests in `main_test.go` (1 in `logging_test.go`)
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
| Recording fails | Low disk (<30 min)? Already recording? OLED flashes warning |
| WebUI unreachable | Any interface with IP? `ss -tlnp \| grep 8080` |
| USB not detected | `mount -t tmpfs none /media/usb` for testing; real USB: `lsblk` |
| Logs | `sudo journalctl -u pi9696 -f` + `/var/log/pi9696/app.log` |

---

## Known Limitations & Design Debt

These are documented in PROJECT_STATUS.md but worth flagging here:

1. **Playback via Inferno/AoIP** — blocked on Inferno's receive-only contract. Local ALSA only.
2. **No HTTPS** — plain HTTP on port 8080. Do not expose beyond trusted LAN.
3. **Directory fsync** — take content fsync'd, but parent directory entry fsync is unimplemented (power loss can lose directory entry).
4. **FIFO handoff window** — monitor→recording transition has an unbounded window where both processes read the FIFO. Acceptance documented in code comments.
5. **Config persistence** — atomic rename, but temp file not fsync'd before rename (power loss can truncate config).
6. **Meter race on monitor→record** — stale monitor goroutine can clear recording's meter slices if it completes after startRecording reallocates them. Meters may show dead for the take.
7. **No analog/USB audio I/O** — Ethernet only (product decision).
8. **Sim config path** — `PI9696_SIM=1` writes to `/tmp/pi9696-config.json`; real Pi writes to `/etc/pi9696/config.json`. Per-test env override is ignored (ConfigPath resolved once in init).

---

## Repository Layout

```
main.go            app: state machine, menus, recording/playback, Inferno lifecycle
remote.go          web server: auth, dashboard, settings, downloads, meter push
logging.go         log/slog (stderr + app.log, default Error-only)
hardware/          SSD1322 display, encoder, buttons, lamps, network detection
cmd/simcheck/      renders OLED screens to PNG via PI9696_SIM
inferno/           Inferno AoIP server (Rust) — prebuilt with `cargo build --release`
deploy/            systemd unit
rec/               recordings (per-day folders), raw FIFO scratch
fonts/             FiraCode TTFs
```

---

**Version:** 1.17.1 · **Status:** feature-complete per Round 3 design; deployment blocked on hardware bring-up.
