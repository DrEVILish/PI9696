# PI9696 - Raspberry Pi Audio Recorder

A professional audio recording interface for Raspberry Pi 5 designed to fit in a 1U rack unit.

## Hardware Requirements

- Raspberry Pi 5
- 2.7" 256×64 OLED Display (SSD1322) via SPI
- Rotary Encoder (EC11) with push button
- 3x Momentary buttons (Record, Stop, Play)
- Audio interface (USB or HAT)

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

## Software Setup

### Prerequisites

1. Enable SPI interface:
```bash
sudo raspi-config
# Navigate to Interface Options > SPI > Enable
```

2. Install Go (if not already installed):
```bash
wget https://go.dev/dl/go1.21.0.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.21.0.linux-arm64.tar.gz
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
sudo chown -R pi:pi /rec
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

### Auto-start on boot

Create a systemd service:

```bash
sudo tee /etc/systemd/system/pi9696.service > /dev/null <<EOF
[Unit]
Description=PI9696 Audio Recorder
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/home/pi/PI9696
ExecStart=/home/pi/PI9696/pi9696
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable pi9696.service
sudo systemctl start pi9696.service
```

## Usage

### Controls

- **Record Button**: Start recording (only when idle, requires Inferno server running)
- **Stop Button**: Stop current recording (leaves Inferno server running)
- **Play Button**: Reserved for future playback functionality
- **Rotary Encoder**: Navigate menus, toggle between elapsed/remaining time
- **Encoder Push**: Enter menus, confirm selections
- **Encoder Hold (3s)**: Cancel copy operations

### Display Layout

- **Left Panel**: Status information (time, storage, settings)
- **Right Panel**: Menu system (when active)
- **Full Width**: Status display when not in menu

### Menu System

1. **Sample Rate**: 44.1kHz, 48kHz, 96kHz, 192kHz (auto-restarts Inferno server)
2. **Channel Count**: Adjust from 1 to 128 channels (auto-restarts Inferno server)
3. **Copy Files**: Transfer recordings to USB drive
4. **System Options**: System management submenu
5. **Network Info**: Display network connection details
6. **Restart Inferno**: Manually restart Inferno Audio over IP server
7. **Exit**: Return to main display

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

- **Output Format**: WAV (PCM 24-bit)
- **Internal Pipeline**: 32-bit signed little-endian via FIFO
- **Sample Rates**: 44.1kHz, 48kHz, 96kHz, 192kHz
- **Channel Support**: 1-128 channels
- **File Naming**: `recording_YYYYMMDD_HHMMSS_chN_NNkHz.wav`
- **Raw FIFO**: `/rec/raw/inferno_YYYYMMDD_HHMMSS_chN_NNkHz.raw` (temporary)

### Inferno Server Operation

- **Startup**: Automatic when eth0 networking becomes available
- **Persistence**: Runs continuously between recordings
- **Auto-Restart**: When sample rate or channel count changes
- **Status Display**: Flame icon (🔥) in status bar when running
- **Manual Control**: "Restart Inferno" option in settings menu

## Troubleshooting

### Display Issues
- Check SPI is enabled: `lsmod | grep spi`
- Verify wiring connections
- Check permissions: `ls -l /dev/spidev*`

### Audio Issues
- List audio devices: `arecord -l`
- Test recording: `arecord -D hw:0 -f S32_LE -r 48000 -c 2 test.wav`
- Check ALSA configuration: `cat /proc/asound/cards`
- Check Inferno server: Look for flame icon in status bar
- Test Inferno manually: `cd inferno && INFERNO_SAMPLE_RATE=48000 cargo run -- -c 2 -o /tmp/test.fifo`

### Network Issues
- Check eth0 interface: `ip addr show eth0`
- Monitor network status: Watch network icon in status bar
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
- `svg/`: Icon assets including inferno.svg for status bar
- `rec/`: Final recording output directory
- `rec/raw/`: Temporary FIFO files for audio pipeline

### Inferno Server Integration
- **Directory**: `./inferno/` contains the Rust/Cargo project
- **Command**: `INFERNO_SAMPLE_RATE=<rate> cargo run -- -c <channels> -o <fifo>`
- **Pipeline**: Inferno → FIFO → FFmpeg → WAV file
- **Management**: Automatic startup, restart, and monitoring

## License

This project is licensed under the MIT License - see the LICENSE file for details.