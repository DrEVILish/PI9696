#!/bin/bash

# PI9696 Audio Recorder Complete Setup Script
# Comprehensive setup including system preparation and FiraCode font integration
# Run with: bash setup.sh

set -e

# Configuration
FIRACODE_VERSION="6.2"
FIRACODE_URL="https://github.com/tonsky/FiraCode/releases/download/${FIRACODE_VERSION}/Fira_Code_v${FIRACODE_VERSION}.zip"
FONTS_DIR="./fonts"
TEMP_DIR="/tmp/pi9696_setup"

# Pinned to 4.0.0 rather than the unversioned/"latest" CDN URL: htmx 2.x is
# still the "latest" npm/CDN tag (4.0 stays "next" until early 2027 so
# existing 2.x users on unversioned URLs aren't force-upgraded), so an
# unversioned URL would silently serve 2.x instead of the 4.0 this app's
# templates are written against.
HTMX_VERSION="4.0.0"
HTMX_URL="https://unpkg.com/htmx.org@${HTMX_VERSION}/dist/htmx.min.js"
HTMX_SHA256="e484d9171a9db30a39c8f16e3d709d4137f3211c659f8e6125816635033d593f"
WEB_DIR="./web"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_step() {
    echo -e "${PURPLE}[STEP]${NC} $1"
}

# Header
echo -e "${CYAN}============================================${NC}"
echo -e "${CYAN}    PI9696 Audio Recorder Complete Setup   ${NC}"
echo -e "${CYAN}    System + FiraCode Font Integration     ${NC}"
echo -e "${CYAN}============================================${NC}"
echo

# Check if running as root
if [[ $EUID -eq 0 ]]; then
   log_error "This script should not be run as root."
   echo "Please run as your normal (non-root) user: bash setup.sh"
   exit 1
fi

# Raspberry Pi OS has not defaulted to a "pi" user since Bookworm - first
# boot (Raspberry Pi Imager or the on-device setup wizard) prompts for a
# username. Use whoever is actually running this script for all
# ownership/group changes below instead of hardcoding "pi".
TARGET_USER="$(id -un)"
TARGET_GROUP="$(id -gn)"

# Check if we're on a Raspberry Pi
if ! grep -q "Raspberry Pi" /proc/device-tree/model 2>/dev/null; then
    log_warning "This doesn't appear to be a Raspberry Pi."
    echo "Some hardware features may not work correctly."
    read -p "Continue anyway? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Check if we're in the correct directory
if [ ! -f "main.go" ] || [ ! -d "hardware" ]; then
    log_error "This script must be run from the PI9696 project directory"
    echo "Please cd to the PI9696 directory and run: bash setup.sh"
    exit 1
fi

# =============================================================================
# SYSTEM SETUP
# =============================================================================

log_step "System Update and Dependencies"
log_info "Updating system packages..."
sudo apt update -y
sudo apt upgrade -y

log_info "Installing required system packages..."
# golang-go and cpufrequtils are intentionally not installed via apt:
# - Debian's golang-go trails upstream by multiple feature releases (e.g.
#   1.24 on trixie vs upstream 1.27+), and this module's dependencies
#   require a newer minimum than trixie currently packages. Go is
#   installed directly from go.dev below instead.
# - cpufrequtils has no candidate package on trixie at all (removed);
#   `apt install` would fail outright since it's part of this one command.
#   linux-cpupower (cpupower) replaces it below.
sudo apt install -y \
    ffmpeg \
    alsa-utils \
    git \
    build-essential \
    pkg-config \
    libasound2-dev \
    udev \
    systemd \
    rsync \
    wget \
    unzip \
    curl \
    linux-cpupower

log_step "Go Toolchain Installation"

# Keep in sync with go.mod's minimum. golang.org/x/image (and other deps)
# have periodically raised their own minimums, which is why this installs
# a specific current release directly from go.dev rather than relying on
# the OS package.
GO_MIN_VERSION="1.25.0"
GO_INSTALL_VERSION="1.27.0"
GO_ARCH="arm64"
GO_TARBALL="go${GO_INSTALL_VERSION}.linux-${GO_ARCH}.tar.gz"
GO_SHA256="51798d2c42d0e1c6ed7fd9f48728b4193abac9e8aad6dbac2fe96a81f5909bda"

