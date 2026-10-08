#!/usr/bin/env bash
# PI9696 installer.
#
#   curl -fsSL https://drevilish.github.io/pi9696/install.sh | bash
#
# Target: Raspberry Pi OS Lite 64-bit (Debian 13 "trixie") on a Raspberry Pi 4
# or 5. Installs every dependency, builds the recorder, the inferno ALSA plugin
# and the statime PTP clock from pinned sources, and installs and starts the
# services. Safe to re-run: a second run updates and rebuilds in place and
# keeps the unit's settings, recordings and access token.
#
# Options (environment variables):
#   PI9696_DIR=/opt/pi9696   install location
#   PI9696_REF=main          pi9696 branch, tag or commit to install
#   PI9696_CLOCK=statime     statime: follow the network's PTP leader (needed to
#                            record); stub: single-host clock, monitor and
#                            playback only (no PTP leader on the LAN)
#   PI9696_PORT=80           WebUI port (written to $PI9696_DIR/.env once)
#   PI9696_FORCE=1           skip the OS / hardware checks
#   PI9696_NO_START=1        install everything but do not (re)start services

set -euo pipefail

INSTALLER_URL="${PI9696_INSTALLER_URL:-https://drevilish.github.io/pi9696/install.sh}"

# Pinned sources. Move a pin deliberately; every one is checked out exactly.
REPO_URL="https://github.com/DrEVILish/pi9696.git"
INFERNO_URL="https://github.com/DrEVILish/inferno.git"
INFERNO_REF="5981fee"                       # fork dev
STATIME_URL="https://github.com/teodly/statime.git"
STATIME_BRANCH="inferno-dev"
STATIME_REF="244f20a"
GO_VERSION="1.27.1"
GO_SHA256_ARM64="3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec"
# Web assets served by the app (not in git), with their checksums.
WEB_ASSETS=(
  "htmax.min.js|https://unpkg.com/htmx.org@4.0.0/dist/htmax.min.js|2b90cb1844656b9f025805b479288cea2877c7f91068a845c4106ebf5d87a315"
  "uPlot.iife.min.js|https://unpkg.com/uplot@1.6.32/dist/uPlot.iife.min.js|19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f"
  "uPlot.min.css|https://unpkg.com/uplot@1.6.32/dist/uPlot.min.css|df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04"
)
APT_PACKAGES=(
  git curl ca-certificates                 # fetch sources
  build-essential pkg-config cmake         # Go cgo, Rust, inferno, statime
  libasound2-dev libudev-dev               # ALSA (app cgo + inferno plugin), udev
  alsa-utils                               # aplay/arecord for checks
  ffmpeg                                   # recording, playback, metering
  fonts-firacode                           # OLED text
  avahi-daemon avahi-utils                 # <name>.local (mDNS)
  hostapd                                  # optional Wi-Fi access point
  exfatprogs dosfstools                    # USB drive formatting
  sudo logrotate
)

DIR="${PI9696_DIR:-/opt/pi9696}"
REF="${PI9696_REF:-main}"
CLOCK="${PI9696_CLOCK:-statime}"
PORT="${PI9696_PORT:-80}"
LOG=/var/log/pi9696-install.log
REBOOT_NEEDED=0

say()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need_root() {
  [ "$(id -u)" -eq 0 ] && return
  command -v sudo >/dev/null || die "run as root (sudo is not installed)"
  # Piped into bash there is no file to re-run: fetch the installer again
  # under sudo, passing the options through.
  say "Re-running as root"
  curl -fsSL "$INSTALLER_URL" | sudo --preserve-env=PI9696_DIR,PI9696_REF,PI9696_CLOCK,PI9696_PORT,PI9696_FORCE,PI9696_NO_START,PI9696_INSTALLER_URL bash
  exit $?
}

preflight() {
  say "Checking the system"
  case "$CLOCK" in statime|stub) ;; *) die "PI9696_CLOCK must be statime or stub" ;; esac
  [[ "$PORT" =~ ^[0-9]+$ ]] || die "PI9696_PORT must be a number"
  local codename arch model
  codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
  arch=$(dpkg --print-architecture)
  model=$(tr -d '\0' </proc/device-tree/model 2>/dev/null || echo "unknown")
  info "OS: $(. /etc/os-release && echo "$PRETTY_NAME") ($arch)"
  info "Hardware: $model"
  if [ "${PI9696_FORCE:-0}" != 1 ]; then
    [ "$codename" = trixie ] || die "needs Debian 13 trixie (Raspberry Pi OS Lite), found '$codename' (PI9696_FORCE=1 to try anyway)"
    [ "$arch" = arm64 ] || die "needs the 64-bit OS (arm64), found $arch (PI9696_FORCE=1 to try anyway)"
    case "$model" in
      *"Raspberry Pi 4"*|*"Raspberry Pi 5"*|*"Compute Module 4"*|*"Compute Module 5"*) ;;  # incl. Pi 400/500
      *) die "needs a Raspberry Pi 4 or 5, found '$model' (PI9696_FORCE=1 to try anyway)" ;;
    esac
  fi
  [ "$arch" = arm64 ] || die "only arm64 is supported (the Go toolchain and ALSA paths below are arm64)"
}

