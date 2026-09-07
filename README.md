# PI9696 — Rack-Mount Audio Recorder

A professional 1U rack-mounted multichannel audio recorder built on the Raspberry Pi 5.
It captures an AES67/Dante (Audio over IP) stream via the persistent **Inferno** server,
records uncompressed WAV through an ffmpeg pipeline, and is operated from a 256×64 OLED
with a rotary encoder and dedicated transport buttons — or from a token-authenticated web
dashboard that mirrors the device.

**Documentation map**

| File | Contents |
|------|----------|
| `README.md` | Specifications, features, system architecture, build & install, usage |
| `WIRING.md` | Hardware connection reference, power, construction, testing |
| `PROJECT_STATUS.md` | Design decisions (all rounds), implementation status, known gaps, feature history |

---

## Specifications

| Area | Specification |
|------|---------------|
| Audio input | AES67/Dante over Ethernet via Inferno (no analog/USB audio I/O in the build) |
| Sample rates | 44.1 / 48 / 96 / 192 kHz |
| Channels | 1–128 (Pi 5 throughput at the top end pending stress testing) |
| Format | WAV, PCM 24-bit on disk (32-bit internal capture pipeline) |
| File naming | `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav`, stored in per-day folders (`/rec/2026-08-30/`) |
| Storage | `/rec` on the SD card; USB is a copy/export target only |
| Display | SSD1322 256×64 OLED (SPI), FiraCode TTF rendering |
| Controls | EC11 rotary encoder (rotate / click / hold) + Record, Stop, Play buttons |
| Remote | HTTP web UI on port 8080, bound to every up interface (eth0/wlan0), token + session auth |
| Logging | Leveled (Error/Warn/Info/Debug, default Error-only) to journald + `/var/log/pi9696/app.log` |
| Service | systemd unit (`Restart=always`), installed by `setup.sh` |
| File size | ~8.3 MB/minute at 48 kHz stereo 24-bit (scales with rate × channels) |

## Features

**Recording**
- Manual start/stop only (no scheduling — removed from the product design)
- Start is refused when less than 30 minutes of space remains at the current rate
- Tag presets (Show / Rehearsal / Soundcheck / Interview / Backup / None) and a filename
  prefix (preset list on the OLED, free text in the WebUI)
- Real-time elapsed / remaining time, storage, and Peak/RMS readout on the OLED

**Playback**
- Plays the most recent recording to the local ALSA output (`ffmpeg -f alsa default`)
- Encoder **click** toggles play/pause, **rotate while paused** scrubs (5 s per detent),
  **hold** exits; the playing screen shows a progress bar + elapsed/total
- Playback and recording are mutually exclusive in both directions
- Note: routing playback out through Inferno/AoIP is the design *target*, not yet
  implemented (see PROJECT_STATUS, Known Gaps)

**Level metering**
- Peak/RMS per channel from ffmpeg's `astats` filter running alongside the encode —
  the same readings power the OLED VU pages and the WebUI meter bank
- WebUI bars are colour-coded green/yellow/red by level (−18 dBFS / −6 dBFS breakpoints);
  the OLED stays grayscale
- Configurable meter range and peak-hold decay

**Files**
- Copy selected takes (or all) to USB with progress; delete with confirmation
- Format USB drive (FAT32); per-day folder structure preserved on copy
- WebUI: per-file download plus **Download ALL** as a single streaming ZIP with a
  `manifest.txt` (path, size, channels, rate, duration, start time per file)

**Settings (OLED + WebUI)**
- Sample rate, channel count, tag, prefix, meter range, peak hold
- OLED brightness (0–100 % continuous) and auto-dim (dim after 30 s idle, off after
  2 min; any input wakes it; an active recording/playback keeps the panel bright)
- Log level, transport-button style (icon/text), WiFi AP toggle
- All persisted to `/etc/pi9696/config.json` (atomic write) and reapplied at boot

## System Architecture

One Go process owns everything behind a single app mutex; a 100 ms render tick redraws
the OLED, and the web server, meters, and hardware callbacks all funnel through it.

```
Inferno (AES67/Dante, persistent) ──FIFO──> ffmpeg (s24le → WAV 24-bit) ──> /rec/YYYY-MM-DD/
                                   └─ astats meter tap ──> meterReader ──> OLED/WebUI
ffmpeg -f alsa default <── playback of latest take (pause via SIGSTOP, seek via restart)
```

- **Recording engine** — `startRecording` owns the FIFO: the persistent Inferno server
  (auto-started when any IP interface comes up, restarted automatically when sample rate
  or channel count changes) writes s32le frames into a FIFO; ffmpeg converts to 24-bit
  WAV. A pass-through `astats` filter reports levels without touching the audio.
- **Playback** — ffmpeg plays the newest take to local ALSA. Pause freezes the process
  (SIGSTOP/SIGCONT); seeking restarts ffmpeg at the new offset (`-ss`) and keeps state.
- **Display** — `hardware/` drives the SSD1322 over SPI with FiraCode TTF rendering;
  status bar shows `[ETH] [INF] [USB]` indicators plus time/remaining/storage.