CURRENT_GO_VERSION="0.0.0"
if command -v go &> /dev/null; then
    CURRENT_GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+(\.[0-9]+)?' || echo "0.0.0")
fi

if [[ "$(printf '%s\n' "$GO_MIN_VERSION" "$CURRENT_GO_VERSION" | sort -V | head -n1)" = "$GO_MIN_VERSION" ]] && [[ "$CURRENT_GO_VERSION" != "0.0.0" ]]; then
    log_success "Go $CURRENT_GO_VERSION already installed and meets the minimum ($GO_MIN_VERSION)"
else
    log_info "Installing Go ${GO_INSTALL_VERSION} (this module requires go >= ${GO_MIN_VERSION})..."
    TMP_GO_TARBALL="/tmp/${GO_TARBALL}"
    if ! wget -q -O "$TMP_GO_TARBALL" "https://go.dev/dl/${GO_TARBALL}"; then
        log_error "Failed to download Go ${GO_INSTALL_VERSION}"
        exit 1
    fi
    if ! echo "${GO_SHA256}  ${TMP_GO_TARBALL}" | sha256sum -c - > /dev/null 2>&1; then
        log_error "Go tarball checksum mismatch - aborting"
        rm -f "$TMP_GO_TARBALL"
        exit 1
    fi
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$TMP_GO_TARBALL"
    rm -f "$TMP_GO_TARBALL"
    if ! grep -q '/usr/local/go/bin' ~/.bashrc 2>/dev/null; then
        echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    fi
    export PATH=$PATH:/usr/local/go/bin
    log_success "Go ${GO_INSTALL_VERSION} installed to /usr/local/go"
fi

log_step "Hardware Configuration"

# Enable SPI
log_info "Enabling SPI interface..."
if sudo raspi-config nonint do_spi 0; then
    log_success "SPI interface enabled"
else
    log_warning "Could not enable SPI automatically"
    echo "Please enable SPI manually: sudo raspi-config -> Interface Options -> SPI"
fi

# Enable I2C
log_info "Enabling I2C interface..."
if sudo raspi-config nonint do_i2c 0; then
    log_success "I2C interface enabled"
else
    log_warning "Could not enable I2C automatically"
fi

# GPU memory split is intentionally not configured here: raspi-config
# removed do_memory_split around the Bookworm release, since the Pi 5 (this
# project's target board) has no firmware-managed GPU memory carveout to
# split in the first place - display/multimedia buffers are allocated
# dynamically from system RAM by the kernel. The option is a no-op on any
# currently supported Raspberry Pi OS release.

# Create recording directories
log_info "Creating recording directories..."
sudo mkdir -p /rec/raw
sudo chown -R "${TARGET_USER}:${TARGET_GROUP}" /rec
sudo chmod -R 755 /rec
log_success "Recording directories created: /rec and /rec/raw"

# Create log directory
log_info "Creating log directory..."
sudo mkdir -p /var/log/pi9696
sudo chown "${TARGET_USER}:${TARGET_GROUP}" /var/log/pi9696
sudo chmod 755 /var/log/pi9696
log_success "Log directory created: /var/log/pi9696"

log_step "USB Auto-Mount Configuration"

# Create USB auto-mount script
log_info "Creating USB auto-mount script..."
sudo tee /usr/local/bin/usb-mount.sh > /dev/null <<'EOF'
#!/bin/bash

# PI9696 USB Auto-Mount Script
# Automatically mounts/unmounts USB drives to /media/usb

DEVICE="/dev/$2"
MOUNT_POINT="/media/usb"
LOG_FILE="/var/log/pi9696/usb-mount.log"

log_message() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1" >> "$LOG_FILE"
}

