# PI9696 — Deployment on a Raspberry Pi

Install record for a Debian Trixie test unit. Everything here is verified on
hardware; where something is inferred or environment-specific it says so.

**The [Inferno README](https://github.com/teodly/inferno) is authoritative** for
building, configuring and deploying Inferno. This document does not restate it
and does not override it: it records what was actually measured on this unit,
and the two places where reality differed from the expected path. If the two
ever disagree, the upstream README wins and this file is what needs fixing.

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

`libasound2-dev` is required twice over: for Inferno's ALSA plugin target, and for
the app itself — `alsapcm/` uses cgo (`pkg-config alsa`), so any `go build ./...`
or `go test ./...` needs the headers.

---

## 2. Inferno (AES67 AoIP server)

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

There is no binary called `inferno`; the app looks for
`inferno/target/release/inferno2pipe` (override with `PI9696_INFERNO_BIN` if
you install it somewhere else).

**Never run `go test ./...` on a unit - run it on the dev server.** Older test
helpers emptied `inferno/`; the current ones build their stub in a temp dir,
and `TestSuiteDoesNotTouchInstalledInferno` fails if `inferno/` is ever
emptied. Until `0049ab5` the suite also emptied `/rec` (`TestDownloadAll`) and
cut takes into it - on 2026-09-30 that deleted every recording on the test
unit (REPORT.md). It now runs against a temp recordings tree, guarded by
`TestSuiteDoesNotTouchRealRecordings`, but it still starts ffmpeg children and
inferno clients that do not belong on a live recorder.

```bash
# ALSA virtual soundcard (needed to transmit, and for any inferno output path)
cp target/release/libasound_module_pcm_inferno.so \
   /usr/lib/aarch64-linux-gnu/alsa-lib/       # find with: find /usr/lib* -type d -name alsa-lib
```

### asoundrc

The app opens the **bare** `inferno` device and passes every per-instance
setting (NAME, SAMPLE_RATE, TX/RX_CHANNELS, PROCESS_ID, ALT_PORT) through
`INFERNO_*` environment variables (`txholder.go`, since `275c3c5`). The unit's
`/etc/asound.conf` therefore defines the device with no keys at all:

```
pcm.inferno {
	type inferno
	hint {
		show on
		description "Inferno ALSA virtual device"
	}
}
```

This replaces the earlier instruction to copy Inferno's own
`alsa_pcm_inferno/asoundrc` (the `@args.X` form). That form was verified to drop
unset variables, but the unit's own config comment records empty expansions
panicking numeric keys; the two were never reconciled. The bare form is what
the unit runs and what the two-host test used (REPORT.md), so keep it.

One behaviour worth knowing when running several instances on one IP, because
it is easy to lose an afternoon to: `inferno_aoip` merges the ALSA config
*first* and only fills gaps from the environment
(`config.entry(key).or_insert(env_value)`). A key given a **non-empty** value in
`asoundrc` therefore wins over the `INFERNO_*` env var of the same name. Leave
a key out (or empty) if you intend to set it per instance.

Defaults, all documented upstream and all confirmed here: `SAMPLE_RATE` 48000,
`RX_CHANNELS` and `TX_CHANNELS` 2, `RX_LATENCY_NS` and `TX_LATENCY_NS` 10 ms,
`CLOCK_PATH` `/tmp/ptp-usrvclock`.

### Running more than one instance

Only one instance can use the standard inferno UDP ports. Per the README, each
extra instance needs both `ALT_PORT` and `PROCESS_ID` — a distinct `DEVICE_ID`
is *not* sufficient — and instances should be separated by at least 10 ports
(`ALT_PORT` to `ALT_PORT+3` are used today). Without this the second instance
dies with `error starting really needed listener: Address already in use`.

Latency is a real constraint rather than a tuning knob: the protocol caps it at 40 ms,
and the README's method for finding your own floor is to measure worst-case
scheduling latency with `cyclictest` and derive `TX_LATENCY_NS`/`RX_LATENCY_NS`
from it.

---

## 3. Clock source (required before Inferno will transmit)

Inferno refuses to send audio without a PTP clock overlay on the usrvclock
socket (`/tmp/ptp-usrvclock`). Without it the transmitter aborts with:

```
no clock available (timeout waiting for overlay update)
```

The receiver still starts, so this presents as "the inferno path is broken"
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

### Without one (this test unit — no hardware inferno-network device on the LAN)

The README's PTPv2 route does work as advertised: with `protocol-version =
"PTPv2"` Statime becomes a working master and announces on the wire.

```
INFO statime::port: new state for port 1: Listening -> Master
TRACE statime::port::master: sending sync message
```

It still will not transmit on this LAN, because with no hardware inferno-network device present
the clock overlay is never published. Inferno connects and reports `clock
ready`, then has nothing to schedule against:

```
ERROR inferno_aoip::device_server::flows_tx] unable to get start timestamp for ring buffer output: RecvError(())
```

This is not contrary to the README so much as its stated caveat: master
operation removes the need for a hardware device to *exist*, but upstream also
states that at least one hardware device with AES67 enabled must be present on
the network for Inferno to interoperate with such devices. With zero devices there is nothing to
discipline the master clock against and the usrvclock export never becomes
valid. Tested with both `virtual-system-clock-base` values the README mentions
(`monotonic` and the shipped `monotonic_raw`) - no overlay either way.

So for a LAN with no hardware inferno-network device, build the clock stub from Inferno's own
test suite:

```bash
gcc -O2 -o /opt/pi9696/fake_usrvclock_server \
    inferno/test/dockerized_trx/fake_usrvclock_server/fake_usrvclock_server.c
