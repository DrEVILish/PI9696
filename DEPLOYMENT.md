# PI9696 — Deployment on a Raspberry Pi

Install record for a Debian Trixie test unit. Everything here is verified on
hardware; where something is inferred or environment-specific it says so.

Test unit: Raspberry Pi 4 Model B rev 1.5, Debian 13 (trixie) aarch64,
kernel 6.18.50+rpt-rpi-v8, no OLED/buttons attached.

---

## Order of operations

The dependency chain is not obvious from the app's code, and two of the steps
are easy to skip and produce confusing symptoms:

1. **SPI must be enabled in firmware.** The app opens the SSD1322 over SPI at
   startup and exits if it cannot. `PI9696_SIM=1` is the only way to run
   without it.
2. **Inferno must be built** or every recording fails (`InfernoFailed`).
3. **A clock source must be running** or Inferno starts but never transmits.
4. **A subscription must exist** before any audio flows (see
   [Subscriptions](#subscriptions-are-what-make-audio-flow)).

```bash
# 1. firmware
sed -i 's/^#dtparam=spi=on/dtparam=spi=on/' /boot/firmware/config.txt && reboot
# ... then steps 2-5 below
```

---

## 1. Prerequisites

```bash
apt-get install -y build-essential pkg-config libasound2-dev libudev-dev cmake
apt-get install -y fonts-firacode            # OLED text rendering
apt-get install -y hostapd avahi-daemon       # WiFi AP + mDNS service publishing
apt-get install -y ffmpeg python3-pip         # recording pipeline, netaudio

# Go (official binary; Debian's is too old)
curl -L https://go.dev/dl/go1.26.0.linux-arm64.tar.gz | tar xz
mv go /usr/local/go1.26 && ln -sf /usr/local/go1.26/bin/go /usr/local/bin/go

# Rust (Inferno and Statime are Rust)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
source ~/.cargo/env
```

`libasound2-dev` is only needed for the ALSA plugin target; the app itself needs
no ALSA library.

---

## 2. Inferno (Dante/AES67 AoIP server)

Inferno is pinned at install time, not tracked as a submodule (it has its own
submodules, and they must be initialised too).

```bash
cd /opt/pi9696
git clone https://github.com/DrEVILish/inferno inferno
cd inferno
git checkout v0.5.4                      # 04c0efe, an ancestor of dev
git submodule update --init --recursive  # searchfire, alsa-sys-all, usrvclock-rs
cargo build --release                    # ~7 min on a Pi 4
```

### Why this tag

`origin/HEAD` is `dev`, whose `inferno2pipe` takes a *different* CLI
(`sample_rate=... save_to_file N`) and cannot be invoked the way the app
invokes it. The 2023-era `master` branch has the right CLI but is three years
stale. `v0.5.4` is the only recent tag that both keeps the
`-c <channels> -o <path>` + `INFERNO_SAMPLE_RATE`/`INFERNO_NAME` contract the
app depends on *and* carries the modern transmit support.

### What gets built

| Artifact | Use |
|---|---|
| `target/release/inferno2pipe` | the receiver the app runs — **not** named `inferno` |
| `target/release/libasound_module_pcm_inferno.so` | ALSA virtual soundcard, for transmit |

There is no binary called `inferno`; the app's `InfernoBinary` constant points
at `inferno/target/release/inferno2pipe`.

```bash
# ALSA virtual soundcard (needed to transmit, and for any Dante output path)
cp target/release/libasound_module_pcm_inferno.so \
   /usr/lib/aarch64-linux-gnu/alsa-lib/       # find with: find /usr/lib* -type d -name alsa-lib
```

### asoundrc

Keep it minimal — all settings from `INFERNO_*` env vars:

```
pcm.inferno {
    type inferno
    hint { show on; description "Inferno virtual device" }
}
```

Keeping it minimal is not superstition: `inferno_aoip` merges the ALSA config
*before* the environment and only fills gaps (`config.entry(key).or_insert(env_value)`),
so any key given a non-empty value in asoundrc silently wins over the
`INFERNO_*` env var you meant to set — which matters when you run several
instances with different `NAME`/`PROCESS_ID`/`ALT_PORT`. Leaving a key out
lets the env var through; leaving it empty is harmless (verified: unset
`@args.X` expansions are dropped, and `INFERNO_ALT_PORT` is still honoured).
`SAMPLE_RATE` defaults to 48000 if absent.

---

## 3. Clock source (required before Inferno will transmit)

Inferno refuses to send audio without a PTP clock overlay on the usrvclock
socket (`/tmp/ptp-usrvclock`). Without it the transmitter aborts with:

```
no clock available (timeout waiting for overlay update)
```

The receiver still starts, so this presents as "the Dante path is broken"
rather than "there is no clock".

### With a PTP grandmaster on the LAN (real deployment)

```bash
cd /opt/pi9696
git clone --recurse-submodules -b inferno-dev https://github.com/teodly/statime
cd statime && cargo build --release
# edit inferno-ptpv1.toml: interface = "eth0", hardware-clock = "none" on a Pi 4
sudo ./target/release/statime -c inferno-ptpv1.toml
```

Disable NTP while Inferno is in use (`systemctl stop systemd-timesyncd`):
stepping the system clock collides with PTP, and inferno's README documents
audible damage from it. With `virtual-system-clock = true` the risk is much
lower, but it is not zero.

### Without one (this test unit — no Dante hardware on the LAN)

Neither a Dante device nor a hardware clock master exists here, so statime
either sits as an unsupported PTPv1 master or, as PTPv2 master, never produces
an overlay. Use the clock stub from inferno's own test suite instead:

```bash
gcc -O2 -o /opt/pi9696/fake_usrvclock_server \
    inferno/test/dockerized_trx/fake_usrvclock_server/fake_usrvclock_server.c
```

`deploy/pi9696-clock.service` runs it, with the statime invocation documented in
the unit for a real install.

### Hardware clock caveat

The Pi 4's `eth0` has **software timestamping only** — `ethtool -T eth0` reports
`PTP Hardware Clock: none` and there is no `/dev/ptp0`, so `hardware-clock`
must be `none`. AES67/Dante clock quality on a Pi 4 is correspondingly worse
than on hardware with a PTP-capable NIC.

---

## 4. Subscriptions are what make audio flow

Inferno does **not** auto-connect. A receiver only subscribes to a transmitter
when a Dante controller tells it to, so without this step everything starts
cleanly and no audio ever arrives.

```bash
pip3 install --break-system-packages \
    https://github.com/chris-ritsen/network-audio-controller/releases/download/v0.3.14/netaudio-0.3.14-py3-none-manylinux_2_28_aarch64.whl
pip3 install --break-system-packages ifaddr zeroconf   # wheel deps

netaudio device list
netaudio subscription add --tx tx:1@TXDEVICE --rx rx:1@RXDEVICE
```

Note the CLI in v0.3.14 is `--tx tx:N@DEV --rx rx:N@DEV`. Inferno's own test
scripts use `--rx-device-name`/`--rx-channel-name` from an older release,
which no longer exists.

Two traps:

- **`netaudio subscription list` is not trustworthy here.** It reported
  `⊘ Unresolved` for flows that were demonstrably delivering audio. Assert on
  the audio, not on the list.
- **Multiple instances need `INFERNO_ALT_PORT` and `INFERNO_PROCESS_ID`.**
  Inferno binds fixed UDP ports (ARC 4440, CMC 8800, info 8700); a second
  instance dies with `Address already in use` unless the block is moved.
  Keep the tone source alive for the whole test — the ALSA virtual device only
  exists on the network while an application holds it open, so a test tone
  shorter than the test window makes the transmitter vanish mid-run.

---

## 5. Application

```bash
cd /opt/pi9696
git submodule update --init --recursive        # ftl-themes
# web assets are not in git:
#   web/htmax.min.js, web/uPlot.iife.min.js, web/uPlot.min.css
#   third_party/ftl-themes/dist/  (theme bundles + woff2 fonts)
mkdir -p fonts
ln -sf /usr/share/fonts/truetype/firacode/FiraCode-Regular.ttf fonts/
ln -sf /usr/share/fonts/truetype/firacode/FiraCode-Bold.ttf    fonts/
go build -o pi9696 .
```

### systemd

Generate the unit from the repo template rather than writing one by hand — the
template carries the mount and hardening decisions that matter:

```bash
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696.service \
    > /etc/systemd/system/pi9696.service
cp deploy/pi9696-clock.service /etc/systemd/system/
printf 'PI9696_REMOTE_PORT=80\n' > /opt/pi9696/.env   # optional; default 8080
systemctl daemon-reload
systemctl enable --now pi9696-clock pi9696
```

**Do not add a `CapabilityBoundingSet` to the unit.** See below — restricting
it to a single capability silently costs the app its FIFO buffer.

---

## Kernel limits and fixes

### `F_SETPIPE_SZ` needs `CAP_SYS_RESOURCE`

The app asks for a 4 MB FIFO buffer so a reader stall cannot back-pressure
Inferno into an audible gap (at 128ch/48kHz s32le the stream runs ~24 MB/s, so
the 64 KB default is ~2.6 ms of audio). Measured on this unit:

| Caller | 256 KB | 1 MB | 4 MB |
|---|---|---|---|
| root (with `CAP_SYS_RESOURCE`) | granted | granted | granted |
| unprivileged user | granted | granted | **`EPERM`** |
| unprivileged, `fs.pipe-max-size=4194304` | granted | granted | granted |

The request is **refused, not clamped** — there is no partial success to notice
in logs beyond a debug line.

Three distinct causes of a 64 KB FIFO, all observed during bring-up:

1. **Restricted capabilities.** A unit with
   `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` (added for port 80) drops every
   other capability, `CAP_SYS_RESOURCE` included, and the 4 MB request then
   fails with `EPERM`. The unit still starts and the WebUI still works, so this
   is easy to miss. Confirm with `grep CapEff /proc/<pid>/status`.
   **Fix:** do not set `CapabilityBoundingSet`; the app runs as root and needs
   the capability. To serve on a low port without a capability, use the
   `PI9696_REMOTE_PORT` env var — the app runs as root anyway, so the
   capability buys nothing.
2. **Ceiling too low for an unprivileged service.** `fs.pipe-max-size`
   (default 1048576) is the hard ceiling. **Fix:**
   `sysctl -w fs.pipe-max-size=4194304` (verified to work). There is also a
   per-user aggregate ceiling, `fs.pipe-user-pages-soft` (16384 pages = 64 MB);
   once a user's pipes total more than that, new pipes silently get the
   default size.
3. **Nobody holding the pipe open.** A pipe's buffer belongs to the open pipe
   inode and is freed when the last descriptor closes. Sizing the pipe from a
   descriptor that is then dropped leaves the default size. This was a real bug
   in the app (`enlargeFifo` did exactly that) and is fixed: the descriptor is
   now held for the FIFO's lifetime, which also stops a reader seeing EOF
   before the other end attaches.

Check a live FIFO with:

```bash
python3 -c "import os,fcntl; fd=os.open('/rec/raw/inferno_<pid>_ch2_48kHz.raw', os.O_RDONLY|os.O_NONBLOCK); print(fcntl.fcntl(fd,1032))"
```

### 4 MB is one pipe per recorder

At 192 kHz/128 ch the stream is ~96 MB/s, so even 4 MB is ~40 ms. The headroom
is for scheduling jitter, not for buffering a take.

---

## Verifying audio

`inferno-loopback.sh` proves audio in and out with no Dante hardware present:
a tone is played into Inferno's ALSA virtual device (transmitted), and a
subscribed `inferno2pipe` receives it.

```bash
./inferno-loopback.sh 20
```

Result on this unit:

```
inferno2pipe (Dante RX -> file): mean=-27.7dBFS  tone=1000Hz  (source 1000Hz)
OK: audio received from inferno's Dante transmit
== PASS: audio passed out of inferno (ALSA -> Dante TX) and back in (Dante RX -> inferno2pipe)
```

### End-to-end through the app

With a transmitter running and subscribed to the app's device, record a take:

```bash
netaudio subscription add --tx tx:1@TXDEV --rx rx:1@PI9696
curl -b cookies -X POST http://<pi>/api/input/button/record
sleep 20
curl -b cookies -X POST http://<pi>/api/input/button/stop
ffprobe /rec/$(date +%F)/*.wav     # 19.8s, 48kHz stereo pcm_s24le
```

Verified content of a recorded take: 1000 Hz, mean -24.1 dBFS, both channels.

**The access token is deliberately OLED-only on real hardware** — it is never
logged and never served to an unauthenticated client, so with no panel attached
there is no way to log in and drive the WebUI. Audio-path testing therefore
runs a second instance with `PI9696_SIM=1` (display only; the audio path is
identical):

```bash
PI9696_SIM=1 PI9696_REMOTE_PORT=8081 ./pi9696   # prints the token to stderr
```

---

## Known limitations on this unit

- **Playback still goes to local ALSA**, not out through Inferno. Inferno's
  transmit works (verified above), so a Dante output path is a matter of
  routing ffmpeg at the `inferno` ALSA device — but a second Dante receiver is
  needed to confirm it, and a single host cannot provide one (all instances
  share one IP, so the unicast addresses the transmitter advertises cannot be
  resolved per receiver).
- **No OLED or buttons attached.** The panel SPI path is fixed and exercised
  (`/dev/spidev0.0`, 4 MB FIFO), but rendering has not been seen on glass.
- **Statime cannot be a clock master here** — see the stub above.
- **Pi 4 has no PTP hardware clock**, so AES67 clock quality is software-only.