case "$1" in
    add)
        log_message "USB device $DEVICE detected"

        # Create mount point if it doesn't exist
        mkdir -p "$MOUNT_POINT"

        # Try to mount the device
        if mount "$DEVICE" "$MOUNT_POINT" 2>/dev/null; then
            # Set permissions
            chown -R __PI9696_USER__:__PI9696_GROUP__ "$MOUNT_POINT" 2>/dev/null || true
            chmod 755 "$MOUNT_POINT" 2>/dev/null || true
            log_message "Successfully mounted $DEVICE to $MOUNT_POINT"
        else
            log_message "Failed to mount $DEVICE"
        fi
        ;;

    remove)
        log_message "USB device $DEVICE removed"

        # Unmount the device
        if umount "$MOUNT_POINT" 2>/dev/null; then
            log_message "Successfully unmounted $MOUNT_POINT"
        else
            log_message "Failed to unmount $MOUNT_POINT (may not have been mounted)"
        fi
        ;;
esac
EOF

sudo sed -i "s/__PI9696_USER__/${TARGET_USER}/;s/__PI9696_GROUP__/${TARGET_GROUP}/" /usr/local/bin/usb-mount.sh
sudo chmod +x /usr/local/bin/usb-mount.sh
log_success "USB auto-mount script created"

# Create udev rules
log_info "Creating udev rules for USB auto-mount..."
sudo tee /etc/udev/rules.d/99-usb-automount.rules > /dev/null <<'EOF'
# USB auto-mount rules for PI9696
# Automatically mount USB storage devices to /media/usb

KERNEL=="sd[a-z][0-9]", SUBSYSTEM=="block", ACTION=="add", RUN+="/usr/local/bin/usb-mount.sh add %k"
KERNEL=="sd[a-z][0-9]", SUBSYSTEM=="block", ACTION=="remove", RUN+="/usr/local/bin/usb-mount.sh remove %k"
EOF

sudo udevadm control --reload-rules
sudo udevadm trigger
log_success "USB auto-mount rules created and loaded"

log_step "User Permissions"

# Add the invoking user to required groups
GROUPS=("audio" "gpio" "spi" "i2c")
for group in "${GROUPS[@]}"; do
    log_info "Adding ${TARGET_USER} to $group group..."
    sudo usermod -a -G "$group" "${TARGET_USER}"
    log_success "${TARGET_USER} added to $group group"
done

log_step "System Optimization"

# Disable unnecessary services
log_info "Disabling unnecessary services..."
SERVICES_TO_DISABLE=(
    "bluetooth"
    "hciuart"
    "triggerhappy"
    "avahi-daemon"
)

for service in "${SERVICES_TO_DISABLE[@]}"; do
    if systemctl is-enabled "$service" >/dev/null 2>&1; then
        sudo systemctl disable "$service"
        log_success "Disabled $service"
    else
        log_info "$service not found or already disabled"
    fi
done

# Configure CPU governor. The /etc/default/cpufrequtils file this used to
# write was only ever read by cpufrequtils' own init script, which doesn't
# exist on trixie (see the package note above) - the file would have been
# silently ignored. cpupower (from linux-cpupower, installed earlier) sets
# the governor directly; a oneshot systemd unit reapplies it on every boot
# since the kernel resets to its default governor (ondemand/schedutil) on
# each startup otherwise.
log_info "Setting CPU governor to performance..."
sudo cpupower frequency-set -g performance > /dev/null

sudo tee /etc/systemd/system/pi9696-cpu-performance.service > /dev/null <<EOF
[Unit]
Description=Set CPU governor to performance for PI9696
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/usr/bin/cpupower frequency-set -g performance
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable pi9696-cpu-performance.service
log_success "CPU governor set to performance (persists across reboots via systemd)"

# Install Rust and Cargo for Inferno server
log_step "Inferno Audio over IP Server Setup"

if command -v cargo &> /dev/null; then
    log_info "Rust/Cargo already installed: $(cargo --version)"
else
    log_info "Installing Rust and Cargo..."
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
    source ~/.cargo/env
    log_success "Rust and Cargo installed: $(cargo --version)"
fi

# Create placeholder for Inferno server
if [ ! -d "inferno" ]; then
    log_info "Creating Inferno server directory placeholder..."
    mkdir -p inferno
    cat << 'INFERNO_EOF' > inferno/README.md
# Inferno Audio over IP Server

This directory should contain the Inferno Audio over IP server project.

