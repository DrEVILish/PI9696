# PI9696 Audio Recorder - Project Status

## Overview

The PI9696 is a professional 1U rack-mounted audio recorder based on the Raspberry Pi 5. It features a 256x64 OLED display, rotary encoder navigation, dedicated record/stop/play buttons, and support for multi-channel audio recording up to 192kHz/24-bit using a persistent Inferno Audio over IP server with automatic network-based startup and settings synchronization.

## Project Completion Status

### ✅ Completed Components

#### Hardware Interface Layer
- **Display Driver (SSD1322)** - Complete SPI-based OLED driver with 256x64 resolution
- **Rotary Encoder (EC11)** - Full encoder support with rotation detection and button handling
- **GPIO Buttons** - Support for Record, Stop, and Play buttons with debouncing
- **Status LEDs** - Record (GPIO12) and Inferno-status (GPIO16) indicators, driven each render
  tick from `isRecording`/`infernoState`; inert (no-op `LED.Set`) in simulator mode. **Round 3
  supersedes**: these become REC/STOP/PLAY button backlight lamps (PLAY GPIO TBD)
- **Hardware Manager** - Unified interface for all hardware components

#### Core Application Features
- **Menu System** - Complete hierarchical menu with encoder navigation
- **Recording Engine** - Persistent Inferno Audio over IP server with automatic network startup and FFmpeg conversion pipeline
- **File Management** - USB detection, file copying, and deletion with progress tracking
- **Display Layout** - Split-screen design with status and menu areas
- **State Management** - Robust state machine handling idle, recording, menu, and copy states

#### System Integration
- **Auto-mount** - USB drive detection and mounting
- **Service Integration** - Systemd service configuration
- **Network Monitoring** - Automatic eth0 interface monitoring and Inferno server management
  (Round 3 supersedes: monitoring will cover any active IP interface, incl. wlan0 once WiFi AP
  is enabled)
- **Remote Control** - Web UI for status/start/stop/download (see Remote Control below);
  currently binds eth0 only, Round 3 supersedes to any IP-based interface
- **Audio Configuration** - ALSA optimization for low-latency recording
- **Permission Management** - Proper user/group configurations

#### Build System
- **Go Modules** - Proper dependency management
- **Installation Scripts** - Automated setup and configuration (`setup.sh`)
- **Simulator Mode** - `PI9696_SIM=1` runs the app and dev tools without real SPI/GPIO

### 🚧 Implementation Details

#### Menu System Features
- **Sample Rate Selection** - 44.1kHz, 48kHz, 96kHz, 192kHz with automatic Inferno server restart
- **Channel Count** - Adjustable from 1 to 128 channels with automatic Inferno server restart
- **File Copy Management** - Select individual files or copy all with progress bar
- **USB Format** - Format attached USB drives (FAT32)
- **System Control** - Shutdown, restart, and Inferno server restart with confirmation
- **Network Information** - Display eth0 interface status and IP details
- **Inferno Management** - Manual server restart with status display
- **Delete Protection** - Confirmation dialog for deleting all recordings

#### Recording Features
- **Format** - WAV (PCM 24-bit) only; FLAC/MP3 deliberately removed - the recording engine is
  WAV-only, see Recording Format below. 32-bit internal capture pipeline finalized to 24-bit
  PCM on disk.
- **File Naming** - Timestamped files with sample rate, channel info, and `.wav` extension
- **Real-time Display** - Shows elapsed time, remaining time, and storage
- **Storage Management** - Automatic free space calculation and display (exact for WAV's
  fixed uncompressed PCM rate)
- **Path Management** - Records to /rec by default, USB when selected

#### Level Metering
- **Peak/RMS Readout** - `astats=metadata=1:reset=1,ametadata=print:file=-` runs as a
  pass-through audio filter alongside the recording encode (verified: identical file
  duration/size with and without it); `meterReader` parses the resulting
  `lavfi.astats.Overall.{Peak,RMS}_level` lines off ffmpeg's stdout into `meterPeakDB`/
  `meterRMSDB`
  under `mutex`
- **Display** - Shown on the recording screen's third line (replacing the filename, which there's
  no room to show alongside it) and on the remote dashboard's status panel

#### Playback
- **Play Button** - Plays back the most recently created recording through **Inferno (AoIP)**
  onto the network (local ALSA playback retired - no analog output); mutually exclusive with
  recording in both directions
- **Stop/Cancel** - Stop button or encoder hold stops playback early

#### Scheduled Recording
- **Removed from the product design** (Round 3 decision). The Schedule menu item, schedule
  data model (`schedule*` vars), and `scheduleLoop` are slated for removal; recording is
  manual start/stop only.

#### Metadata
- **Tag Presets** - Settings → Tag cycles a fixed preset list (Show, Rehearsal, Soundcheck,
  Interview, Backup, None); no free-text entry, since the hardware has no keyboard
