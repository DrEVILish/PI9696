# PI9696 — Design Record

Implementation status, product decisions, and feature history. For specs/usage → `README.md`; for hardware → `WIRING.md`.

**Version:** 1.20.0 · **Status:** tbc unknown need to evaluate each feature to check implementation.

---

## Current Status

**Working end-to-end** (verified by the test suite and demo mode):

| Area | Status |
|------|--------|
| Recording | Inferno → FIFO → ffmpeg → 24-bit WAV in `/rec/YYYY-MM-DD/`, with start-time naming, tag presets, low-disk gating (30-min rule), fsync, mid-take auto-stop (<1 min) |
| Playback | Most recent take to local ALSA; click=pause, rotate=seek, hold=exit; progress bar + elapsed/total |
| OLED | Full menu system, status bar `[ETH] [INF] [USB]`, VU (12/page + `n/T` indicator)/waveform/info pages, access-QR on network page, brightness + auto-dim + menu timeout back to Standby |
| WebUI | Live OLED mirror (reloads on framebuffer change), on-screen encoder/buttons, settings modal, VU meters (100 ms WebSocket) as ftl-meter-v strips, telemetry over hx-ws push (status + CPU/RAM/temp/disk graphs, 24h clock) with conn lamp, recordings browser + Download-ALL ZIP + manifest, token pre-fill via `?t=` |
| Auth | Token → session (12 h), rate-limited login, constant-time compare |
| Theming | ftl-themes engine (submodule @ 3dd148f, v3.14.0): 22 themes as single bundles, `html[data-theme]` + OOB swap; shared icon sprite per theme at `/static/themes/icons/<slug>.svg`; display options (motion/contrast/density) persisted device-wide; markup on `.ftl-*` components (field-row, switch, slider, table, modal, meter, empty-state, input-group, vertical settings tabs, ftl-scroll boxes, spacing tokens); engine issues reported upstream (#43 meter-span fixed, #44 icon docs fixed, #45 scrollbars fixed, #49 blue-future un-archive requested) |
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

### Core Behaviour

- 24-hour (HH:MM) clock throughout device and WebUI
- No user recording time limit; auto-stop at <1 min space remaining (graceful finalize)
- No power source monitoring (mains assumed)
- No OTA updates in initial release
- Playback seek/scrub (implemented in 1.16.0)
- **Target:** playback out through Inferno/AoIP (not yet implemented — Known Gaps #1)

### Refinements

- OLED brightness: continuous 0–100% slider (implemented)
- Auto-dim + screen saver (dim after 30 s, off after 2 min; active take/playback keeps panel bright)
- Audio source: always Inferno (AoIP); no raw ALSA device selection
- Meters: Peak + RMS only; no additional types
- File naming: custom prefix (text in WebUI, preset list on OLED)
- WebUI auth: keep token + session cookie - auth token shown on OLED if the login page is open
- WebUI binds every IP interface (no eth0-only limitation)
- WiFi AP: option to enable for WebUI reachability without RJ45 *(partial support exists)*
- Display rotation: landscape only (256×64)
- Logging: multi-tiered (Error/Warn/Info/Debug), production default Error-only, dev mode, default Debug.
- Destructive actions: confirmation dialogs as protection
- Status indication: button lamps only; status on OLED
- Button lamps: REC + PLAY only; STOP has no lamp
- No beeper; no power loss recovery; LAN only; no redundancy
- Inferno sourced from official repos (fetched/pinned at install time)
- Inferno is bidirectional (AES67/Dante: sends + receives)
- Channel ceiling: 1–128 (top end pending stress testing)
- All sample rates (44.1/48/96/192 kHz) selectable
- Config export/import: non-secret JSON on USB
- Meter colors: WebUI only (green/yellow/red); OLED is grayscale
- WebUI OLED display should only update if the OLED screen changes
- Use HTMX, HTMAX and Websockets, don't use polling.

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
- [ ] Inferno built and systemd unit installed
- [ ] Recording verified end-to-end (Inferno reachable; `[INF]` in status bar)
- [ ] USB copy/download verified; WebUI login verified from browser
- [ ] Log level left at Error (default) unless debugging

---

## QA

- `go build ./...`, `go vet ./...`, `go test ./...` green on every commit
- restart the service after each build
- Suite: real handlers over `httptest` (auth, recordings API, ZIP, settings); playback/seek against fake ffmpeg; Inferno worker concurrency against stub server

### Documentation Map

| File | Contents |
|------|----------|
| `README.md` | Specifications, features, architecture, build, usage, troubleshooting |
| `WIRING.md` | Pinouts, wiring, power, construction, testing |
| `PROJECT_STATUS.md` | This file: design decisions, implementation status, feature history |

---
