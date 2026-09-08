# PI9696 Audio Recorder — Project Status & Design Record

This file is the **design record**: what is implemented, what is deliberately not, the
product decisions behind the behaviour, and the per-feature history. For setup and usage
see `README.md`; for hardware see `WIRING.md`.

**Last updated:** 2026-09-08 · **Version:** 1.17.1 (INFERNO-LINK deck lamp live; button
lamps driven, scrollable recording-list, config export/import to USB)

---

## Current Status

**Working end-to-end** (verified by the test suite and the live sim instance):

- Recording: Inferno → FIFO → ffmpeg → 24-bit WAV in `/rec/YYYY-MM-DD/`, with start-time
  naming (`prefix_..._chN_NNkHz.wav`), tag presets, low-disk start refusal (30-min rule),
  fsync of the finished WAV, and mid-take auto-stop when under a minute of space remains
- Playback of the latest take to local ALSA, with encoder click = play/pause,
  rotate-while-paused = seek (5 s/detent), hold = exit, progress bar + elapsed/total
- OLED UI: full menu system, status bar `[ETH] [INF] [USB]`, VU/waveform/info idle-browse
  pages, brightness (0–100 %) + auto-dim (30 s → dim, 2 min → off, wake on input, skipped
  while a take or playback is running)
- WebUI: dashboard mirroring the OLED (live PNG + on-screen encoder/buttons driving the
  same handlers as hardware), settings modal, per-channel meters over a 100 ms WebSocket,
  recordings browser with per-file download and Download-ALL streaming ZIP + manifest
- Auth: 8-char token (shown on the OLED, sim prints it to stderr) → 12 h server-side
  session (distinct random ID, lazily + on-login swept), per-IP login rate limiting,
  constant-time token compare
- Files: copy to USB (per-day structure preserved), delete with confirmation, format USB
- Logging: `log/slog`, Error/Warn/Info/Debug (default Error-only), journald + app.log,
  level changeable from OLED and WebUI, persisted

**Deliberately not in the build** (product decisions): scheduling, FLAC/MP3, analog/USB
audio I/O, local monitor output, HTTPS, user accounts, mirroring/RAID, OTA (future),
language toggle (future), power-source monitoring.

## Known Gaps & Backlog

Prioritized. Items marked *(design promise)* contradict a recorded decision and should be
fixed before release; the rest are recorded as future work.

1. **Playback via Inferno/AoIP** — the recorded target. Blocked: the Inferno server
   contract is receive-only today (no input/stream command to feed a WAV back out).
   Playback currently goes to local ALSA.
2. **Sim-mode OLED input** — the token logging fixed sim authentication, but there is
   still no way to drive the OLED menus from a dev box (only the WebUI's on-screen
   encoder, which needs auth). Add only when actually needed.
3. **Recordings list at scale** — the WebUI globs and re-renders every 15 s; fine to
   hundreds of takes, degrades at thousands. The list itself is now a full-height
   scrollable panel (no pagination, per directive); the 15 s re-glob is the remaining
   ceiling. Revisit only if real use hits it.

Superseded design notes kept for the record: scheduled recording was **removed from the
product design** (Round 3) and is fully removed from the codebase; the two GPIO status
LEDs (GPIO12/16) were **removed** in the same round in favour of button backlights.

## Implementation Status by Subsystem

### Hardware Interface Layer
- **Display (SSD1322, 256×64, SPI)** — complete; FiraCode TTF rendering with named font
  contexts (statusbar/header/recording/menu/details/alert); brightness via contrast
  command 0xC1; inert no-op paths in simulator mode.
- **Rotary encoder (EC11)** — rotate / click / hold with debouncing.
- **GPIO buttons** — Record (GPIO5), Stop (GPIO6), Play (GPIO13), internal pull-ups.
- **Button lamps** — REC + PLAY backlights, driven (GPIO12 = REC lit while recording;
  GPIO16 = PLAY solid while playing, 250 ms blink while paused; STOP has no lamp per the
  Round-4 directive). Change-only writes via `LampManager`, inert in sim.
- **Hardware manager** — unified init/close over the sub-managers + network detection.