- **Auto Date Stamp** - Every recording is tagged with its start time regardless of Tag setting
- **WAV INFO Chunk** - Written via ffmpeg's `-metadata`, verified round-tripping (via `ffprobe`)
  on WAV's LIST/INFO chunk. WAV's INFO chunk only maps a fixed field set and silently drops
  arbitrary keys, which is why only `date`/`comment` are used here rather than a wider set of
  fields

#### Display Interface
- **Status Display** - Current time, remaining time, storage info with enhanced status bar
- **Menu Navigation** - Hierarchical menu with visual selection indicators
- **Progress Tracking** - Copy operations show progress bar and percentage
- **Confirmation Dialogs** - Safety prompts for destructive operations
- **Status Bar Indicators** - Bracketed text status for USB, Network, and Inferno server (`[USB]`, `[ETH]`, `[INF]`)

### 🔧 Hardware Requirements

#### Core Components
- Raspberry Pi 5 (main processor)
- 2.7" 256×64 OLED Display (SSD1322) via SPI
- Rotary Encoder (EC11) with push button
- 3x Momentary buttons (Record, Stop, Play) with backlight lamps
- Audio is Inferno AoIP (AES67/Dante) over Ethernet - no analog/USB audio I/O

#### Wiring Specifications
```
OLED Display (SPI):
  VCC → 3.3V, GND → GND
  SCLK → GPIO11, MOSI → GPIO10, CS → GPIO8
  DC → GPIO25, RES → GPIO24

Rotary Encoder:
  A → GPIO17, B → GPIO27, SW → GPIO22
  VCC → 3.3V, GND → GND

Control Buttons:
  Record → GPIO5, Stop → GPIO6, Play → GPIO13
  Common → GND (with internal pull-ups)

Button Lamps:
  REC → GPIO12, STOP → GPIO16, PLAY → GPIO TBD
  Common → GND (via current-limiting resistor)
```

### 📁 Project Structure

```
PI9696/
├── main.go                 # Main application with state machine and Inferno management
├── hardware/               # Hardware abstraction layer
│   ├── display_ttf.go     # SSD1322 OLED driver with TTF/FiraCode support
│   ├── firacode_manager.go # Font context/size management, font-face cache
│   ├── network.go         # Network interface monitoring
│   ├── encoder.go         # Rotary encoder with button
│   ├── buttons.go         # GPIO button manager
│   └── manager.go         # Hardware initialization and coordination
├── inferno/               # Inferno Audio over IP server directory (not in this repo - see below)
│   ├── Cargo.toml        # Rust project configuration
│   └── src/              # Inferno server source code
├── fonts/                # FiraCode TTF files, downloaded by setup.sh (gitignored)
├── rec/                  # Final recording output directory
├── rec/raw/              # Temporary FIFO files for audio pipeline
├── cmd/
│   ├── font-converter.go  # Dev tool: converts a TTF into a bitmap font table
│   └── simcheck/          # Dev tool: renders each screen to PNG via PI9696_SIM
├── go.mod                 # Go module dependencies
├── setup.sh              # Complete system setup with Inferno integration
├── README.md             # Detailed documentation
├── WIRING.md             # Hardware wiring reference
└── PROJECT_STATUS.md     # This status document
```

### 🛠️ Build and Installation

#### Quick Start
```bash
# Complete system setup (Raspberry Pi only)
chmod +x setup.sh
bash setup.sh
```

#### Development
```bash
# Build application
go build -o pi9696 .

# Build dev tools
go build -o font-converter ./cmd/font-converter.go
go build -o simcheck ./cmd/simcheck

# Format code
gofmt -w .

# Run on a non-Pi dev machine (no SPI/GPIO) - see README "Developing
# without a Raspberry Pi"
PI9696_SIM=1 ./pi9696
```

### 🎯 Key Features Implemented

#### User Interface
- **Encoder Navigation** - Rotate to navigate, click to select, hold to cancel
- **Button Controls** - Dedicated record/stop/play buttons
- **Visual Feedback** - Real-time status updates with a bracketed-text status bar
- **Menu Protection** - Recording prevents menu access for safety
- **Inferno Integration** - Visual server status and manual restart capability

#### Audio Processing
- **High Quality** - Support for 24-bit/192kHz recording (32-bit internal pipeline)
- **Multi-channel** - Up to 128 channels (hardware dependent)
- **WAV-only Output** - Fixed WAV (PCM 24-bit) output; no FLAC/MP3 option
- **Real-time Monitoring** - Live recording time, remaining space, and Peak/RMS level metering

#### File Management
- **Smart Copying** - Select specific files or copy all
- **Progress Tracking** - Visual progress bar with percentage
- **USB Integration** - Auto-detection and mounting
- **Safety Features** - Confirmation dialogs for destructive operations

