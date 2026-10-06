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
git checkout df4d79f                     # fork dev: v0.5.4 + 13 dev commits + #49 + U13/U1/U2 + #8 + #41 + TX-restart + wakeup-rate + controller interop (U20-U24, U26-U28)
git submodule update --init --recursive  # searchfire, alsa-sys-all, usrvclock-rs
cargo build --release                    # ~7 min on a Pi 4
```

### Why this commit

The fork's `dev` at `df4d79f` is `v0.5.4` (`04c0efe`), the 13 later `dev`
commits (tests, dependency bumps, configurable TX dither with the old 32-bit
default), the malformed-packet fixes (INFERNO-UPSTREAM.md U15), the
channel-list paging, bulk unsubscribe and rate-probe fixes (U13, U1, U2), the
plugin panic guard (U16), the stale-audio-after-disconnect fixes (U17) and
TX flows kept across a transmitter restart (U18), and the realtime loops
waking the application at most four times per ALSA period (U19), and the
controller interop fixes U20-U24 (product version, clock role, device
settings, 0x2204, controller renames) plus the STATE_DIR setting, and
U26-U28 (sync status, sample rate / encoding status and capability bits).
Pin a commit, not the branch, so a rebuild is reproducible; move the pin
deliberately.

### What gets built

| Artifact | Use |
|---|---|
| `target/release/libasound_module_pcm_inferno.so` | the inferno ALSA plugin: the app's one inferno instance (RX and TX) |

The app runs inferno in-process: it opens the plugin's `inferno` device as a
paired capture+playback handle (`txholder.go`), so the unit is one device on
the network with equal RX and TX channels. It no longer runs `inferno2pipe`
(still part of the inferno tree, just not used here) or a separate
`<name>-TX` device. Only the plugin has to be installed; without it the app
reports Inferno failed and plays takes through local ALSA.

**Never run `go test ./...` on a unit - run it on the dev server.** Older test
helpers emptied `inferno/`; the current ones build their stub in a temp dir,
and `TestSuiteDoesNotTouchInstalledInferno` fails if `inferno/` is ever
emptied. Until `0049ab5` the suite also emptied `/rec` (`TestDownloadAll`) and
cut takes into it - on 2026-09-30 that deleted every recording on the test
unit (REPORT.md). It now runs against a temp recordings tree, guarded by
`TestSuiteDoesNotTouchRealRecordings`, but it still starts ffmpeg children and
inferno clients that do not belong on a live recorder.

```bash
# the inferno ALSA plugin: required, it is the app's inferno instance
cp target/release/libasound_module_pcm_inferno.so \
   /usr/lib/aarch64-linux-gnu/alsa-lib/       # find with: find /usr/lib* -type d -name alsa-lib
```

### asoundrc

The app opens the **bare** `inferno` device and passes every setting (NAME,
SAMPLE_RATE, TX/RX_CHANNELS, TX_SOURCE_BIT_DEPTH) through `INFERNO_*`
environment variables (`applyUnifiedInfernoEnv` in `txholder.go`). The unit's
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

**Recording needs more than a clock: it needs a network clock.** The app
refuses to start a take until statime's observation socket
(`/run/statime/observation.sock`) reports this unit as a PTP slave within
1 ms of its master on 5 consecutive polls. The stub below never qualifies, so a
unit on the stub can monitor and play back but cannot record.

### With a PTP grandmaster on the LAN (real deployment)

```bash
cd /opt/pi9696
git clone --recurse-submodules -b inferno-dev https://github.com/teodly/statime
cd statime && cargo build --release && cd ..
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/statime.service > /etc/systemd/system/statime.service
systemctl daemon-reload
systemctl disable --now pi9696-clock      # the stub and statime conflict (one owns /tmp/ptp-usrvclock)
systemctl enable --now statime
```

`deploy/statime.service` runs the release binary with the tracked
`deploy/statime.toml` (PTPv1, `eth0`, `hardware-clock = "auto"`, usrvclock export,
observation socket) and carries `Conflicts=pi9696-clock.service`. Check the
lock on the dashboard's Status panel (Clock row) or with
`python3 -c "import socket;s=socket.socket(socket.AF_UNIX);s.connect('/run/statime/observation.sock');print(s.recv(65536))"`.
The installed unit used to point at `/home/pi/statime/target/debug/statime`
(absent) and failed at every boot; reinstall it from the template.

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

Stop `pi9696-clock` first. Before `25de538`, `pi9696.service` had
`Wants=pi9696-clock`, so any app restart brought the stub back, which
re-created `/tmp/ptp-usrvclock` and silently took the socket over from statime
(`systemctl mask --runtime` did not prevent it: the unit file in
`/etc/systemd/system` takes precedence over the `/run` mask). Units installed
from the current templates are safe. Check with
`ss -xp | grep ptp-usrvclock` - only statime should be bound. The slave config
needs the `[observability]` section from `deploy/statime.toml` for the recording
gate to see it.
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
install -m 0644 deploy/pi9696.logrotate /etc/logrotate.d/pi9696   # app.log rotation (logrotate ships with Debian)
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696.service \
    > /etc/systemd/system/pi9696.service
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/pi9696-clock.service \
    > /etc/systemd/system/pi9696-clock.service
sed -e 's|__PI9696_DIR__|/opt/pi9696|g' deploy/statime.service \
    > /etc/systemd/system/statime.service
printf 'PI9696_REMOTE_PORT=80\n' > /opt/pi9696/.env   # optional; default 8080
# optional: behind a reverse proxy, name it so login rate limiting is per client
# printf 'PI9696_TRUSTED_PROXIES=<proxy-ip>\n' >> /opt/pi9696/.env
# optional: Go pprof profiles of the running app (loopback only, no auth)
# printf 'PI9696_PPROF=127.0.0.1:6060\n' >> /opt/pi9696/.env
systemctl daemon-reload
# exactly one clock: statime (network PTP leader present - required to record)
# or the stub (no leader: monitor/playback only). They conflict.
systemctl enable --now statime pi9696      # or: pi9696-clock pi9696
```