### Recording Engine
- Persistent **Inferno** server (fetched/pinned by `setup.sh` from the official repos),
  auto-started when any IP interface gains an address, auto-restarted on sample-rate or
  channel-count changes, manual restart in Settings; single serialized worker goroutine
  owns the lifecycle.
- Pipeline: Inferno → FIFO (`rec/raw/`) → ffmpeg (`s32le` → WAV PCM 24-bit) →
  `/rec/YYYY-MM-DD/prefix_...wav`. 32-bit internal capture, 24-bit on disk.
- **Metering tap**: pass-through `astats=metadata=1:reset=1,ametadata=print:file=-`
  alongside the encode (verified: identical file size with and without it); `meterReader`
  parses `lavfi.astats.*` lines into peak/RMS globals under the app mutex; peak-hold
  ballistics (hold + 20 dB/s decay) feed every display.
- **Start gating** shared by the Record button and the WebUI: only from idle, never over
  an active take, refused when less than 30 minutes of space remains (with an on-screen
  warning). Free space always measures `/rec` (the recording media), never the USB stick.
- WAV INFO chunk via ffmpeg `-metadata` (`date`, `comment`) — verified round-trip via
  `ffprobe` (WAV INFO silently drops arbitrary keys, hence the fixed field set).

### Playback
- Plays the **most recent** recording (mtime-sorted) through **local ALSA**
  (`ffmpeg -f alsa default`). Mutually exclusive with recording in both directions.
- Encoder click toggles play/pause (SIGSTOP/SIGCONT); rotate while paused scrubs 5 s per
  detent — implemented by restarting ffmpeg with `-ss` at the new offset, keeping a paused
  track paused; hold exits. Playhead is clamped to `[0, duration]`; duration derives from
  the WAV size.
- Stopping always follows SIGTERM with SIGCONT (a SIGSTOP'd process defers SIGTERM —
  without this, stop-while-paused hangs; same for the seek path).