```

`deploy/pi9696-clock.service` runs it, and carries the documented Statime
invocation for a real install. Replace it with Statime the moment a hardware
inferno-network device is on the network.

The stub publishes the host's own `CLOCK_MONOTONIC_RAW` (uptime) with no shift,
so **it only works for a single host**. Two hosts on stubs are days apart, and
inferno TX between them arrives as silence (packets were stamped 159,446 s
away from the receiver's clock).

### Two inferno hosts, no hardware device (interop testing)

Statime cannot be a PTPv1 master (`trying to act as master in PTPv1, not
implemented yet`), so use PTPv2 on both - inferno only reads the overlay and
does not care which PTP version produced it:

- **Second host (master):** `protocol-version = "PTPv2"`, `priority1` below the
  unit's 251, `usrvclock-export = false`, `interface` set to its NIC, plus the
  stock stub for its own inferno. A master never steers, so it never exports;
  its PTP time is its own `monotonic_raw`, which is exactly what the stub
  publishes.
- **Unit (slave):** the repo's `inferno-ptpv1.toml` with only
  `protocol-version = "PTPv2"`. It locks within ~20 s (±130 µs to a VM master).

Stop `pi9696-clock` first, and **do not start or restart `pi9696.service`**
while testing: its `Wants=pi9696-clock` brings the stub back, which re-creates
`/tmp/ptp-usrvclock` and silently takes the socket over from statime.
`systemctl mask --runtime` does not prevent it, because the unit file in
`/etc/systemd/system` takes precedence over the `/run` mask. Check with
`ss -xp | grep ptp-usrvclock` - only statime should be bound.
`test/interop/README.md` has the full procedure.

### Hardware clock caveat

The Pi 4's `eth0` has **software timestamping only** — `ethtool -T eth0` reports
`PTP Hardware Clock: none` and there is no `/dev/ptp0`. That is expected and
supported: the README lists "Raspberry Pi 4 (no hardware PTP)" as a tested host
and names software timestamping as the default that "is compatible with all
NICs". Leave `hardware-clock` at its shipped default (`auto`) rather than
forcing `none`; it falls back on its own.

The practical consequence is clock quality, not function — inferno clock quality
on a Pi 4 is software-derived, so it is worse than a PTP-capable NIC and
latency has to be set with more margin.

On the deployment target (Pi 5) `eth0` exposes a MAC-level PTP hardware clock
(`/dev/ptp0`, Cadence GEM via `macb`). Keep one clock config for both models:
leave `hardware-clock` at `auto` so statime takes the PHC where it exists and
falls back to software timestamping on a Pi 4 — never model-detect, and the
app must never assume `/dev/ptp0` exists. First Pi 5 contact must verify
`ethtool -T eth0` shows `PTP Hardware Clock: 0` and that statime actually
selects `/dev/ptp0` (early Pi 5 kernels had missing-timestamp driver bugs, so
re-verify on the pinned kernel).

---

## 4. Subscriptions are what make audio flow

Inferno does **not** auto-connect. A receiver only subscribes to a transmitter
when an inferno controller tells it to, so without this step everything starts
cleanly and no audio ever arrives.

```bash
pip3 install --break-system-packages \
    https://github.com/chris-ritsen/network-audio-controller/releases/download/v0.3.14/netaudio-0.3.14-py3-none-manylinux_2_28_aarch64.whl
pip3 install --break-system-packages ifaddr zeroconf   # wheel deps