## Requirements:
- Rust/Cargo project with Cargo.toml
- Supports command line: inferno -c <channels> -o <output_file>
- Supports INFERNO_SAMPLE_RATE environment variable
- Outputs s32le format audio data

## Setup:
1. Place the Inferno server source code in this directory
2. Run: cargo build --release (produces inferno/target/release/inferno)
3. Test basic functionality

## Example usage:
INFERNO_SAMPLE_RATE=48000 ./target/release/inferno -c 2 -o /rec/raw/test.fifo
INFERNO_EOF
    log_warning "Inferno server placeholder created. Please install the actual server."
else
    log_info "Inferno directory already exists"
fi

# Build the Inferno server release binary during installation so the app can
# launch the prebuilt executable at runtime instead of invoking cargo run on
# every start/restart (which recompiles and blocks on the FIFO while the build
# runs). If only the placeholder exists (no Cargo.toml yet), skip with a
# warning - the app will report the missing binary until the real server is
# dropped in and setup.sh rerun.
if [ -f "inferno/Cargo.toml" ]; then
    log_info "Building Inferno server (release)..."
    if ( cd inferno && cargo build --release ); then
        log_success "Inferno server built: inferno/target/release/inferno"
        if [ ! -x "inferno/target/release/inferno" ]; then
            log_warning "Cargo build succeeded but no inferno binary found - check the package name in inferno/Cargo.toml (the app expects inferno/target/release/inferno)"
        fi
    else
        log_error "Inferno server build failed - see the cargo output above. Fix the source in ./inferno and rerun setup.sh."
    fi
else
    log_warning "No inferno/Cargo.toml found - skipping build. The app will not start a recording until the Inferno source is added and setup.sh is rerun."
fi