install_packages() {
  say "Installing system packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q
  apt-get install -y -q --no-install-recommends "${APT_PACKAGES[@]}"
}

enable_spi() {
  say "Enabling SPI (OLED panel)"
  local cfg=/boot/firmware/config.txt
  [ -f "$cfg" ] || cfg=/boot/config.txt
  if grep -qE '^\s*dtparam=spi=on' "$cfg" 2>/dev/null; then
    info "already on in $cfg"
  elif command -v raspi-config >/dev/null; then
    raspi-config nonint do_spi 0
    REBOOT_NEEDED=1
  elif [ -f "$cfg" ]; then
    printf '\n# PI9696: SPI for the SSD1322 OLED\ndtparam=spi=on\n' >>"$cfg"
    REBOOT_NEEDED=1
  else
    warn "no firmware config.txt found: enable SPI yourself (dtparam=spi=on)"
  fi
  [ -e /dev/spidev0.0 ] || REBOOT_NEEDED=1
}

install_go() {
  if command -v go >/dev/null && go version | grep -q "go${GO_VERSION} "; then
    say "Go ${GO_VERSION} already installed"; return
  fi
  say "Installing Go ${GO_VERSION}"
  local tmp; tmp=$(mktemp -d /var/tmp/pi9696-go.XXXXXX)
  curl -fsSL -o "$tmp/go.tgz" "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz"
  echo "${GO_SHA256_ARM64}  $tmp/go.tgz" | sha256sum -c --quiet - || die "Go download failed its checksum"
  tar -xzf "$tmp/go.tgz" -C "$tmp"
  local dest="/usr/local/go${GO_VERSION%.*}"
  rm -rf "$dest"; mv "$tmp/go" "$dest"; rm -rf "$tmp"
  ln -sf "$dest/bin/go" /usr/local/bin/go
  ln -sf "$dest/bin/gofmt" /usr/local/bin/gofmt
  hash -r
  info "$(go version)"
}

install_rust() {
  export PATH="$HOME/.cargo/bin:$PATH"
  if command -v cargo >/dev/null; then
    say "Rust already installed ($(cargo --version))"; return
  fi
  say "Installing Rust (rustup, stable)"
  curl --proto '=https' --tlsv1.2 -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal
  info "$(cargo --version)"
}

# checkout <url> <dir> <ref> [branch]: clone or update a source tree to an
# exact ref, refusing to clobber local changes.
checkout() {
  local url=$1 dir=$2 ref=$3 branch=${4:-}
  if [ -d "$dir/.git" ]; then
    if [ -n "$(git -C "$dir" status --porcelain --untracked-files=no)" ]; then
      die "$dir has local changes; commit or stash them, then re-run"
    fi
    git -C "$dir" fetch -q --tags origin ${branch:+"$branch"}
  else
    git clone -q ${branch:+-b "$branch"} "$url" "$dir"
  fi
  if git -C "$dir" rev-parse -q --verify "origin/$ref" >/dev/null; then
    # a branch: follow it, but never drop local commits that are not on it
    if git -C "$dir" rev-parse -q --verify "refs/heads/$ref" >/dev/null &&
       [ -n "$(git -C "$dir" rev-list "origin/$ref..refs/heads/$ref")" ]; then
      die "$dir has local commits on $ref that are not on origin; push or move them, then re-run"
    fi
    git -C "$dir" checkout -q -B "$ref" "origin/$ref"
  else
    git -C "$dir" checkout -q "$ref"                      # a tag or commit
  fi
  git -C "$dir" submodule update -q --init --recursive
  info "$(basename "$dir") at $(git -C "$dir" rev-parse --short HEAD)"
}

fetch_sources() {
  say "Fetching sources"
  mkdir -p "$(dirname "$DIR")"
  checkout "$REPO_URL" "$DIR" "$REF"
  checkout "$INFERNO_URL" "$DIR/inferno" "$INFERNO_REF"
  checkout "$STATIME_URL" "$DIR/statime" "$STATIME_REF" "$STATIME_BRANCH"
  say "Fetching web assets"
  mkdir -p "$DIR/web"
  local entry name url sum
  for entry in "${WEB_ASSETS[@]}"; do
    IFS='|' read -r name url sum <<<"$entry"
    if ! echo "$sum  $DIR/web/$name" | sha256sum -c --quiet - 2>/dev/null; then
      curl -fsSL -o "$DIR/web/$name.tmp" "$url"
      echo "$sum  $DIR/web/$name.tmp" | sha256sum -c --quiet - || die "$name failed its checksum"
      mv "$DIR/web/$name.tmp" "$DIR/web/$name"
    fi
    info "web/$name"
  done
  mkdir -p "$DIR/fonts"
  ln -sf /usr/share/fonts/truetype/firacode/FiraCode-Regular.ttf "$DIR/fonts/"
  ln -sf /usr/share/fonts/truetype/firacode/FiraCode-Bold.ttf "$DIR/fonts/"
}