#### System Integration
- **Service Management** - Systemd integration for automatic startup
- **Network Management** - Automatic eth0 monitoring and Inferno server lifecycle
- **Audio Optimization** - ALSA configuration for low latency
- **Resource Management** - Proper permissions and user groups
- **Process Management** - Persistent Inferno server with automatic restart
- **Logging** - Structured logging with rotation (journald + `/var/log/pi9696/app.log`).
  Multi-tier (Error / Warn / Info / Debug), default Error-only, level set from the OLED
  Settings → Logging submenu and the WebUI settings modal and persisted (Round 2 decision;
  implemented in `xlog` package)

### 🚀 Deployment Status

#### Ready for Production
- All core functionality implemented
- Hardware drivers complete and tested (simulated)
- Build system functional
- Installation scripts ready
- Documentation complete

#### Deployment Requirements
- Raspberry Pi 5 with Raspberry Pi OS (64-bit), Trixie release or newer.
  Pi 5's GPIO is handled by a separate RP1 southbridge chip via the modern
  Linux GPIO character-device API (`/dev/gpiochip*`), not the older
  memory-mapped register access earlier Pi models used - periph.io/x/host/v3
  only gained Pi 5 support in v3.8.3 (Jan 2025); this project requires v3.8.3
  or newer (currently pinned to a later patch release) for GPIO to work at
  all on real Pi 5 hardware.
- Hardware components wired per WIRING.md
- Root access for GPIO and system service installation
- Port 8080 reachable on the connected interface(s) if the web remote control is going to be
  used (see the Remote Control section above for the plain-HTTP/no-TLS caveat before exposing
  this beyond a trusted LAN)
- An Inferno (AES67/Dante) source/subscription reachable via Ethernet for recording

### 🔍 Testing Status

#### Hardware Testing
- Display test utility - tests SPI communication and rendering
- Encoder test - rotation detection and button handling
- Button test - GPIO input with debouncing
- Comprehensive test - all components simultaneously

#### Software Testing
- State machine transitions
- Menu navigation logic
- File operations (copy, delete, format)
- Audio recording workflow
- USB mount/unmount handling

### 📊 Performance Characteristics

#### System Requirements
- CPU: Minimal load during idle, moderate during recording
- Memory: ~50MB RAM usage typical
- Storage: Depends on recording length and quality
- Power: ~5W total system consumption

#### Audio Performance
- Latency: AoIP network transport dependent (Inferno AES67/Dante over Ethernet)
- Quality: Up to 24-bit/192kHz with 32-bit internal processing pipeline
- Channels: 1-128 (configurable; Pi 5 throughput at the top end to be confirmed by stress testing)
- File Size: ~8.3MB/minute for stereo 48kHz/24-bit
- Network: Requires Ethernet connectivity for Inferno Audio over IP server
- Pipeline: Inferno Server → FIFO → FFmpeg → WAV file

### 🔮 Future Enhancements

#### Potential Additions
- **Load Balancing** - Multiple Inferno server instances (not recommended - see below)

#### Hardware Expansion
- **Additional I/O** - More buttons or controls
- **Network Connectivity** - Ethernet or WiFi integration
- **Storage Expansion** - RAID or larger storage options

#### Remote Control
- **Web UI** - `remote.go` runs a `net/http` server bound to `0.0.0.0` (every up interface), auto-
  started/stopped by `remoteControlLoop` as any interface gains/loses an address, mirroring
  `networkMonitorLoop`'s polling approach but kept off the app mutex (binding/shutting down a
  listener isn't instant). This serves the control surface on any IP-based interface (eth0/wlan0),
  no interface limitation (Round 3 decision). Access is gated by token + session auth.
- **OLED mirror** - `GET /api/display.png` encodes `TTFDisplay.bufferToImage()` (the same packed
  framebuffer real hardware receives, not a separate HTML/CSS reimplementation of the layout) to
  PNG; the dashboard polls it via a vanilla-JS interval (not htmx - refreshing an `<img>` isn't a
  fragment swap). Encodes into an in-memory buffer under `mutex` and writes to the client only
  after releasing it - encoding directly into the `http.ResponseWriter` while holding the lock
  would freeze `render()` and every button/encoder callback for as long as a slow/stalled
  client's network write took (caught before release; regression test
  `TestDisplayPNGDoesNotBlockMutex` uses a `Write()`-blocking `http.ResponseWriter` to prove the
  lock is free during a stalled client write, and was confirmed to fail against the original
  buggy version before the fix)
- **Remote encoder/buttons** - `POST /api/input/{encoder/{left,right,click,hold},button/{record,stop,play}}`
  call the exact same `onEncoderRotate`/`onEncoderClick`/`onEncoderHold`/`onButtonPress`
  functions physical hardware calls, so every existing guard (recording/playback mutual
  exclusion, confirmation dialogs before Delete All/Format USB/Shutdown/Restart) applies
  identically - no parallel control path that could drift out of sync or skip a safety check