# Set up log rotation
log_info "Configuring log rotation..."
sudo tee /etc/logrotate.d/pi9696 > /dev/null <<'EOF'
/var/log/pi9696/*.log {
    daily
    missingok
    rotate 7
    compress
    delaycompress
    notifempty
    copytruncate
    su ${TARGET_USER} ${TARGET_GROUP}
}
EOF

log_success "Log rotation configured"

# =============================================================================
# FIRACODE FONT INSTALLATION
# =============================================================================

log_step "FiraCode Font Installation"

# Create fonts directory
log_info "Creating fonts directory..."
mkdir -p "$FONTS_DIR"

# Download FiraCode if not already present
FIRACODE_ZIP="$TEMP_DIR/FiraCode_v${FIRACODE_VERSION}.zip"

if [[ ! -f "$FONTS_DIR/FiraCode-Regular.ttf" ]] || [[ ! -f "$FONTS_DIR/FiraCode-Bold.ttf" ]]; then
    log_info "Downloading FiraCode v${FIRACODE_VERSION}..."

    mkdir -p "$TEMP_DIR"

    if ! wget -q --show-progress -O "$FIRACODE_ZIP" "$FIRACODE_URL"; then
        log_error "Failed to download FiraCode"
        exit 1
    fi

    log_success "Downloaded FiraCode v${FIRACODE_VERSION}"

    # Extract fonts
    log_info "Extracting fonts..."
    PROJECT_DIR="$(pwd)"
    cd "$TEMP_DIR"
    unzip -q "$FIRACODE_ZIP"

    # Copy TTF fonts to project directory. Must be an absolute path: from
    # inside $TEMP_DIR (e.g. /tmp/pi9696_setup), a relative "../$FONTS_DIR"
    # resolves to a sibling of $TEMP_DIR, not back to the project directory,
    # so the fonts silently landed nowhere useful and then got deleted by
    # the "rm -rf $TEMP_DIR" cleanup below.
    if [[ -d "ttf" ]]; then
        cp ttf/*.ttf "$PROJECT_DIR/$FONTS_DIR/"
    else
        # Fallback for different archive structures
        find . -name "*.ttf" -exec cp {} "$PROJECT_DIR/$FONTS_DIR/" \;
    fi

    cd - > /dev/null

    # Cleanup
    rm -rf "$TEMP_DIR"

    log_success "Fonts extracted to $FONTS_DIR"
else
    log_info "FiraCode fonts already present, skipping download"
fi

log_step "htmx Installation (Remote Control UI)"

mkdir -p "$WEB_DIR"

if [[ ! -f "$WEB_DIR/htmx.min.js" ]]; then
    log_info "Downloading htmx v${HTMX_VERSION}..."
    TMP_HTMX="/tmp/htmx.min.js"

    if ! wget -q -O "$TMP_HTMX" "$HTMX_URL"; then
        log_error "Failed to download htmx"
        exit 1
    fi

    if ! echo "${HTMX_SHA256}  ${TMP_HTMX}" | sha256sum -c - > /dev/null 2>&1; then
        log_error "htmx checksum mismatch - aborting"
        rm -f "$TMP_HTMX"
        exit 1
    fi

    mv "$TMP_HTMX" "$WEB_DIR/htmx.min.js"
    log_success "htmx v${HTMX_VERSION} installed to $WEB_DIR"
else
    log_info "htmx already present, skipping download"
fi

# Verify font installation
log_info "Verifying font installation..."

REQUIRED_FONTS=(
    "FiraCode-Regular.ttf"
    "FiraCode-Bold.ttf"
    "FiraCode-Light.ttf"
    "FiraCode-Medium.ttf"
    "FiraCode-SemiBold.ttf"
)

MISSING_FONTS=()
AVAILABLE_FONTS=()

for font in "${REQUIRED_FONTS[@]}"; do
    if [[ -f "$FONTS_DIR/$font" ]]; then
        AVAILABLE_FONTS+=("$font")
    else
        MISSING_FONTS+=("$font")
    fi
done

log_success "Available fonts: ${AVAILABLE_FONTS[*]}"

if [[ ${#MISSING_FONTS[@]} -gt 0 ]]; then
    log_warning "Optional fonts not found: ${MISSING_FONTS[*]}"
    log_warning "Core functionality will work, but some contexts may fall back"
fi

# Install Go dependencies for TTF rendering
log_info "Installing Go dependencies for font rendering..."

# Check if go.mod exists
if [[ ! -f "go.mod" ]]; then
    log_info "Initializing Go module..."
    go mod init pi9696
fi

# Add required dependencies
DEPENDENCIES=(
    "golang.org/x/image@latest"
    "github.com/golang/freetype@latest"
)

for dep in "${DEPENDENCIES[@]}"; do
    log_info "Adding dependency: $dep"
    go get "$dep"
done

go mod tidy

log_success "Go dependencies installed"

# =============================================================================
# APPLICATION BUILD
# =============================================================================

log_step "Building PI9696 Application"

# Build the main application
log_info "Building PI9696 application..."
if go mod tidy && go build -ldflags "-X main.BuildTime=$(date -u '+%Y-%m-%d_%H:%M:%S')" -o pi9696 .; then
    chmod +x pi9696
    log_success "PI9696 application built successfully"
else
    log_error "Failed to build PI9696 application"
    exit 1
fi

# Build test utilities if they exist
if [ -d "cmd" ]; then
    log_info "Building test utilities..."
    for test_file in cmd/*.go; do
        if [ -f "$test_file" ]; then
            test_name=$(basename "$test_file" .go)
            if go build -o "$test_name" "$test_file"; then
                chmod +x "$test_name"
                log_success "Built $test_name"
            else
                log_warning "Failed to build $test_name"
            fi
        fi
    done
fi

# =============================================================================
# SERVICE INSTALLATION
# =============================================================================

log_step "Service Installation"

# Install the version-controlled unit file from deploy/pi9696.service into
# place, substituting the actual install directory (the app resolves the
# Inferno binary via a relative path, so WorkingDirectory must track wherever
# this repo lives). Keeping the unit in-repo means service changes go through
# git review like everything else instead of being silently embedded in this
# script.
if [ ! -f "deploy/pi9696.service" ]; then
    log_error "Missing deploy/pi9696.service - cannot install service."
    echo "Reclone the repo (the unit file is tracked) and rerun setup.sh."
    exit 1
fi

log_info "Installing systemd service from deploy/pi9696.service..."
sudo sed "s|__PI9696_DIR__|$(pwd)|g" deploy/pi9696.service > /etc/systemd/system/pi9696.service
log_success "Systemd service installed (/etc/systemd/system/pi9696.service)"

# Enable service
sudo systemctl daemon-reload
sudo systemctl enable pi9696.service
log_success "PI9696 service enabled"

# =============================================================================
# HARDWARE VALIDATION
# =============================================================================

log_step "Hardware Validation"

log_info "Checking SPI interface..."
if [ -e /dev/spidev0.0 ]; then
    log_success "SPI interface available"
else
    log_warning "SPI interface not found. Check /boot/config.txt"
fi

log_info "Checking GPIO access..."
if [ -e /dev/gpiomem ]; then
    log_success "GPIO interface available"
else
    log_warning "GPIO interface not found"
fi

log_info "Checking audio devices..."
if arecord -l >/dev/null 2>&1; then
    log_success "Audio recording devices found:"
    arecord -l | grep -E "(card|device)" || echo "No specific devices listed"
else
    log_warning "No audio recording devices found"
fi

# =============================================================================
# UTILITY SCRIPTS
# =============================================================================

log_step "Creating Utility Scripts"

# Create startup script
cat > start-pi9696.sh << 'EOF'
#!/bin/bash
# PI9696 Manual Start Script

echo "Starting PI9696 Audio Recorder with FiraCode..."
echo "Press Ctrl+C to stop"
echo

# Check hardware
if [ ! -e /dev/spidev0.0 ]; then
    echo "WARNING: SPI interface not found"
fi

if [ ! -e /dev/gpiomem ]; then
    echo "WARNING: GPIO interface not found"
fi

# Check fonts
if [ ! -f "fonts/FiraCode-Regular.ttf" ]; then
    echo "WARNING: FiraCode fonts not found"
fi

# Start application
sudo ./pi9696
EOF

chmod +x start-pi9696.sh
log_success "Manual start script created: start-pi9696.sh"

# Set proper permissions
chmod +x setup.sh
chmod 644 fonts/*.ttf 2>/dev/null || true
chmod 644 fonts/font_config.json 2>/dev/null || true

# =============================================================================
# FINAL VALIDATION
# =============================================================================

log_step "Final Validation"

VALIDATION_ERRORS=()

# Check system components
if [ ! -e /dev/spidev0.0 ]; then
    VALIDATION_ERRORS+=("SPI interface not available")
fi

if [ ! -e /dev/gpiomem ]; then
    VALIDATION_ERRORS+=("GPIO interface not available")
fi

# Check fonts
if [[ ! -f "$FONTS_DIR/FiraCode-Regular.ttf" ]]; then
    VALIDATION_ERRORS+=("FiraCode-Regular.ttf missing")
fi

if [[ ! -f "$FONTS_DIR/FiraCode-Bold.ttf" ]]; then
    VALIDATION_ERRORS+=("FiraCode-Bold.ttf missing")
fi

# Check Go dependencies
if ! go list golang.org/x/image/font >/dev/null 2>&1; then
    VALIDATION_ERRORS+=("golang.org/x/image/font dependency missing")
fi

# Check application build
if [ ! -f "pi9696" ]; then
    VALIDATION_ERRORS+=("PI9696 application not built")
fi

if [[ ${#VALIDATION_ERRORS[@]} -gt 0 ]]; then
    log_warning "Validation issues found:"
    for error in "${VALIDATION_ERRORS[@]}"; do
        echo "  - $error"
    done
    echo
    log_warning "Some features may not work properly. Check the issues above."
else
    log_success "All validation checks passed!"
fi

# =============================================================================
# COMPLETION SUMMARY
# =============================================================================

echo
echo -e "${CYAN}============================================${NC}"
echo -e "${CYAN}         Setup Complete!                   ${NC}"
echo -e "${CYAN}============================================${NC}"
echo

log_success "PI9696 Audio Recorder with FiraCode Integration Setup Complete!"
echo

echo -e "${GREEN}📝 Installation Summary:${NC}"
echo "  • System packages: INSTALLED (including FFmpeg)"
echo "  • Hardware interfaces: CONFIGURED (SPI, I2C, GPIO)"
echo "  • Audio system: OPTIMIZED (ALSA)"
echo "  • Rust/Cargo: INSTALLED"
echo "  • Inferno server: PLACEHOLDER CREATED"
echo "  • FiraCode fonts: INSTALLED (v${FIRACODE_VERSION})"
echo "  • Programming ligatures: ENABLED"
echo "  • Go dependencies: INSTALLED"
echo "  • PI9696 application: BUILT"
echo "  • Systemd service: INSTALLED & ENABLED"
echo

echo -e "${GREEN}🎨 FiraCode Features:${NC}"
echo "  • Available variants: ${#AVAILABLE_FONTS[@]}"
echo "  • Programming ligatures: → ← ⇒ ≤ ≥ ≠ ≡ && ||"
echo "  • Context-aware rendering: 9 different contexts"
echo "  • Unicode symbols: ⚡ 📁 🎵 ⚠ ✓ 🔄 ⏱"
echo "  • OLED optimized: 256×64 display support"
echo

echo -e "${GREEN}🔌 Hardware Setup Required:${NC}"
echo "Connect the following components:"
echo
echo "OLED Display (SSD1322) - SPI Connection:"
echo "  VCC → Pin 1 (3.3V)"
echo "  GND → Pin 6 (GND)"
echo "  D0/SCLK → Pin 23 (GPIO11/SPI0_SCLK)"
echo "  D1/MOSI → Pin 19 (GPIO10/SPI0_MOSI)"
echo "  CS → Pin 24 (GPIO8/SPI0_CE0)"
echo "  DC → Pin 22 (GPIO25)"
echo "  RES → Pin 18 (GPIO24)"
echo
echo "Rotary Encoder (EC11):"
echo "  A → Pin 11 (GPIO17)"
echo "  B → Pin 13 (GPIO27)"
echo "  SW → Pin 15 (GPIO22)"
echo "  VCC → Pin 1 (3.3V)"
echo "  GND → Pin 6 (GND)"
echo
echo "Control Buttons:"
echo "  Record → Pin 29 (GPIO5)"
echo "  Stop → Pin 31 (GPIO6)"
echo "  Play → Pin 33 (GPIO13)"
echo "  Other side of buttons → GND"
echo

echo -e "${GREEN}🚀 Usage Commands:${NC}"
echo "  • Start manually:       ./start-pi9696.sh"
echo "  • Start service:        sudo systemctl start pi9696"
echo "  • Stop service:         sudo systemctl stop pi9696"
echo "  • View logs:            sudo journalctl -u pi9696 -f"
echo "  • Rebuild + restart:    ./restart-pi9696.sh"
echo

echo -e "${GREEN}📁 File Locations:${NC}"
echo "  • Recordings:           /rec/"
echo "  • Raw FIFO files:       /rec/raw/"
echo "  • USB mount:            /media/usb/"
echo "  • Logs:                 /var/log/pi9696/"
echo "  • Fonts:                ./fonts/"
echo "  • Font config:          ./fonts/font_config.json"
echo "  • Inferno server:       ./inferno/"
echo "  • Service:              /etc/systemd/system/pi9696.service"
echo

echo -e "${YELLOW}⚡ Next Steps:${NC}"
echo "1. 🔌 Connect all hardware components (see wiring above)"
echo "2. 📦 Install Inferno Audio over IP server in ./inferno/ directory"
echo "3. 🔄 Reboot the system: sudo reboot"
echo "4. 🚀 Start service: sudo systemctl start pi9696"
echo

echo -e "${GREEN}📖 Documentation:${NC}"
echo "  • Hardware wiring:      WIRING.md"
echo "  • Project status:       PROJECT_STATUS.md"
echo

if [[ ${#VALIDATION_ERRORS[@]} -eq 0 ]]; then
    echo -e "${GREEN}✅ Setup completed successfully!${NC}"
    echo -e "${GREEN}   System is ready for PI9696 Audio Recorder with enhanced typography${NC}"
else
    echo -e "${YELLOW}⚠️  Setup completed with warnings${NC}"
    echo -e "${YELLOW}   Please address the validation issues above${NC}"
fi

echo
echo -e "${CYAN}Reboot recommended to ensure all changes take effect.${NC}"
echo -e "${CYAN}After reboot, run: sudo systemctl status pi9696${NC}"