netaudio device list
netaudio subscription add --tx tx:1@TXDEVICE --rx rx:1@RXDEVICE
```

The README lists `network-audio-controller` as supported control software, and
this is how the loopback script drives it. One divergence from Inferno's own
test scripts: those use `--rx-device-name`/`--rx-channel-name`, which no longer
exist in netaudio 0.3.14 (`No such option`). The current syntax is positional per
channel, as above.

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
mkdir -p /rec /var/log/pi9696 /etc/pi9696
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696.service \
    > /etc/systemd/system/pi9696.service
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696-clock.service \
    > /etc/systemd/system/pi9696-clock.service
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

`inferno-loopback.sh` proves audio in and out on a single host:
a tone is played into Inferno's ALSA virtual device (transmitted), and a
subscribed `inferno2pipe` receives it.

```bash
./inferno-loopback.sh 20
```

Result on this unit (script output summarised): `inferno2pipe` received the
1000 Hz tone at a mean of -27.7 dBFS and the script reported PASS - audio went
out through the inferno ALSA transmitter and back in through `inferno2pipe`.

Note the limit of this proof: the loopback runs on one host with the stub
clock, whose overlay shift is always 0, so it cannot catch a transmitter that
stamps packets with the wrong time base. See REPORT.md for the two-host test.

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

## Reporting issues upstream

Nothing is filed to `teodly/inferno` without the maintainer's explicit
consent — not even doc bugs or divergences like the `netaudio` flags above.

The observations that would have been worth reporting are recorded here instead,
so they are not lost:

| Observation | Where it is covered |
|---|---|
| `inferno2pipe/README.md` documents `./save_to_file N` and `sample_rate=`, but the v0.5.4 binary takes `-c`/`-o` and `INFERNO_SAMPLE_RATE`; it also omits the clock daemon the top-level README calls mandatory | §2, §3 |
| With no hardware inferno-network device on the LAN, Statime as PTPv2 master never publishes the usrvclock overlay, so transmit cannot start | §3 |
| `test/dockerized_trx/control_and_test.sh` uses `netaudio` flags removed in 0.3.14 | §4 |
| `alsa_pcm_inferno` RX recorded silence while `inferno2pipe` RX worked — same host, verified subscription, likely a same-IP addressing artifact rather than a plugin fault | Known limitations |
| `netaudio subscription list` reported `Unresolved` for flows that were carrying audio | §4 |

Nothing outstanding for `ftl-themes`.

---

## Known limitations on this unit

- **Playback goes out through Inferno when the holder is ready**, local ALSA
  otherwise (see README). The app holds a TX-only instance (`<name>-TX`,
  `PROCESS_ID=1`, `ALT_PORT=10300`) alongside inferno2pipe's default-port RX
  instance - the same separation inferno-loopback.sh proves. Measured with a
  second host (REPORT.md): audible at a subscriber and visible in both
  modes, but the shipped build underruns and restarts the transmitter many
  times per minute (F2), sends no media while idle and loses the start of
  each playback (F3), and dithers 24-bit output (F4). Behaviour past 2ch is
  still unmeasured.
  Port reservations on one host: inferno2pipe defaults, app TX 10300-10303,
  loopback.sh 10100-10102/10200-10202 - never run the loopback while the app
  is up (it also runs `pkill -x inferno2pipe`, which kills the app's
  receiver). `/etc/asound.conf` must stay bare (see asoundrc above): a key
  set there wins over the `INFERNO_*` env the app passes per open, and would
  pin e.g. TX to 2ch.
- **No OLED or buttons attached.** The panel SPI path is fixed and exercised
  (`/dev/spidev0.0`, 4 MB FIFO), but rendering has not been seen on glass.
- **A second ALSA-based receiver records silence** on this single host, while
  `inferno2pipe` from the same build receives correctly. Unresolved: all
  instances share 192.168.10.69, so the unicast endpoints the transmitter
  advertises cannot be resolved per receiver. Needs a second host to settle.
- **No hardware inferno-network device on the LAN**, so the clock comes from the test stub rather
  than Statime — see the clock section.
- **`statime.service` is stale**: it points at
  `/home/drevilish/statime/target/debug/statime`, which does not exist, and
  has failed since boot. The built binary is
  `/opt/pi9696/statime/target/release/statime`.
- **`/var/log/pi9696/` was missing**, so there was no `app.log`; the installed
  `pi9696.service` also predates the template (`TimeoutStopSec` 20 vs 30).
  Re-run the §5 install steps.
- **`inferno2pipe` output is discarded** by the app, so subscription and media
  errors ("not receiving media packets", reorder-buffer losses) never reach the
  journal. Debug the receive side with a standalone `inferno2pipe` instance.
- **The sample rate is not shown by netaudio** for any inferno device: inferno
  does not answer netaudio's sample-rate probe. The TX channels carry it in
  their mDNS records (`rate=`); the RX-only `PI9696` device publishes none.
- **Pi 4 has no PTP hardware clock**, so AES67 clock quality is software-only.