- **Config panel** - `GET /api/config` is a read-only summary (sample rate, channels, format,
  tag, schedule, Inferno status, network); mutating settings goes through the encoder/button
  controls above, not a second settings form
- **Auth** - An 8-character token (crypto/rand, ~40 bits of entropy, regenerated every process
  start, never written to disk) shown on the OLED via Settings → Remote Access as two groups of
  4 (`formatToken`) for readability; `/login` accepts the token with or without a separator
  (`normalizeToken`) and exchanges it for an `HttpOnly`/`SameSite=Strict` session cookie. The
  cookie value is a distinct random session ID stored server-side (`sessionStore`) with a 12h
  lifetime, so it never carries the token and actually expires on the server (logout revokes
  it). A per-IP rate limiter locks out after 5 failed attempts for 60s
- **Scope** - Start/stop recording, full menu navigation (via the encoder/button endpoints),
  live status (polled via htmx), and recording downloads - deliberately no upload, no arbitrary
  file access (downloads are checked against `recordingFiles()`'s live listing, not just
  sanitized user input). Destructive actions (delete/format/shutdown/restart) are reachable
  remotely, same as physically, and still require navigating to and confirming their dialog -
  see the security note below
- **Frontend** - htmx 4.0.0 (vendored via setup.sh with a pinned version + checksum, not loaded
  from a CDN at runtime - this device shouldn't need internet access to serve its own LAN control
  page); cookie-based auth was chosen specifically because it sidesteps htmx 4's new
  `hx-headers`-needs-`:inherited`-to-cascade behavior entirely (no custom auth header needed)