- **Remote control** — `remote.go`: htmx dashboard (OLED mirrored as a live PNG,
  on-screen encoder/buttons driving the same handlers as the physical hardware),
  recordings browser, settings modal, per-channel meters over a 100 ms WebSocket push.
- **Logging** — `log/slog` with a dynamic threshold (default Error-only); dual sink:
  stderr (journald via systemd) + rotating `/var/log/pi9696/app.log`.
- **Simulator mode** — `PI9696_SIM=1` runs the full app with no SPI/GPIO; the framebuffer
  is dumped to `/tmp/pi9696_sim_frame.png` each frame.

## Building and Installing

### Prerequisites
- Raspberry Pi 5, Raspberry Pi OS 64-bit (Trixie or newer)
- Go 1.26+ (build), Rust/Cargo (Inferno server, built once by `setup.sh`)
- Root access for GPIO/SPI/ALSA/USB mounting

### One-step setup (on the Pi)

```bash
git clone <repo> /opt/PI9696 && cd /opt/PI9696
sudo bash setup.sh        # system prep, SPI enable, fonts, Inferno build, systemd unit
```

`setup.sh` installs a version-controlled systemd unit from `deploy/pi9696.service`
(`Restart=always`, `WorkingDirectory=/opt/PI9696`) and enables it:

```bash
sudo systemctl start pi9696
sudo systemctl status pi9696
sudo journalctl -u pi9696 -f        # follow logs
```

### Manual build

```bash
go mod tidy
go build -o pi9696 .
sudo ./pi9696                        # root for GPIO/SPI access
```

### Developing without a Raspberry Pi (simulator mode)

```bash
PI9696_SIM=1 ./pi9696                # full app, no SPI/GPIO
```

- Every frame is dumped to `/tmp/pi9696_sim_frame.png` (override: `PI9696_SIM_OUT`).
- The WebUI access token is printed to stderr at startup (`sim mode: remote access
  token XX XX XXXX`) — in sim there is no hardware input path to the OLED screen
  that normally shows it.
- `go run ./cmd/simcheck` renders every OLED screen to `/tmp/pi9696_shots/*.png`
  for layout checks without running the app.
- Recording needs the Inferno binary (`setup.sh` builds it into
  `inferno/target/release/inferno`); without it the app runs and logs a clear error.

### Rebuilding after changes

```bash
./restart-pi9696.sh          # rebuild + restart the service
./restart-pi9696.sh logs     # tail service logs
./restart-pi9696.sh status   # service status
```

## Remote Control

When any IP interface is up, the web UI is served on port 8080. The dashboard mirrors
the physical device: the live OLED image, the rotary encoder and all three transport
buttons (click/rotate/hold map 1:1 onto the hardware handlers), every setting, a
read-only config summary, and the recordings browser with download (single or
Download-ALL ZIP). Log in with the 8-character access token shown on the OLED's
Settings → Remote Access screen (two groups of 4, e.g. `K7M2 QX9F`; the space is
optional when typing). A correct token is exchanged for a session cookie valid 12
hours; the session ID is distinct from the token and expires server-side.

**Security posture — read before exposing beyond a trusted LAN:**
- The server is **plain HTTP, not HTTPS**. Anyone who can sniff the unit's LAN traffic
  can see the token and hijack the session. Treat it like an unencrypted admin page.
- The token gates every route and is compared in constant time; login attempts are
  rate-limited per source IP.
- Downloads are whitelisted against the app's own recording list (no arbitrary file
  access, no upload, no delete-over-network).
- Destructive actions (delete all, format USB, shutdown/restart) always require
  confirmation — including from the WebUI.

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Display blank / SPI errors | SPI enabled in raspi-config; wiring per WIRING.md; run as root |
| `[INF]` never lights | Ethernet up? `ip addr show eth0`; Inferno binary built (`inferno/target/release/inferno`)? |
| Recording fails to start | Low disk (30-min rule) or already recording — the OLED flashes a warning |
| WebUI unreachable | Any interface with an IP? `ss -tlnp | grep 8080`; check `remoteControlLoop` in logs |
| USB not detected | `lsblk`; mount point `/media/usb`; format/copy from the OLED menu |
| Logs | `sudo journalctl -u pi9696 -f` and `/var/log/pi9696/app.log` (raise log level in Settings → Logging) |

## Repository Layout

```
main.go            app: state machine, menus, recording/playback, Inferno lifecycle
remote.go          web server: auth, dashboard, settings API, downloads, meter push
logging.go         leveled logging on log/slog (stderr + app.log, default Error-only)
hardware/          SSD1322 display (TTF), encoder, buttons, network detection
cmd/simcheck/      dev tool: renders each OLED screen to PNG via PI9696_SIM
inferno/           Inferno AoIP server (Rust) - built once by setup.sh
deploy/            version-controlled systemd unit
setup.sh           one-step install (system prep, fonts, Inferno build, service)
restart-pi9696.sh  rebuild + restart helper
rec/               recordings (per-day folders), raw FIFO scratch
fonts/             FiraCode TTFs (fetched by setup.sh)
```