- The playing screen shows a progress bar + `elapsed / total` with a `[PAUSED]` marker.
- **Routing playback out through Inferno/AoIP is the design target, not implemented** —
  blocked on the Inferno server exposing an input/stream command (see Known Gaps #1).

### Level Metering
- Peak/RMS per channel from the recording/monitor pipeline; WebUI bars colour
  green/yellow/red by level (−18 dBFS / −6 dBFS breakpoints, sized to the full track so
  the colour switches at the right level); the OLED stays grayscale (shading + segments).
- Configurable meter range (floor) and peak-hold decay; WebSocket push every 100 ms with
  JSON-safe (non-finite-free) dB values.

### OLED Interface
- Status bar (time, remaining, storage, `[ETH] [INF] [USB]`), split-screen menu system
  with scroll, confirmation dialogs for destructive actions, per-day Copy Files browser,
  idle-browse flow (paged VU meters → waveform → network/token page), Network Info and
  Remote Access screens (URL + formatted token), WiFi QR screen.
- Settings menu: Audio (sample rate, channels, tag, prefix), Metering (range, peak hold),
  Display (brightness, auto-dim), Logging (level), Copy Files, System Options, Network
  Info, Remote Access, Restart Inferno, WiFi, Exit.
- Low-disk warning flashes on the idle screen when a record press is refused.

### Remote Control (WebUI)
- `remote.go` runs `net/http` on `0.0.0.0` (every up interface, no eth0-only limitation —
  Round 3), started/stopped by a 5 s poll loop as interfaces come and go; graceful drain
  with force-close fallback.
- Auth: token + session cookie. Sessions are distinct random IDs (16-byte) with 12 h
  server-side expiry, swept on each login and on logout; login is rate-limited per IP;
  token comparison is constant-time. The cookie never carries the token.
- Dashboard: OLED PNG mirror (`/api/display.png`, encoded without holding the app mutex —
  pinned by a stalled-writer regression test), on-screen encoder/buttons calling the exact
  hardware handlers, settings modal (all persisted settings), status panel, config summary,
  recordings table (path-keyed download links), **Download ALL** streaming ZIP + manifest.
- Metering: `/api/meter` one-shot + `/ws/meter` push every 100 ms with a 5 s write
  deadline per frame (a vanished client can no longer pin the loop) — headroom to tighten
  the interval later without per-request HTTP overhead.
- Input endpoints map 1:1 onto the physical controls (`/api/input/encoder/{left,right,
  click,hold}`, `/api/input/button/{record,stop,play}`), so every guard (mutual exclusion,
  confirmations, low-disk refusal) applies identically.

### Files & Storage
- Recordings always land on `/rec` (the SD card) in per-day folders; USB is only a
  copy/export target. Free-space math (30-min rule, remaining readouts) measures `/rec`.
- Copy to USB recreates the per-day structure; delete-all and format-USB require
  confirmation; USB format remounts (previously it never did).
- Download keying is path-relative to `/rec` (basenames can collide across days); the
  download whitelist checks against the app's own file listing (no path traversal).

### Logging
- `log/slog` with a `slog.LevelVar` threshold: Error/Warn/Info/Debug, **default
  Error-only** (Round 2). Dual sink: stderr (journald via systemd) +
  `/var/log/pi9696/app.log` (best-effort; logrotate). Level changeable from the OLED
  Settings → Logging submenu and the WebUI settings modal; persisted (`logLevelIdx`).

### Persistence
- `/etc/pi9696/config.json` (path overridable for dev/sim), atomic temp+rename writes:
  device name, sample rate, channels, tag, prefix, meter range, peak hold, transport
  mode, log level, OLED brightness, auto-dim, WiFi settings. Applied at boot.

## Performance & Deployment

**System**: Pi 5 (RP1 GPIO needs periph v3.8.3+ — pinned newer), Raspberry Pi OS 64-bit
Trixie+, root for GPIO/SPI/ALSA/USB, port 8080 reachable if the WebUI is used, an Inferno
AES67/Dante subscription on the network for recording. Idle CPU minimal, ~50 MB RAM,
~5 W.

**Audio**: up to 192 kHz / 24-bit / 128 ch (top end pending stress testing); ~8.3 MB/min
at 48 kHz stereo; latency is AoIP-transport dependent.

**Deployment checklist**
- [ ] Hardware assembled per `WIRING.md`; SPI enabled
- [ ] `sudo bash setup.sh` (builds Inferno, installs the systemd unit)
- [ ] Recording verified end-to-end (Inferno reachable; check `[INF]` in the status bar)
- [ ] USB copy/download verified; WebUI login verified from a browser
- [ ] Log level left at Error (default) unless debugging

## Product Decisions (2026-09-04)

Product-behaviour decisions captured from the design review. Items marked *(future)* are
noted for later releases; everything else reflects the intended current behaviour.

### Boot Behaviour
- **Default on power-on: auto-monitor input.** The unit begins monitoring audio input
  (level meters) rather than sitting on a pure idle/standby screen.

### Recording Backups & Data Protection
- **Recordings stay on `/rec`**; the user copies files to USB via the OLED interface or
  downloads them via the WebUI.
- **Low-space warning:** warn when there is **less than 30 minutes** of recording space
  remaining at the current settings (implemented — `diskWarnMinutes`).

### WebUI Recordings Download
- **Single-file download** plus **download ALL** in one action (implemented as a single
  streaming ZIP with a manifest).

### WebUI Localization
- **Language toggle** for the dashboard interface *(future)*.

### Clock & Timestamps
- **24-hour (HH:MM)** time format throughout the device and WebUI.

### Level Meters
- **Color coding** near clipping: green below −18 dBFS, yellow −18…−6 dBFS, red above
  −6 dBFS — **WebUI only** (Round 2 keeps the OLED grayscale).

### Power Handling
- **Graceful shutdown prompt**: a confirmation dialog before shutdown/restart.

### Recording Length
- **No user recording time limit**; records until manually stopped or storage is
  exhausted. **Auto-stop gracefully when less than 1 minute of space remains** to avoid
  an unrecoverable/corrupt take. *(Implemented — see the fsync + mid-take auto-stop
  feature entry in the history.)*

### Power Source Monitoring
- **Not needed** — mains power assumed; no battery/UPS monitoring *(out of scope)*.

### Software Updates
- OTA/software updates are **out of scope for initial development**, part of the final
  release *(future)*.

### WebUI Visual Style (authoritative spec)
- **Dark, futuristic SciFi HUD** on a predominantly **black and deep-navy** palette.
- **Electric blue + cyan** as the primary accent colours; **white** for important
  information.
- Should feel like an advanced spacecraft computer / AI operating system / high-end
  industrial control system — **not** a cyberpunk website.
- Clean geometric layouts, dark panels, thin blue/cyan borders, subtle transparency,
  technical icons, precise information hierarchy.
- Typography: modern, highly legible; technical/monospaced for system information.
- Uncluttered, generous dark space, clear separation between navigation / data /
  controls / status.
- Blue/cyan glow used **sparingly** to highlight active controls, selections, system
  activity, and important data.
- Subtle futuristic details (fine grid patterns, small status indicators, telemetry,
  restrained holographic effects) but **no excessive neon, heavy gradients, visual
  clutter, or bright glow everywhere**.
- Overall: sophisticated, functional, precise, technologically advanced.

### Playback
- **Add seek/scrub, only during playback** (pause + forward/rewind within a recording).
  *(Implemented in 1.16.0 — encoder click = play/pause, rotate-while-paused = seek.)*
- **Target path**: playback goes out through **Inferno/AoIP**; local ALSA playback is
  retired eventually. **Not yet implemented** — the Inferno contract is receive-only
  today, so playback still goes to local ALSA (see Known Gaps #1).
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
- **Status LEDs** - **Button lamps only (REC + PLAY)**: the two GPIO status LEDs
  (GPIO12/16) are removed; illumination is behind the REC and PLAY buttons only — STOP
  has no lamp (Round-4 directive; the STOP action has a long-press, so its lamp would
  signal nothing a user waits on). Status indication otherwise lives on the OLED.
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

- **INFERNO-LINK deck lamp live (2026-09-08).** The dashboard's link lamp was a static
  soft-cyan dot; now it reflects the Inferno server state. The 100 ms meter WebSocket
  push (already streaming to the same dashboard) carries an `infernoUp` field set from
  `infernoState == InfernoRunning` — the same source the status panel uses — and
  `applyMeter` toggles the lamp's `on` class against it, so both the WebSocket and the
  `/api/meter` fallback path get live state with one field, one class, and one CSS rule.

- **Config export/import to USB (2026-09-08).** New System Options rows — Export Config
  and Import Config (between Format USB and the power actions) publish and load a
  non-secret JSON profile (`pi9696-config.json`) on the USB drive. The profile carries
  device name, sample rate/channels, tag preset, file prefix, VU range/peak-hold,
  transport mode, log level, OLED brightness, auto-dim, and the WiFi SSID/enable — but
  **never** `WifiPassword` or the access token (export blanks it; import re-applies an
  existing local password when the file carries none). Out-of-range indexes are clamped
  like boot-time load, log level/brightness are re-applied live, an Inferno restart is
  scheduled when sample rate/channel count changed, and the imported values are
  persisted. In the WebUI the same two actions live in a Config group of the settings
  modal (import forces an HX-Refresh so every settings row shows the new values; failures
  stay inline in the modal). The directory-level helpers are unit-tested for the full
  round-trip, password non-leak, and clean failure on a profile-less drive.

- **Button lamps driven (2026-09-08).** The Round-3 backlights were LED statuses, not
  transport lamps; a new `LampManager` writes change-only GPIO values on the 100 ms OLED
  tick — REC (GPIO12) lit while recording, PLAY (GPIO16) solid while playing and blinking
  at 250 ms while paused, both off otherwise. STOP has no lamp per the Round-4 narrowing.
  Sim-mode lamps are no-ops, so the full path runs headlessly.

- **WebUI recordings list is a scrollable panel (2026-09-08).** The dashboard grows to
  viewport height on desktop layouts (≥801 px); the content grid takes all free vertical
  space and only `#recordings` scrolls — no pagination, per directive — while the deck,
  meter and settings panels squeeze to their own content. Phone widths are unchanged.

- **Data-loss hardening (2026-09-07).** The two design promises for protecting takes
  against losing them are now in the code. (1) Every finished WAV is fsynced by the
  take's owning goroutine after ffmpeg exits — an `open+Sync` outside the app mutex,
  with failures logged — so a power cut right after "stop" can't leave a drained
  journal-cache entry. (2) A take running into < 1 minute of space is now auto-stopped
  (same graceful SIGTERM finalize as the Stop button) via a once-per-second check under
  the render tick, flashing the existing LOW DISK warning; the pure predicate
  `shouldAutoStopTake` (unknown/zero estimate ≡ "not low", matching `lowDisk`) is
  pinned by a test, and the recording-lifecycle test now creates the take file so the
  fsync path runs against a real file.

- **Clock + meter colors (2026-09-07).** Two Round-1 design decisions the code had
  silently skipped. (1) A 24-hour HH:MM clock leads the OLED status bar (re-rendered on
  the existing 100 ms tick); no clock existed anywhere on the device before — the design
  specified one "throughout the device and WebUI", and the WebUI's 7-segment counter
  already covers the WebUI half during takes. (2) The WebUI VU meters now switch color at
  the design's absolute thresholds (green < -18 dBFS, yellow -18…-6, red > -6) with the
  band positions computed in JS from the configured meter floor via the same `vuPct()`
  curve used for the ticks and fills (previously a fixed 58 %/100 % gradient that only
  approximated -18 dBFS at the default floor and had no -6 boundary). Verified across
  floors -40/-60/-90/-120: -30 reads green, -12 yellow, -3 red.

- **Audit cuts (2026-09-07).** Whole-tree over-engineering audit; ~800 lines removed in
  eight commits with zero feature change (full suite green after each): dead
  HardwareManager/FiraCodeManager surface (self-test utilities, JSON diagnostics, unused
  getters), the WebUI demo mode (synthetic VU, absent from every design round), the GPIO
  status LEDs (per Round 3), cmd/font-converter (abandoned bitmap-embed tool), the xlog
  package (replaced by stdlib log/slog — see the logging change above), dead NetworkDetector
  surface, and five near-identical settings-dropdown templates consolidated into one shared
  template (byte-identical output, verified by rendering before/after). Also: the Download
  ALL button got a visible label and an explanatory empty-state page instead of a 404, and
  sim mode now prints the WebUI access token to stderr (the OLED screen that shows it is
  unreachable without input in sim).

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

- `go build ./...`, `go vet ./...` and the full `go test ./...` suite are green on every
  commit (one feature/fix per commit; see the history above).
- The suite runs the app's real handlers over `httptest` (auth flows, recordings API,
  download ZIP, settings endpoints), the playback/seek lifecycle against a fake ffmpeg,
  and the Inferno worker concurrency paths against a stub server.
- Display regressions are checked visually via `cmd/simcheck` (renders every OLED screen
  to PNG) and, for the WebUI deck, by rendering the served SVG markup.
- Hardware behaviour beyond sim (SPI/GPIO timing, ALSA device naming, real AoIP) is
  verified on the unit during deployment — see the checklist above.

### 📞 Documentation Map

- `README.md` — specifications, features, system architecture, build & install, usage,
  remote control + security posture, troubleshooting, repository layout
- `WIRING.md` — pinouts, wiring, power budget, construction and bring-up testing
- `setup.sh` — automated install (system prep, fonts, Inferno build, systemd unit)
- Source comments carry the design rationale alongside the code they explain

---

**Project Status: feature-complete per the Round 3 design; deployment blocked only on
hardware bring-up. The known gaps list above is the honest remainder.**

**Version:** 1.16.0 — playback seek/scrub (encoder click = play/pause, rotate-while-paused
= seek, relative-offset position display); **1.16.1** — two Round-1 design decisions
(status-bar clock; WebUI meter colors at the design's dBFS thresholds); **1.16.2** — the
two data-loss design promises (fsync of finished takes; mid-take auto-stop at <1 min).
**1.17.0** — button lamps driven from transport state (REC + PLAY, STOP has no lamp),
WebUI recordings list as a full-height scrollable panel, config export/import to USB
(non-secret JSON profile). **1.17.1** — the INFERNO-LINK deck lamp reflects Inferno state
(100 ms meter push carries `infernoUp`).
Plus the over-engineering audit cuts (~800 lines: dead manager surface, demo mode, GPIO
status LEDs, font-converter tool, xlog → log/slog, dead network code, template
consolidation).