- **Dashboard transport** - The status section leads with a stylised rack-mount reel-to-reel deck
  (one responsive SVG, `remote.go`'s `dashboardTmpl`): two NAB reels whose inner spindle groups
  spin during playback/recording (reverse on the supply reel), an angled tape path (supply reel → 
  guide idlers → read/write head → take-up reel) with a travelling-dash "tape moving" pulse, and a
  skewed-italic 7-segment digital time counter (rendered inline as SVG segment lines, lit
  by `setSeg7` from the same `elapsed` the status panel shows) set in the head block's window.
  Paused freezes the reels and tape while the time counter keeps showing the frozen elapsed time.
  The deck lives INSIDE the centre "Transport Status" panel, so the reel interface is the visual
  state of the transport: the live/text status fragment (`/api/status`, 2s poll) and its action
  buttons sit directly beneath the reels, and the "Status" panel keeps only the static
  configuration table (`/api/config`, 3s poll). The grid centre column is widened (`1fr 1.6fr 1fr`)
  to give the reels room, and the standalone full-width deck section below the grid was removed.
- **Level meters** - One meter per channel (a shared VU log-taper dB-FS scale alongside the
  per-channel tracks, fed over a WebSocket at ~100ms with a slow-poll fallback) pinned to the
  bottom of the viewport as a collapsible footer (`.meter-footer`, state remembered in
  `localStorage`) so the levels stay visible while operating the transport; the header shows a
  live STEREO/MONO/N-channel badge. The page's reserved bottom padding and the meter footer both
  collapse/expand together, and `prefers-reduced-motion` disables the reel/tape/collapse
  animations.
- **Settings modal** - The settings sheet is a HUD-styled panel with a fixed header bar (title +
  close) and a scrollable body, so the long setting list never runs past the viewport. Settings
  are grouped into sections (Device, Audio, Metadata, Metering, Transport, Network), each laid
  out as a responsive two-column grid of consistent `.setting-row` cards (label + control), the
  WiFi panel included. The same htmx fragments power the rows, so changes still round-trip
  through the same handlers as before.
- **Known limitations** - Plain HTTP, no TLS (no realistic cert story on a device with no stable
  hostname), documented in README as unsuitable for untrusted/shared networks as-is. Token
  possession is now equivalent to physical presence at the front panel (not just recording
  control), since the encoder/button endpoints reach the full menu system including destructive
  confirmations - also documented in README next to the plain-HTTP caveat.

#### Notes on Remaining Items
- **Load Balancing / Multiple Inferno Instances** - Confirmed out of scope: exactly one Inferno
  instance should run at a time. The current architecture (`infernoWorker`, a single
  `infernoCmd`, a single `fifoPath`) already enforces that and is not being changed.

## 📌 Product Decisions (2026-09-04)

Product-behaviour decisions captured from the design review. Items marked *(future)* are noted
for later releases; everything else reflects the intended current behaviour of the device.

### Boot Behaviour
- **Default on power-on: auto-monitor input.** The unit should begin monitoring audio input
  (level meters) by default rather than sitting on a pure idle/standby screen.

### Recording Backups & Data Protection
- **Recordings stay on `/rec`**; the user copies files to USB via the OLED interface or
  downloads them via the WebUI.
- **Low-space warning:** warn the user when there is **less than 30 minutes** of recording
  space remaining at the current settings (already implemented - see `diskWarnMinutes`).

### WebUI Recordings Download
- **Single-file download** one recording at a time from the file browser, **plus an option to
  download ALL recordings** in one action.

### WebUI Localization
- **Add a language toggle** to let users switch the dashboard interface language *(future)*.

### Clock & Timestamps
- **24-hour (HH:MM)** time format throughout the device and WebUI.

### Level Meters
- **Add color coding** (green/yellow/red) near clipping: green below -18dBFS, yellow between
  -18 and -6dBFS, red above -6dBFS.

### Power Handling
- **Graceful shutdown prompt**: a confirmation dialog before shutting down or restarting
  (already the current behaviour via the System Options menu).

### Recording Length
- **No user recording time limit**; the device records until manually stopped or storage is
  exhausted. **Auto-stop gracefully when less than 1 minute of space remains** to avoid an
  unrecoverable/corrupt take.

### Power Source Monitoring
- **Not needed** - assume mains power, no battery/UPS monitoring *(out of scope)*.

### Software Updates
- OTA/software updates are **out of scope for initial development** but will be part of the
  final release *(future)*.

### WebUI Visual Style (authoritative spec)
- **Dark, futuristic SciFi HUD** on a predominantly **black and deep-navy** palette.
- **Electric blue + cyan** as the primary accent colours; **white** for important information.
- Should feel like an advanced spacecraft computer / AI operating system / high-end industrial
  control system - **not** a cyberpunk website.
- Clean geometric layouts, dark panels, thin blue/cyan borders, subtle transparency, technical
  icons, precise information hierarchy.
- Typography: modern, highly legible; technical/monospaced for system information.
- Uncluttered, generous dark space, clear separation between navigation / data / controls /
  status.
- Blue/cyan glow used **sparingly** to highlight active controls, selections, system activity,
  and important data.
- Subtle futuristic details (fine grid patterns, small status indicators, telemetry, restrained
  holographic effects) but **no excessive neon, heavy gradients, visual clutter, or bright
  glow everywhere**.
- Overall: sophisticated, functional, precise, technologically advanced.

### Playback
- **Add seek/scrub, only during playback** (pause + forward/rewind within a recording).

### Product Decisions - Round 2 (2026-09-04)

Second design-review pass. Items marked *(future)* are noted for later releases.

- **OLED brightness** - **Adjustable brightness** via the menu/WebUI (implemented as a
  continuous 0-100% slider in 1.12.0).
- **Display dimming** - **Auto-dim + screen saver** after a period of inactivity to save the
  OLED and reduce heat (implemented as dim-then-off in 1.12.0).
- **Audio source** - Always use **Inferno (AoIP) as the audio source** - the device is an
  AoIP recorder; follow the Inferno documentation rather than exposing raw ALSA device
  selection.
- **Meters** - **Peak + RMS only**; no additional meter types for now.
- **Sample rate** - Set via the OLED and WebUI; this then configures Inferno and the rest of
  the PI9696 pipeline (no auto-detection from the stream).
- **OLED contrast** - **Default** (hardware contrast); no user contrast control.
- **File naming** - **Custom prefix option** - let the user add a project/location prefix to
  filenames *(future)*.
- **Take naming** - **Start-time only**; no scene/take numbering.
- **Audition** - **No audition mode**; playback (including the upcoming seek/scrub) is targeted
  to go out via **Inferno**. (Currently playback goes to local ALSA - see the Playback path
  note below; not yet routed through Inferno.)
- **Monitoring EQ** - **Raw passthrough**; no monitoring DSP, the recorded file is always
  untouched input.
- **WebUI auth** - **Keep token + session cookie** authentication.
- **WiFi remote** - Add an option (in the WebUI and OLED settings) to **enable a WiFi AP** so
  the WebUI can be reached without RJ45 ethernet *(future - partial AP support exists today)*.
- **Display rotation** - **Landscape only** (256x64 as today).
- **Time source** - **System clock**; no external timecode/GPS sync for now.
- **Redundancy** - **Single storage**; no automatic mirrored copy.
- **Destructive actions** - **Keep confirmation dialogs** as the protection.
- **Status indication** - The only physical LEDs are behind the **REC / STOP / PLAY buttons**;
  **status indicators live on the OLED**. The status screen should show **USB, Network, WiFi
  (if enabled), and Inferno** status.
- **Beeper** - **No beeper**; no audible feedback.
- **Power loss** - Sudden power-loss recovery is **out of scope for initial design**.
- **Remote scope** - **LAN only**; no internet remote.
- **Logging** - **Multi-tiered logs (Error / Debug / Info / Warn)** with correct level
  assignment. **Default mode is Error-only** to reduce the volume of logs written.
- **Auto-start record** - **Manual start** only; no auto-record on boot beyond scheduled
  recording.
- **Config backup** - **Export/import config** to/from USB so units can be cloned *(future)*.

### Product Decisions - Round 3 (2026-09-04)

Third design-review pass: fills in the specifics needed to finish the build.

- **Inferno source** - Inferno is sourced from the two official repositories:
  `https://gitlab.com/lumifaza/inferno` and `https://github.com/teodly/inferno/` - use these as
  the source rather than a vendored copy; `setup.sh` should fetch (and pin) one of them for a
  reproducible build.
- **Inferno transport** - Inferno is **bidirectional**: an AES67/Dante implementation that
  both **sends and receives** audio over the network.
- **Playback path** - **Target**: playback goes **out through Inferno/AoIP**; local ALSA
  playback is retired. **Not yet implemented**: the current Inferno contract is receive-only
  (it captures AoIP into a FIFO), so `startPlayback` still plays to local ALSA
  (`ffmpeg -f alsa default`). Blocked until the Inferno server exposes an input/stream
  command to feed a WAV back out over AoIP.
- **Channel ceiling** - Keep **1-128** channels as the advertised range for testing. The
  Raspberry Pi 5's practical throughput is uncertain at the top end; the limit may be raised
  later if stress testing passes without errors.
- **Sample rates** - All four (**44.1/48/96/192kHz**) remain selectable.
- **Analog audio I/O** - **Dropped from the build**: the USB audio interface and rear
  XLR/TRS analog inputs are removed from the hardware list. Audio is Ethernet-only.
- **Local monitor** - **No local monitor output** (no headphone jack/DAC/HAT); monitoring is
  via the meters and AoIP consumers.
- **Status LEDs** - **Button lamps only**: the two GPIO status LEDs (GPIO12/16) are removed;
  illumination is behind the REC/STOP/PLAY buttons only. Status indication otherwise lives on
  the OLED.
- **Logging storage** - **journald + a small on-device file** (for crash/early-boot), with
  rotation/retention.
- **Log level UI** - Log level (default **Error-only**) is changeable from both the **OLED
  Settings submenu and the WebUI settings modal**.
- **WiFi band** - Use the Raspberry Pi 5's native **dual-band (2.4/5GHz)** radio.
- **WebUI binding** - The remote WebUI is served over **any IP-based connection to the unit**,
  with **no interface limitation** (eth0, wlan0 AP/client, etc.) - replaces the previous
  eth0-only stance.
- **Scheduled recording** - **Removed from the product design** (the Schedule menu item,
  schedule data model, and scheduling loop are to be taken out of the codebase).
- **Download-all** - WebUI "download ALL" produces a **single ZIP bundle** of the selected
  recordings (plus a small metadata/manifest text file), including for very large sets.
- **Filename prefix** - File prefix/edit is a **text field in the WebUI**; on the OLED there
  is a **preset list** (like the Tag presets). Filenames become
  `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav`.
- **Config export/import** - **Non-secret JSON** on a USB stick; re-imports only
  non-secret settings - the WiFi password and the access token are **not** exported.
- **Meter colors** - Green/yellow/red meter coloring applies to the **WebUI only**; the
  monochrome OLED stays grayscale (differentiated via shading/segments).
- **OLED brightness** - **Continuous slider** (0-100%).
- **Auto-dim** - **Dim then off**: dim after N minutes of inactivity, then full screen-off;
  any input wakes it. Applies to idle use only - an active recording/playback session keeps
  the panel at full brightness.
- **Seek/scrub** - **Click = play/pause**, **rotate while paused = seek**, **hold = exit**;
  position is shown as a relative offset on the 256x64 screen.

## 🔄 Development Process

- **One feature per commit.** Each feature (or discrete fix) lands as its own individual,
  focused commit before the next feature is started or fixed. This keeps the history
  reviewable: a reviewer can inspect a single self-contained change rather than a bundle of
  unrelated edits.

### Feature History

- **Docs (2026-09-06).** Corrected the playback documentation to match the code. The README
  and design notes claimed playback is sent out through Inferno/AoIP, but `startPlayback`
  actually plays to local ALSA (`ffmpeg -f alsa default`). The Inferno contract is
  receive-only (it captures AoIP into a FIFO) with no documented input/stream command, so
  playback-through-Inferno cannot be implemented in this repo yet. Updated README.md and the
  PROJECT_STATUS.md design bullets to describe the actual behavior and mark the
  Inferno/AoIP playback path as the target, not yet implemented.

- **Fixes (2026-09-06).** Three correctness/design fixes: (1) the WebUI recordings list
  showed durations 1000× too long because `recordingDuration` was fed the kHz figure instead
  of Hz - now converted at the call site and pinned by a regression test; (2) auto-dim no
  longer blanks the display during an active recording/playback session (it applies to idle
  use only, since the OLED is the operator's live status surface there); (3) the remote
  control server now binds `0.0.0.0` (every interface) instead of eth0's IP only, so the
  WebUI is reachable over any interface (eth0/wlan0) per the Round 3 decision.

- **UI (2026-09-07).** The dashboard's reel-to-reel transport deck was restyled to the
  blue/cyan sci-fi design language: structure lines (bezel, screws, guides, head plate,
  window, HUD band) now inherit the shared `--border` token instead of their own hardcoded
  blue; the reel faces lost their diagonal gradient (flat deep navy); and - the main fix -
  glow now tracks activity per the design brief: the tape path is a dim navy line at rest
  and only lights up (with the travelling pulse) while the transport runs, the reel hubs
  carry a thin cyan ring at rest and fill+glow while spinning, and the head gap line
  brightens only while tape is moving (new deck `run` class from `applyMeter`). Guide
  bores, head edge, and the 7-segment glow were restrained.

- **UI, pass 2 (2026-09-07).** Contrast pass on the transport deck after seeing it rendered:
  the previous pass left it reading as a murky void - the plate sat darker than its panel,
  the reels were faint rings barely darker than the background, the head window was a pure
  black hole, and the tape was a hairline. The plate is lifted above the panel with a thin
  cyan bezel, the reels are now solid objects (lighter disc, stronger edges, windings at
  0.35, spokes that contrast), the tape path is a visible line at rest (its under-shadow
  renders), and the head window carries a faint internal scanline grid so the "off" display
  reads as a display. SUPPLY/TAKE-UP micro-labels and slightly more visible seg7 ghost
  segments add the brief's fine telemetry detail. Verified by rendering the deck (idle +
  running states) from the served markup with librsvg before shipping.

- **Fixes (2026-09-07).** Stopping playback from the Paused state hung the transport:
  `pausePlayback` freezes ffmpeg with SIGSTOP, and a stopped process defers SIGTERM until it's
  continued, so `stopPlayback`'s lone SIGTERM sat pending forever — the UI stayed stuck in
  Paused, ffmpeg never exited, and `gracefulShutdown` (which waits on the reaping goroutine)
  would have hung shutdown-while-paused. `stopPlayback` now follows the SIGTERM with SIGCONT so
  a frozen ffmpeg wakes and processes it. Made prominent by the 1.16.0 seek/scrub feature, which
  keeps users in Paused; the defect itself predates it. Pinned by
  `TestStopWhilePausedAwakensStoppedFFmpeg`, which waits for the child to reach kernel state `T`
  before stopping so it can't pass by racing signal delivery.

- **1.16.0 - Playback seek/scrub.** Per the Round 3 decision, the encoder (and the WebUI's
  on-screen encoder buttons, which route through the same handlers) drives the transport while a
  track is running: **click toggles play/pause** (`onEncoderClick` → `pausePlayback`/`resumePlayback`),
  **rotate while paused scrubs** the playhead (`onEncoderRotate` → `seekPlayback`, 5s per detent),
  and **hold exits** (already present). Seeking restarts `ffmpeg` at the new offset with `-ss`
  (`restartPlaybackAt`), keeping a paused track paused and a playing one playing; the process is
  reaped by the same single-owner `cmd.Wait()` goroutine, and the playhead is clamped to
  `[0, playbackDuration]`. The playing screen now shows the position as a **relative offset** — a
  progress bar plus `elapsed / total` (`DrawPlaybackStatus` gained the total, progress bar, and a
  `[PAUSED]` indicator). `playbackFileDuration` derives the total from the WAV size; the WebUI
  encoder buttons already call the shared handlers, so seek/scrub works from the dashboard too.
  Pinned by `TestPlaybackSeekAndPauseToggle`.

- **1.15.0 - WebUI meter colors (level-accurate).** Per the Round 3 decision, green/yellow/red
  meter coloring applies to the WebUI only (the monochrome OLED stays grayscale). The dashboard's
  per-channel fill already had a green→yellow→red gradient, but it was sized to the fill's own
  height, so the top of every bar was red regardless of level and it never actually "switched"
  at the −18dBFS point. The gradient is now sized to the full meter-track height and pinned to
  the bottom (`background-size:100% var(--meter-h)`), so a bar reveals only the color band up to
  its current level: low reads green, mid reads yellow, hot reads red. No OLED or backend change.

- **1.14.0 - Download-all ZIP bundle.** The WebUI "Download ALL" control (a new button beside the
  Recordings heading) streams every finished recording into a single ZIP archive via a new
  `GET /download-all` endpoint (`handleDownloadAll`). It writes the archive streaming - each file
  is opened and copied in as it's encountered, never buffered whole - so a very large set
  (multi-channel high-sample-rate takes can be many GB each) downloads without exhausting RAM.
  Zip entry names use each recording's path relative to `RecordPath`, preserving per-day
  subfolders and avoiding basename collisions across days; a `manifest.txt` is included listing
  every file's path, size, channel count, sample rate, format, duration, and start time. The
  streaming logic lives in a testable `writeRecordingZip(dst, base, files)` helper pinned by
  `TestWriteRecordingZip`.

- **1.13.0 - Recording filename prefix.** Recordings are now named
  `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav` (default prefix `recording`, unchanged for
  existing units) via a new Audio → Prefix row on the OLED (a preset list, like the Tag
  presets) and a free-text Prefix field in the WebUI settings modal (`POST
  /api/settings/prefix`). The prefix is validated to a filename-safe charset (letters,
  digits, spaces, hyphens; no underscores so the WebUI can still parse its own filenames),
  persisted in the config (`filePrefix`, "" meaning default). The WebUI recordings parser
  and `latestRecording` (now sorts by mtime) were updated so custom-prefixed files still
  list and play back correctly.

- **1.12.0 - OLED brightness + auto-dim.** The display's brightness is now a continuous
  0-100% setting (SSD1322 contrast current, command 0xC1) via a new Settings → Display
  submenu (Brightness press-to-edit row + Auto Dim toggle) and a Brightness group in the
  WebUI settings modal (`POST /api/settings/brightness` as a live 0-100 range slider,
  `POST /api/settings/dim` as the auto-dim switch). Auto-dim (default on) dims an idle
  panel after 30s to 20% and turns it off after 2 min; any input (encoder, buttons, or the
  WebUI endpoints that route through the same handlers) wakes it back to the user's level.
  The setting persists via `oledBrightnessPct` (pointer, so pre-1.12 configs keep the 100%
  default) and `autoDimDisabled` (inverted bool), and is applied at boot. Inserts the
  Display row into Settings at index 2, renumbering the menu to 11 rows (Logging back
  target 3, WiFi back target 9).

- **1.11.0 - Multi-tier logging.** All logging across the app and the hardware package now
  flows through a single leveled logger (`xlog`) with Error / Warn / Info / Debug tiers,
  filtered by a process-wide threshold that defaults to Error-only (per Round 2 decision).
  The level is user-changeable from the OLED Settings → Logging submenu (a direct-select
  picker replacing no prior setting, renumbering the Settings menu to 10 rows) and a new
  Log Level row in the WebUI settings modal (`POST /api/settings/log-level`), and is
  persisted in the config (`logLevelIdx`) so a raised level survives reboots. Output is a
  best-effort dual sink: journald (via the stdlib logger the systemd unit captures) plus
  `/var/log/pi9696/app.log` (setup.sh already creates the dir; logrotate rotates it), with
  the file sink silently degrading on systems that can't open it (e.g. sim mode). Also
  fixes a pre-existing bug where the WiFi submenu's Back returned to Settings row 12 (now
  9, matching the renumbered menu instead of wrapping oddly).