`pi9696.service` only orders itself after the clock units; it no longer
`Wants=` the stub, so restarting the app cannot replace a running statime.

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
  otherwise (see README). The TX holder is the playback side of the app's
  one in-process inferno instance (default ports). Measured with a
  second Pi on 2026-10-04 (`test/interop/e2e_bitperfect.py`, 32 ch, shared
  PTP clock): playback is bit-perfect end to end. The earlier faults are
  fixed: pump stalls (F2), no media while idle / lost playback start (F3;
  the app now transmits silence while idle), dithered 24-bit output (F4),
  and a transmitter restart dropping every receiver's flow (fork U18).
  Port reservations on one host: the app's instance takes the inferno
  defaults, loopback.sh 10100-10102/10200-10202 - never run the loopback
  while the app is up. `/etc/asound.conf` must stay bare (see asoundrc above): a key
  set there wins over the `INFERNO_*` env the app passes per open, and would
  pin e.g. TX to 2ch.
- **No OLED or buttons attached.** The panel SPI path is fixed and exercised
  (`/dev/spidev0.0`, 4 MB FIFO), but rendering has not been seen on glass.
- **A second ALSA-based receiver records silence** on this single host, while
  `inferno2pipe` from the same build receives correctly. Unresolved: all
  instances share 192.0.2.69, so the unicast endpoints the transmitter
  advertises cannot be resolved per receiver. Needs a second host to settle.
- **Clock: the LAN's hardware interface leads.** An inferno-network USB
  interface (AVIO, 2x2) on the LAN is the PTPv1 leader. Since 2026-10-05
  `statime.service` is **enabled** with the deployed `deploy/statime.toml`
  (PTPv1 slave, usrvclock export), so the unit follows it (offset a few µs)
  and recording is allowed. The unit must follow the same leader as every
  device it exchanges audio with: on a different clock (the two-host PTPv2
  recipe while the interface leads PTPv1) received audio lands outside the
  window the capture reads, and the meters and takes are silent.
  `/var/log/pi9696/` created and `pi9696.service` reinstalled from the
  template (2026-10-01).
- **Gaps in the input** show as capture overruns, logged by the app's capture
  loop at most every 10 s (`in-process inferno: N capture overrun(s)`, Error
  level). The plugin logs to the app's stderr (the journal) directly.
- **The sample rate is not shown by netaudio** for any inferno device: inferno
  does not answer netaudio's sample-rate probe (INFERNO-UPSTREAM.md U2, patch
  verified), and netaudio would not probe 192.0.2.69 at all while `PI9696` and
  `PI9696-TX` shared it (U8; the app now runs one instance). The TX channels carry the rate in their mDNS
  records (`rate=`); the RX-only `PI9696` device publishes none.
- **Pi 4 has no PTP hardware clock**, so AES67 clock quality is software-only.
