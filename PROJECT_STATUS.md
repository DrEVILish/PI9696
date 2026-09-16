# PI9696 — Design Record

Implementation status, product decisions, and feature history. For specs/usage → `README.md`; for hardware → `WIRING.md`.

**Version:** 1.19.0 · **Status:** feature-complete per Round 3 design; deployment blocked on hardware bring-up.

---

## Current Status

**Working end-to-end** (verified by the test suite and live sim):

| Area | Status |
|------|--------|
| Recording | Inferno → FIFO → ffmpeg → 24-bit WAV in `/rec/YYYY-MM-DD/`, with start-time naming, tag presets, low-disk gating (30-min rule), fsync, mid-take auto-stop (<1 min) |
| Playback | Most recent take to local ALSA; click=pause, rotate=seek, hold=exit; progress bar + elapsed/total |
| OLED | Full menu system, status bar `[ETH] [INF] [USB]`, VU (12/page + `n/T` indicator)/waveform/info pages, access-QR on network page, brightness + auto-dim + menu timeout back to Standby |
| WebUI | Live OLED mirror, on-screen encoder/buttons, settings modal, VU meters (100 ms WebSocket), recordings browser + Download-ALL ZIP + manifest, telemetry panel (uptime, per-core CPU, RAM, temp, disk), token pre-fill via `?t=` |
| Auth | Token → session (12 h), rate-limited login, constant-time compare |
| Files | Copy/delete/format USB; config export/import (non-secret JSON) |
| Logging | Error/Warn/Info/Debug (default Error-only), journald + app.log |
| Button lamps | REC (GPIO12) lit while recording; PLAY (GPIO16) solid/blink; STOP: none |

**Deliberately excluded** (product decisions): scheduling, FLAC/MP3, analog/USB audio, local monitor, HTTPS, user accounts, mirroring/RAID, OTA, language toggle, power monitoring.

---

## Known Gaps

| # | Gap | Status |
|---|-----|--------|
| 1 | Playback via Inferno/AoIP | Blocked: Inferno contract is receive-only (no input/stream command) |
| 2 | Sim-mode OLED input | Deferred: WebUI encoder works; add only if needed |
| 3 | Recordings list at scale | Deferred: 15 s re-glob ceiling; list is scrollable per directive |

**Superseded:** Scheduled recording removed (Round 3); GPIO status LEDs removed in favour of button backlights.

---

## Design Decisions

### Round 1 — Core Behaviour