cargo_jobs() {
  # Rust builds can run a 2 GB Pi out of memory with one job per core.
  local mem_kb; mem_kb=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
  if [ "$mem_kb" -lt 3000000 ]; then echo 2; else nproc; fi
}

build() {
  local jobs; jobs=$(cargo_jobs)
  say "Building the inferno ALSA plugin (several minutes on a Pi 4)"
  (cd "$DIR/inferno" && cargo build -q --release -j "$jobs" -p alsa_pcm_inferno)
  local alsadir
  alsadir="/usr/lib/$(dpkg-architecture -qDEB_HOST_MULTIARCH)/alsa-lib"
  # Replace atomically: a running app has the old plugin mapped.
  install -D -m 0755 "$DIR/inferno/target/release/libasound_module_pcm_inferno.so" "$alsadir/.libasound_module_pcm_inferno.so.new"
  mv -f "$alsadir/.libasound_module_pcm_inferno.so.new" "$alsadir/libasound_module_pcm_inferno.so"
  info "installed $alsadir/libasound_module_pcm_inferno.so"
  gcc -O2 -o "$DIR/fake_usrvclock_server" "$DIR/inferno/test/dockerized_trx/fake_usrvclock_server/fake_usrvclock_server.c"

  say "Building statime (PTP clock)"
  (cd "$DIR/statime" && cargo build -q --release -j "$jobs")

  say "Building pi9696"
  (cd "$DIR" && go build -o pi9696.new . && mv pi9696.new pi9696)
  info "pi9696 $(git -C "$DIR" describe --always --dirty)"
}

configure() {
  say "Configuring"
  mkdir -p /rec /var/log/pi9696 /etc/pi9696 /var/lib/pi9696
  if [ -f /etc/asound.conf ] && ! cmp -s /etc/asound.conf "$DIR/deploy/asound.conf"; then
    cp /etc/asound.conf "/etc/asound.conf.pi9696-backup.$(date +%Y%m%d%H%M%S)"
    info "previous /etc/asound.conf backed up"
  fi
  install -m 0644 "$DIR/deploy/asound.conf" /etc/asound.conf
  if [ ! -f "$DIR/.env" ]; then
    printf 'PI9696_REMOTE_PORT=%s\n' "$PORT" >"$DIR/.env"
    info "WebUI port $PORT ($DIR/.env)"
  fi
  install -m 0644 "$DIR/deploy/pi9696.logrotate" /etc/logrotate.d/pi9696
  local unit
  for unit in pi9696 statime pi9696-clock; do
    sed -e "s|__PI9696_DIR__|$DIR|g" "$DIR/deploy/$unit.service" >"/etc/systemd/system/$unit.service"
  done
  # Wi-Fi access point: off until enabled in the WebUI or on the panel.
  install -D -m 0644 "$DIR/deploy/hostapd-pi9696.conf" /etc/systemd/system/hostapd.service.d/pi9696.conf
  systemctl unmask hostapd >/dev/null 2>&1 || true
  systemctl daemon-reload
  systemctl enable -q avahi-daemon
  if [ "$CLOCK" = statime ]; then
    systemctl disable -q --now pi9696-clock 2>/dev/null || true
    systemctl enable -q statime
  else
    systemctl disable -q --now statime 2>/dev/null || true
    systemctl enable -q pi9696-clock
  fi
  systemctl enable -q pi9696
  info "services: pi9696, $([ "$CLOCK" = statime ] && echo statime || echo pi9696-clock), avahi-daemon"
}

start() {
  if [ "${PI9696_NO_START:-0}" = 1 ]; then
    say "Not starting services (PI9696_NO_START=1)"; return
  fi
  if [ "$REBOOT_NEEDED" = 1 ]; then
    say "SPI was just enabled: services start after the reboot"; return
  fi
  say "Starting services"
  local clock_unit; clock_unit=$([ "$CLOCK" = statime ] && echo statime || echo pi9696-clock)
  systemctl restart "$clock_unit"
  systemctl restart pi9696
  sleep 3
  systemctl is-active -q pi9696 || warn "pi9696 did not start: journalctl -u pi9696 -b"
}

summary() {
  local host; host=$(hostname)
  local url="http://${host}.local"; [ "$PORT" = 80 ] || url="$url:$PORT"
  say "PI9696 installed in $DIR"
  info "WebUI:        $url  (the access code is on the OLED, or:"
  info "              journalctl -u pi9696 -b | grep 'access code')"
  info "Clock:        $CLOCK$([ "$CLOCK" = statime ] && echo " (follows the network's PTP leader; recording needs one)")"
  info "Recordings:   /rec"
  info "Update:       re-run this installer"
  info "Install log:  $LOG"
  if [ "$REBOOT_NEEDED" = 1 ]; then
    printf '\n\033[1;33mReboot to finish (SPI was enabled): sudo reboot\033[0m\n'
  fi
}

main() {
  need_root
  exec > >(tee -a "$LOG") 2>&1
  printf '\n--- pi9696 install %s ---\n' "$(date -Is)"
  preflight
  install_packages
  enable_spi
  install_go
  install_rust
  fetch_sources
  build
  configure
  start
  summary
}

main "$@"
