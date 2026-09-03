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
  tick from `isRecording`/`infernoState`; inert (no-op `LED.Set`) in simulator mode
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
- **Remote Control** - eth0-only web UI for status/start/stop/download (see Remote Control below)
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
- **Format Support** - WAV (PCM 24-bit), FLAC (lossless 24-bit), or MP3 (320kbps CBR), selectable
  from the Settings menu; 32-bit internal capture pipeline regardless of output format. Channel
  count auto-falls-back to a supported format if it exceeds the current format's ceiling (WAV
  128ch, FLAC 8ch, MP3 2ch)
- **File Naming** - Timestamped files with sample rate, channel info, and format extension
- **Real-time Display** - Shows elapsed time, remaining time, and storage
- **Storage Management** - Automatic free space calculation and display (format-aware estimate:
  exact for WAV/MP3, an approximation for FLAC's variable bitrate)
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
- **Play Button** - Plays back the most recently created recording (any supported format) through
  the default ALSA device via ffmpeg; mutually exclusive with recording in both directions
- **Stop/Cancel** - Stop button or encoder hold stops playback early

#### Scheduled Recording
- **Arm/Disarm** - Set Hour/Minute/Duration in Settings → Schedule Recording, then arm; a 1s
  poll loop (`scheduleLoop`) fires `startRecording()` at the target time (Inferno must already
  be running) and auto-stops after Duration minutes if one was set
- **One-shot** - Firing disarms the schedule; must be re-armed for the next occurrence
- **Missed window** - If the device is busy (recording/playing/mid-menu) through the entire
  target minute, the schedule disarms itself with a log line instead of silently rolling over to
  fire a day later - `scheduleLoop` tracks whether the previous tick was inside the target minute
  to detect "the window came and went" independently of "armed after today's window already
  passed" (which legitimately means "fire tomorrow" and must not disarm)
- **Stale auto-stop deadline** - `scheduledStopAt` (the pending auto-stop time) is cleared
  whenever `!isRecording`, so a manually-stopped scheduled recording can't leave a deadline
  lying around that later stops an unrelated recording that happens to still be running when it
  arrives

#### Metadata
- **Tag Presets** - Settings → Tag cycles a fixed preset list (Show, Rehearsal, Soundcheck,
  Interview, Backup, None); no free-text entry, since the hardware has no keyboard
- **Auto Date Stamp** - Every recording is tagged with its start time regardless of Tag setting
- **Uniform Across Formats** - Written via ffmpeg's `-metadata`, verified round-tripping (via
  `ffprobe`) on WAV (INFO chunk), FLAC (Vorbis comments), and MP3 (ID3v2) alike - though not
  every possible key maps cleanly to every container (WAV's INFO chunk only supports a fixed
  field set, unlike FLAC/MP3's open-ended tag systems), which is why only `date`/`comment` are
  used here rather than a wider set of fields

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
- 3x Momentary push buttons (Record, Stop, Play)
- 2x Status LEDs (Record, Inferno server status)
- USB Audio Interface or Pi HAT

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

Status LEDs:
  Record → GPIO12, Status → GPIO16
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
- **Multi-channel** - Up to 128 channels (hardware dependent, format-limited - see Recording Format)
- **Format Flexibility** - WAV, FLAC, or MP3 output
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
- **Logging** - Structured logging with rotation

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
- Audio interface (USB recommended)
- Root access for GPIO and system service installation
- Port 8080 reachable on eth0 if the web remote control is going to be used (see the Remote
  Control section above for the plain-HTTP/no-TLS caveat before exposing this beyond a trusted
  LAN)

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
- Latency: Hardware dependent (USB audio interface) + network audio server
- Quality: Up to 24-bit/192kHz with 32-bit internal processing pipeline
- Channels: 1-128 (theoretical, hardware dependent)
- File Size: ~8.3MB/minute for stereo 48kHz/24-bit
- Network: Requires eth0 connectivity for Inferno Audio over IP server
- Pipeline: Inferno Server → FIFO → FFmpeg → WAV file

### 🔮 Future Enhancements

#### Potential Additions
- **Multiple Audio Sources** - Network audio source configuration
- **Load Balancing** - Multiple Inferno server instances (not recommended - see below)

#### Hardware Expansion
- **Additional I/O** - More buttons or controls
- **Network Connectivity** - Ethernet or WiFi integration
- **Storage Expansion** - RAID or larger storage options

#### Remote Control
- **Web UI** - `remote.go` runs a `net/http` server bound to eth0's current IP only (never
  `0.0.0.0`), auto-started/stopped by `remoteControlLoop` as eth0 comes up/down, mirroring
  `networkMonitorLoop`'s polling approach but kept off the app mutex (binding/shutting down a
  listener isn't instant)
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
  (`normalizeToken`) and exchanges it for an `HttpOnly`/`SameSite=Strict` session cookie; a
  per-IP rate limiter locks out after 5 failed attempts for 60s
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

**Last Updated:** 2026-09-01
**Version:** 1.9.4 - WebUI polish round: the reel deck now sits on the theme's panel-blue palette (gradient plates/reels/head in `--panel`-family hues instead of near-black), and the 7-segment counter is ~55% taller with a wider head console window and chunkier segments. The Network settings replaced the plain WiFi check-box with a squared HUD-style SciFi toggle (chamfered thumb with a diode that lights when the AP is ONLINE, ON/OFF-band colour and an ONLINE/OFFLINE readout), and SSID + Password now sit side-by-side in one row; the wifi form now targets `#wifiqr` so a save no longer overwrites the whole settings group (pre-existing bug). Channels switched from a 1-128 dropdown to a direct number input (bounds-checked). Peak-hold now offers 3s/5s with a 3s default, matching standard practice (ITU-R BS.1771 >=150ms, Pro Tools 3s, broadcast 3-5s)
**Maintainer:** Development Team