- 24-hour (HH:MM) clock throughout device and WebUI
- No user recording time limit; auto-stop at <1 min space remaining (graceful finalize)
- No power source monitoring (mains assumed)
- No OTA updates in initial release
- Playback seek/scrub (implemented in 1.16.0)
- **Target:** playback out through Inferno/AoIP (not yet implemented — Known Gaps #1)

### Round 2 — Refinements

- OLED brightness: continuous 0–100% slider (implemented)
- Auto-dim + screen saver (dim after 30 s, off after 2 min; active take/playback keeps panel bright)
- Audio source: always Inferno (AoIP); no raw ALSA device selection
- Meters: Peak + RMS only; no additional types
- File naming: custom prefix (text in WebUI, preset list on OLED)
- WebUI auth: keep token + session cookie
- WiFi AP: option to enable for WebUI reachability without RJ45 *(partial support exists)*
- Display rotation: landscape only (256×64)
- Logging: multi-tiered (Error/Warn/Info/Debug), default Error-only
- Destructive actions: confirmation dialogs as protection
- Status indication: button lamps only; status on OLED
- No beeper; no power loss recovery; LAN only; no redundancy

### Round 3 — Build Specifics

- Inferno sourced from official repos (fetched/pinned by `setup.sh`)
- Inferno is bidirectional (AES67/Dante: sends + receives)
- Channel ceiling: 1–128 (top end pending stress testing)
- All sample rates (44.1/48/96/192 kHz) selectable
- Analog/USB audio I/O: dropped (Ethernet only)
- Local monitor output: dropped (monitoring via meters + AoIP consumers)
- Button lamps: REC + PLAY only; STOP has no lamp (Round-4)
- WebUI binds every IP interface (no eth0-only limitation)
- Scheduled recording: removed from product design
- Config export/import: non-secret JSON on USB
- Meter colors: WebUI only (green/yellow/red); OLED stays grayscale

### Round 4 — Lamp Narrowing

- STOP lamp removed: nothing a user waits on; STOP action has a long-press

---

## Implementation Status

### Hardware Interface

- **Display (SSD1322, SPI)** — FiraCode TTF, named font contexts, brightness via contrast 0xC1; inert in sim
- **Encoder (EC11)** — rotate / click / hold with debouncing
- **Buttons** — Record (GPIO5), Stop (GPIO6), Play (GPIO13); internal pull-ups
- **Lamps** — REC (GPIO12), PLAY (GPIO16); change-only writes via `LampManager`; inert in sim
- **Network detection** — `net.Interfaces()` polling, interface-name gate

### Recording Engine

- Persistent Inferno server (auto-started when any IP comes up; auto-restarted on rate/channel change)
- Pipeline: Inferno → FIFO (`rec/raw/`) → ffmpeg (`s32le` → WAV 24-bit) → `/rec/YYYY-MM-DD/`
- Metering: pass-through `astats` filter → `meterReader` → peak/RMS globals (under mutex)
- Start gating: only from idle, never over active take, refused when <30 min space
- WAV INFO chunk via ffmpeg `-metadata` (`date`, `comment`); verified round-trip via `ffprobe`

### Playback

- Most recent take (mtime-sorted) to local ALSA
- Click = play/pause (SIGSTOP/SIGCONT); rotate while paused = 5 s scrub (restart ffmpeg with `-ss`); hold = exit
- SIGTERM + SIGCONT on stop (a SIGSTOP'd process defers SIGTERM — without this, stop-while-paused hangs)
- Progress bar + elapsed/total with [PAUSED] marker
- **Target:** route out through Inferno (blocked on contract — Known Gaps #1)

### WebUI

- `net/http` on `0.0.0.0` (every up interface); 5 s poll loop as interfaces come and go
- Auth: token + session (distinct random ID, 12 h expiry, swept on login); rate-limited; constant-time compare
- Dashboard: OLED PNG mirror, on-screen encoder/buttons, settings modal, status/config panels
- Recordings: per-file download + Download-ALL streaming ZIP + manifest
- Meters: `/api/meter` (one-shot) + `/ws/meter` (100 ms push, 5 s write deadline per frame)
- Input: `/api/input/encoder/{left,right,click,hold}`, `/api/input/button/{record,stop,play}`

### Files & Storage

- `/rec` (SD card) always; USB is copy/export target only
- Free-space math measures `/rec`, never USB
- Copy: per-day structure preserved; format: FAT32; download keying path-relative to `/rec`
- Download whitelist: app's own file listing (no path traversal)

### Persistence

- `/etc/pi9696/config.json` (sim: `/tmp/`; override: `PI9696_CONFIG`)
- Atomic temp+rename: device name, sample rate, channels, tag, prefix, meter range, peak hold, transport mode, log level, OLED brightness, auto-dim, menu timeout, WiFi settings
- Applied at boot

### Logging

- `log/slog` with `slog.LevelVar` threshold
- Dual sink: stderr (journald) + `/var/log/pi9696/app.log`
- Level changeable from OLED Settings → Logging and WebUI settings modal; persisted

---

## Performance & Deployment

**System:** Pi 5 (RP1 GPIO needs periph v3.8.3+ — pinned newer), Raspberry Pi OS 64-bit Trixie+, root for GPIO/SPI/ALSA/USB, port 8080 reachable if WebUI used, Inferno AES67/Dante subscription on the network. Idle CPU minimal, ~50 MB RAM, ~5 W.

**Audio:** up to 192 kHz / 24-bit / 128 ch (top end pending stress testing); ~8.3 MB/min at 48 kHz stereo; latency is AoIP-transport dependent.

### Deployment Checklist

- [ ] Hardware assembled per `WIRING.md`; SPI enabled
- [ ] `sudo bash setup.sh` (builds Inferno, installs systemd unit)
- [ ] Recording verified end-to-end (Inferno reachable; `[INF]` in status bar)
- [ ] USB copy/download verified; WebUI login verified from browser
- [ ] Log level left at Error (default) unless debugging

---

## Feature History

### 1.19.x

- **1.19.0** — Telemetry graphs: dropdown replaced by always-visible uPlot CPU (per-core lines) / RAM time graphs (5 min window, themed to deck vars, locally hosted 1.6.32 bundle); deck review fixes (orphan guides removed, tape tucked under rims, lit idlers, gap/label/HUD seating, bottom-aligned seg7 SS)

### 1.18.x

- **1.18.0** — OLED menu timeout (Display → Off/15s/30s/60s/2min, 30 s default, persisted; transport/copy/home never time out) + Standby tildes dropped; OLED access-QR on network page (1 px modules, `?t=` pre-fill on login, redirect preserves query); VU meters 12/page with bottom-right `n/T` indicator; WebUI telemetry panel (uptime, per-core CPU bars, app/inferno/system RAM, temp, /rec disk, record time); deck reels +25% with re-plotted tape path, seg7 seconds at 75%, mobile header stacks logo/OLED/transport; htmax bundle (htmx 4 + extensions) replaces htmx; USB copy worker mutex safety; per-test config overrides removed

### 1.17.x

- **1.17.1** — INFERNO-LINK deck lamp reflects Inferno state (100 ms meter push carries `infernoUp`; `applyMeter` toggles lamp `on` class)
- **1.17.0** — Button lamps driven (REC + PLAY, STOP has no lamp); WebUI recordings list as full-height scrollable panel; config export/import to USB (non-secret JSON profile)

### 1.16.x

- **1.16.2** — Data-loss hardening: fsync of finished takes + mid-take auto-stop at <1 min space
- **1.16.1** — OLED status-bar clock (24h HH:MM); WebUI VU meter colors at design's dBFS thresholds (−18/−6)
- **1.16.0** — Playback seek/scrub: encoder click = play/pause, rotate-while-paused = 5 s seek

### 1.15.x

- **1.15.0** — WebUI meter colors (green/yellow/red by level, sized to full track height)

### 1.14.x

- **1.14.0** — Download-ALL: streaming ZIP + manifest (RAM-safe for large sets)

### 1.13.x

- **1.13.0** — Recording filename prefix (text in WebUI, preset list on OLED)

### 1.12.x

- **1.12.0** — OLED brightness (0–100%) + auto-dim (30 s → dim, 2 min → off; active take/playback keeps bright)

### 1.11.x

- **1.11.0** — Multi-tier logging (Error/Warn/Info/Debug, default Error-only); level changeable from OLED + WebUI; dual sink

### Pre-1.11.0

- OLED menu system with scroll + confirmation dialogs
- Per-day Copy Files browser
- Idle-browse flow (VU meters → waveform → network/token)
- Network Info and Remote Access screens
- WiFi QR screen
- Remote control (token + session auth, every-interface binding)
- Download-ALL ZIP + manifest
- Low-disk warning on idle screen

### Audit Cuts (2026-09-07)

~800 lines removed in eight commits: dead HardwareManager/FiraCodeManager surface, WebUI demo mode, GPIO status LEDs, cmd/font-converter, xlog → log/slog, dead NetworkDetector surface, five near-identical settings-dropdown templates consolidated.

---

## QA

- `go build ./...`, `go vet ./...`, `go test ./...` green on every commit
- Suite: real handlers over `httptest` (auth, recordings API, ZIP, settings); playback/seek against fake ffmpeg; Inferno worker concurrency against stub server
- Display regressions: `cmd/simcheck` renders every OLED screen to PNG; WebUI deck rendered from served SVG
- Hardware beyond sim: verified on unit during deployment

### Documentation Map

| File | Contents |
|------|----------|
| `README.md` | Specifications, features, architecture, build, usage, troubleshooting |
| `WIRING.md` | Pinouts, wiring, power, construction, testing |
| `PROJECT_STATUS.md` | This file: design decisions, implementation status, feature history |
| `setup.sh` | Automated install (system prep, fonts, Inferno build, systemd unit) |
| Source comments | Design rationale alongside code |

---

**Project Status: feature-complete per Round 3 design; deployment blocked only on hardware bring-up. The known gaps list above is the honest remainder.**