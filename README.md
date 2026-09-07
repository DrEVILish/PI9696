# PI9696 - Raspberry Pi Audio Recorder

A professional audio recording interface for Raspberry Pi 5 designed to fit in a 1U rack unit.

## Hardware Requirements

- Raspberry Pi 5 (dual-band WiFi radio used for the optional AP)
- 2.7" 256×64 OLED Display (SSD1322) via SPI
- Rotary Encoder (EC11) with push button
- 3x Momentary buttons (Record, Stop, Play) with backlight lamps
- Audio sources are Inferno AoIP (AES67/Dante) over Ethernet - no analog/USB audio I/O
  in the build (see "Audio & Playback").

## Software Requirements

- Raspberry Pi OS (64-bit), Trixie release or newer
- Go 1.25 or newer (setup.sh installs this directly from go.dev; Trixie's
  `golang-go` apt package currently trails behind this module's minimum)

## Wiring

### OLED Display (SPI)
- VCC → 3.3V
- GND → GND
- D0/SCLK → GPIO11 (SPI0 SCLK)
- D1/MOSI → GPIO10 (SPI0 MOSI)
- CS → GPIO8 (SPI0 CE0)
- DC → GPIO25
- RES → GPIO24

### Rotary Encoder
- A → GPIO17
- B → GPIO27
- SW (Push) → GPIO22
- VCC → 3.3V
- GND → GND

### Buttons
- Record → GPIO5
- Stop → GPIO6
- Play → GPIO13
- All buttons use internal pull-ups

### Status LEDs
- REC / STOP / PLAY buttons each have a **backlight lamp**; there are no separate status LEDs
- Status indication lives on the OLED (USB, Network, WiFi if enabled, and Inferno)
- See WIRING.md for wiring details

## Software Setup

### Prerequisites

1. Enable SPI interface:
```bash
sudo raspi-config
# Navigate to Interface Options > SPI > Enable
```

2. Install Go (if not already installed - this module requires Go 1.25+,
   newer than Trixie's `golang-go` apt package):
```bash
wget https://go.dev/dl/go1.27.0.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.27.0.linux-arm64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

3. Install Inferno Audio over IP server and dependencies:
```bash
sudo apt update
sudo apt install alsa-utils ffmpeg
# Run the setup script to configure Inferno server integration
./setup.sh
```

4. Create recording directories:
```bash
sudo mkdir -p /rec/raw
sudo chown -R "$(id -un):$(id -gn)" /rec
```

### Building

1. Clone or copy the project files to your Pi
2. Build the application:
```bash
cd PI9696
go mod tidy
go build -o pi9696 .
```

### Running

```bash
sudo ./pi9696
```

Note: Requires sudo for GPIO access.

### Developing without a Raspberry Pi

On a dev machine with no SPI/GPIO (e.g. x86), set `PI9696_SIM=1` to skip
hardware init - the app starts up normally and every `Update()` call dumps
the current framebuffer to `/tmp/pi9696_sim_frame.png` (override with
`PI9696_SIM_OUT`) instead of writing to SPI:

```bash
PI9696_SIM=1 ./pi9696
```

To check a specific screen's layout without wiring up encoder/button input,
`cmd/simcheck` renders each screen (idle, recording, every menu, dialogs) to
`/tmp/pi9696_shots/*.png`:

```bash
go run ./cmd/simcheck
```

### Auto-start on boot

`setup.sh` installs a version-controlled systemd unit from `deploy/pi9696.service`
(the unit file lives in the repo, so service changes go through git) and enables
it. The service runs as root for GPIO/SPI/ALSA/USB-mount access, uses `WorkingDirectory`
pointing at the install directory (the app resolves the Inferno binary relative to it),
and restarts automatically (`Restart=always`).

```bash
sudo systemctl start pi9696
sudo systemctl status pi9696
```

### Rebuilding and restarting after code changes

After editing Go source, rebuild and restart the service with the bundled helper:

```bash
./restart-pi9696.sh          # rebuild (go build) + restart the service
./restart-pi9696.sh logs     # tail the service logs
./restart-pi9696.sh status   # show service status
```

Or manually:

```bash
go build -o pi9696 .
sudo systemctl restart pi9696.service
sudo journalctl -u pi9696 -f   # view logs
```

## Usage

### Controls

- **Record Button**: Start recording (only when idle, requires Inferno server running)
- **Stop Button**: Stop current recording, or stop active playback
- **Play Button**: Play back the most recent recording (only when idle, not recording)
- **Rotary Encoder**: Navigate menus, toggle between elapsed/remaining time
- **Encoder Push**: Enter menus, confirm selections
- **Encoder Hold (3s)**: Cancel copy operations

### Display Layout

- **Left Panel**: Status information (time, storage, settings)
- **Right Panel**: Menu system (when active)
- **Full Width**: Status display when not in menu

### Menu System

1. **Audio**: Submenu with Sample Rate (44.1/48/96/192kHz, auto-restarts Inferno server),
   Channel Count (1-128, auto-restarts Inferno server), Tag (attach a preset metadata
   tag to the next recording; WAV is the sole fixed output format), and Prefix (filename
   prefix, via preset list; a free-text field in the WebUI). Filenames are
   `prefix_YYYYMMDD_HHMMSS_chN_NNkHz.wav` (default prefix `recording`).
2. **Metering**: Submenu with Meter Range and Peak Hold
3. **Display**: Submenu with Brightness (continuous 0-100% adjust row) and Auto Dim
   (dim after 30s of inactivity, off after 2 min; any input wakes it). Also settable
   from the WebUI as a brightness slider + auto-dim switch.
4. **Logging**: Submenu selecting the log verbosity - **Error** (default, quietest),
   **Warn**, **Info**, or **Debug** (most verbose). Applied immediately and persisted; the
   active level is also settable from the WebUI settings modal.
5. **Copy Files**: Transfer recordings to USB drive
6. **System Options**: System management submenu
7. **Network Info**: Display network connection details
8. **Remote Access**: Shows the URL and token for the web remote control
9. **Restart Inferno**: Manually restart Inferno Audio over IP server
10. **WiFi**: Access Point submenu (enable/disable the AP, show the join QR code)
11. **Exit**: Return to main display

### System Options Submenu
1. **Delete All**: Remove all recordings with confirmation
2. **Format USB**: Format connected USB drive (FAT32)
3. **Shutdown**: Power off system with confirmation
4. **Restart**: Reboot system with confirmation
5. **Exit**: Return to settings menu

### File Copy Options

- **[All]**: Select all recordings
- **[NONE]**: Deselect all recordings
- Individual file selection with checkboxes
- **Start Copy**: Begin transfer operation

### Recording Format

- **Output Format**: WAV (PCM 24-bit) only - there is deliberately no FLAC or MP3 option
- **Channel Support**: 1-128 channels (the full range, since WAV has no practical channel ceiling)
- **Internal Pipeline**: 32-bit signed little-endian via FIFO; finalized to 24-bit PCM WAV on disk
- **Sample Rates**: 44.1kHz, 48kHz, 96kHz, 192kHz
- **File Naming**: `recording_YYYYMMDD_HHMMSS_chN_NNkHz.wav`
- **Raw FIFO**: `/rec/raw/inferno_YYYYMMDD_HHMMSS_chN_NNkHz.raw` (temporary)
- **Metadata**: Every recording is stamped with a `date` tag (recording start time); a `comment`
  tag is added when a Tag preset other than "None" is selected in Settings. Written via ffmpeg's
  `-metadata`, which maps onto the WAV container's LIST/INFO chunk (`date` and `comment` are
  verified to round-trip) - readable with `ffprobe -show_entries format_tags <file>` or any
  standard tag reader.

### Level Metering

While recording, the third line of the recording screen shows Peak and RMS levels in dB
(replacing the filename, which there's no vertical room to also show on a 64px display - it's
still visible via Copy Files). Levels come from ffmpeg's own `astats` filter running alongside
the encode (a pass-through filter - it doesn't touch the audio, just reads and reports it), so
there's no separate metering pipeline to keep in sync with the recording. The same reading is
available on the remote control dashboard, where the meter bars are color-coded green/yellow/red
by level (the OLED stays grayscale).

### Playback

Press **Play** while idle to play back the most recently created recording (WAV). Playback is
currently sent to the local ALSA output device (`ffmpeg -f alsa default`). Note this does **not**
yet match the product decision that playback should go out through **Inferno** (AES67/Dante) onto
the network — that routing is not yet implemented on the Inferno side (see PROJECT_STATUS.md).
Press **Stop** or hold the encoder to stop playback early. While a track is playing, the encoder
(or the on-screen buttons in the WebUI) doubles as the transport: **click** toggles play/pause,
**rotate while paused** scrubs the playhead (5s per detent), and **hold** exits. The playing
screen shows the position as a relative offset — a progress bar plus elapsed/total. Playback and
recording are mutually exclusive - each button is a no-op while the other is active.

### Remote Control

A web UI is served on **any IP-based interface** (eth0, or wlan0 once the WiFi AP/client is
up) on port 8080 once the interface has an IP - started/stopped automatically as the interface
comes up or down. Settings → Remote Access shows the URL and an 8-character access token
(shown, and enterable at `/login`, as two groups of 4 - e.g. `K7M2 QX9F` - for readability;
the separator is optional when typing it in). A correct token is exchanged for a session cookie
good for 12 hours (the session ID is distinct from the token and expires server-side).

The dashboard mirrors the physical device: the OLED display itself (a live PNG snapshot of the
actual framebuffer, not a redrawn approximation) plus the rotary encoder and all three buttons
at the top, so you can navigate the exact same menu system remotely - Sample Rate/Channel
Count/Tag (the WAV-only recording format is fixed) settings, System Options, everything - the
same way you would standing in front of it. Status,
a read-only config summary, and the recordings file browser (download only - no upload, no
delete-over-network, no arbitrary file access; downloads are checked against the app's own
current recording list, not just sanitized user input). A **Download ALL** control streams every
finished recording as a single ZIP (with a `manifest.txt` listing each file) directly to the
browser, streaming rather than buffering, so large multi-channel sets don't exhaust RAM.

**Security posture, read before exposing this on a shared network:**
- The server is **plain HTTP, not HTTPS** - no certificate management on an embedded device with
  no stable hostname. Anyone who can sniff the unit's LAN traffic (eth0 or WiFi) can see the
  token and hijack the session. Treat this the way you'd treat an unencrypted admin page on any
  other LAN appliance: fine on a trusted/isolated recording-room network, not fine on
  shared/untrusted networks.
- The token is short (8 characters, ~40 bits of entropy) so it fits on the OLED and is typeable
  from a phone; a login rate limiter (5 failed attempts locks out that IP for 60s) is what keeps
  it from being brute-forceable over the network, not the token's raw length.
- The token is regenerated every time the app starts and is never written to disk - if you need
  it, read it off the device's own display.
- **Because the remote encoder/button controls drive the real menu system**, anyone with the
  token has the same reach as someone standing at the device: Delete All, Format USB, Shutdown,
  and Restart are all reachable remotely, the same as physically. Every one of them still needs
  its confirmation dialog navigated and confirmed - there's no one-click destructive action - but
  token possession is now equivalent to physical presence at the front panel, not just
  "can start/stop a recording." Weigh that against the plain-HTTP caveat above.

### Scheduled Recording

**Removed from the product design.** The Schedule menu item and scheduling loop are being
taken out of the codebase; recording is start/stop only.

### Inferno Server Operation

- **Startup**: Automatic when eth0 networking becomes available
- **Persistence**: Runs continuously between recordings
- **Auto-Restart**: When sample rate or channel count changes
- **Status Display**: `[INF]` indicator in status bar when running
- **Manual Control**: "Restart Inferno" option in settings menu

## Troubleshooting

### Display Issues
- Check SPI is enabled: `lsmod | grep spi`
- Verify wiring connections
- Check permissions: `ls -l /dev/spidev*`

### Audio Issues
- Check Inferno server: Look for `[INF]` in the status bar
- Verify AoIP network connectivity (Inferno subscribes/publishes AES67/Dante over eth0)
- Test Inferno manually: `cd inferno && INFERNO_SAMPLE_RATE=48000 ./target/release/inferno -c 2 -o /tmp/test.fifo`

### Network Issues
- Check eth0 interface: `ip addr show eth0`
- Monitor network status: Watch for `[ETH]` in the status bar
- Check connectivity: `ping 8.8.8.8`
- Inferno server requires network: Ensure eth0 is up and configured

### GPIO Issues
- Ensure running as root/sudo
- Check GPIO permissions: `ls -l /dev/gpiomem`
- Verify pin assignments don't conflict

### USB Mount Issues
- Check USB device: `lsblk`
- Manual mount: `sudo mount /dev/sda1 /media/usb`
- Check filesystem: `sudo fsck /dev/sda1`

## Development

The project is structured as follows:
- `main.go`: Main application logic, state management, and Inferno server control
- `hardware/`: Hardware abstraction layer with display, encoder, buttons, and network detection
- `inferno/`: Inferno Audio over IP server project directory (Rust/Cargo)
- `rec/`: Final recording output directory
- `rec/raw/`: Temporary FIFO files for audio pipeline
- `remote.go`: Remote control web server (auth, dashboard, recording control, downloads)
- `web/`: `htmx.min.js`, downloaded by setup.sh (gitignored)
- `cmd/simcheck/`: Dev-only tool that renders each screen to PNG via `PI9696_SIM` for checking layout without hardware

### Inferno Server Integration
- **Directory**: `./inferno/` contains the Rust/Cargo project
- **Build**: `setup.sh` runs `cargo build --release` once during installation, producing `inferno/target/release/inferno`
- **Command**: `INFERNO_SAMPLE_RATE=<rate> ./target/release/inferno -c <channels> -o <fifo>`
- **Runtime**: the app launches the prebuilt binary directly (no `cargo run` at runtime, which would recompile and block on the FIFO during every restart)
- **Pipeline**: Inferno → FIFO → FFmpeg → WAV file
- **Management**: Automatic startup, restart, and monitoring

## License

This project is licensed under the MIT License - see the LICENSE file for details.