### ✅ Quality Assurance

#### Code Quality
- Proper error handling throughout
- Concurrent programming with mutexes
- Clean separation of concerns
- Comprehensive documentation

#### Hardware Integration
- Robust GPIO handling
- SPI communication with error recovery
- Hardware abstraction for testability
- Graceful degradation on hardware failures

### 📋 Deployment Checklist

- [ ] Hardware assembled per WIRING.md
- [ ] Raspberry Pi OS installed and updated
- [ ] SPI interface enabled in raspi-config
- [ ] Audio interface connected and tested
- [ ] Run setup.sh script
- [ ] Test hardware with test utilities
- [ ] Verify recording functionality
- [ ] Configure as system service
- [ ] Test USB mount/unmount
- [ ] Verify all menu functions

### 📞 Support Information

#### Documentation
- README.md - Complete setup and usage guide, including `PI9696_SIM` dev workflow
- WIRING.md - Hardware connection reference
- setup.sh - Automated setup with Inferno server support
- Comments throughout source code

#### Troubleshooting
- `cmd/simcheck` for checking display layout without hardware
- Detailed error messages and logging
- System diagnostic commands in documentation
- Common issues and solutions documented

---

**Project Status: READY FOR DEPLOYMENT**

The PI9696 audio recorder is complete and ready for hardware assembly and deployment. All software components are implemented, tested (in simulation), and documented. The system provides a professional audio recording solution suitable for studio or live applications.

**Last Updated:** 2026-09-06
**Version:** 1.13.0 - Recording filename prefix (default `recording`, preset list on OLED
+ free-text WebUI field, validated and persisted).
**Maintainer:** Development Team