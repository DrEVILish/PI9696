# Inferno — upstream issues and changes wanted

Things found while building and testing PI9696 that belong in inferno (the
AoIP stack, pinned at `v0.5.4` / `04c0efe` from the `DrEVILish/inferno` fork)
or its companions, statime and netaudio, rather than in this repo. Per
DEPLOYMENT.md, nothing is filed upstream without the maintainer's consent; this
file is the record until then.

Each entry gives where the problem is, how it was found, its effect on PI9696,
and the change wanted. Three of the fixes are prototyped in
[`inferno-patches/`](inferno-patches/) and were verified on the dev server.

| # | Component | Issue | PI9696 impact | Status |
|---|---|---|---|---|
| U1 | inferno ARC | Bulk unsubscribe (`0x3014`) only removes the first channel | netaudio bulk remove leaves channels subscribed | **patch verified** |
| U2 | inferno info server | Sample-rate / encoding probes (`0x0081`/`0x0083`) unanswered | rate not visible in netaudio (Q4 requirement) | **patch verified** (`device show`) |
| U3 | inferno TX | TPDF dither always applied to 16/24-bit output | playback not bit-transparent; "silence" is ±1 LSB noise | change wanted |
| U4 | inferno settings | Encoding hard-coded to 24-bit | no PCM32 (undithered) option | change wanted |
| U5 | ALSA plugin | `plugin_stop` blocking-sends under its own mutex | `snd_pcm_drop` can hang the app forever | change wanted (worked around) |
| U6 | ALSA plugin | Ring not cleared on underrun; replayed while nothing writes | stale-audio loop after playback (worked around) | change wanted |
| U7 | ALSA plugin | Buffer capped at 524,288 bytes regardless of channel count | 21 ms max buffer at 128 ch | change wanted |
| U8 | inferno (multi-instance) | Settings probes always go to port 8700 | the TX instance on ALT_PORT can never answer probes | design note |
| U9 | statime | No PTPv1 master ("not implemented yet") | two inferno hosts with no hardware leader must use PTPv2 | upstream limitation |
| U10 | inferno2pipe | Logs at debug by default; one "Lost" line per channel | noisy stderr at 128 ch (counted in-app since `b08d6da`) | change wanted |
| U11 | docs | `inferno2pipe/README.md` documents a CLI the v0.5.4 binary doesn't take | install confusion | already in DEPLOYMENT.md |
| U12 | netaudio | `device list` leaves Sample Rate blank even when the device answers | rate visible only via `device show` | investigate (netaudio) |
| **U13** | inferno ARC | Channel lists paginated **32 per page**; netaudio expects 16 | **a device with >16 channels cannot be read or subscribed by netaudio** | **patch verified** |

---

## U1 — Bulk unsubscribe removes only the first channel (REPORT F9)

**Where:** `inferno_aoip/src/device_server/arc_server.rs`, opcode `0x3014`.

**Found:** `netaudio subscription remove --rx rx:1@X --rx rx:2@X` reported
`FAILED ... fresh readback {1: None, 2: (...)}`. A packet capture shows netaudio
sends a well-formed list:

```
content 0002 00000001 00000002     u16 count, then count x u32 channel id
```

inferno reads `content[4..6]`, i.e. the low half of the *first* id only (the
single-channel example in its own comment, `0001 00000002`, works by
coincidence), and replies success. **netaudio is correct; this is inferno's bug.**

**Fix:** loop over `count` entries at offset `2 + 4*i`, bounds-check, skip ids
outside the RX range. With the patch, the same command removed both channels
("disconnect requested: local channel 1" and "… 2") and netaudio verified each.

## U2 — Sample rate / encoding probes are not answered (REPORT F8, owner Q4)

**Where:** `inferno_aoip/src/device_server/info_mcast_server.rs`.

**Found:** netaudio's device detail sends five probes to UDP 8700 (class
`0x073a`, types `0x0081` sample rate, `0x0083` encoding, `0x0085` pull-up,
`0x1006`, `0x100a`). inferno logs each as `unknown request to multicast port`
and never replies, so netaudio shows `Sample Rate` blank and
"supported_sample_rates unavailable".

**Reply format** (from netaudio's own virtual-device implementation, and
accepted by netaudio when sent): multicast to 8702, class `0x0724`, status
type `0x0080` (rate) / `0x0082` (encoding), content:

```
u16 0x0018 | u16 n_supported | u32 current | u32 0 | u32 0x00020000 | n x u32 supported
```

**Fix:** answer `0x0081` with the instance's `sample_rate` and `0x0083` with
`bits_per_sample` (supported 16/24/32). Verified: `netaudio device show` on a
patched `inferno2pipe` reports `Sample Rate 96 kHz`, `Supported Sample Rates 96 kHz`.
Still open: `0x0085` (pull-up), `0x1006`, `0x100a`; netaudio's `device list`
column (U12); and PI9696 specifically (U8).

## U13 — Channel lists paginated 32 per page (devices >16 ch unusable from netaudio)

**Where:** `arc_server.rs`, `get_receive_channels` / `get_transmit_channels` /
`get_transmit_channels_friendly_names`: `paginate_respond(..., channels.len().min(32), ...)`.
That argument is the number of entries **per response page**, not a total.

**Found:** the channel-count sweep failed at 32, 64 and 128 channels with no
audio fault involved. `netaudio subscription add` aborted with `could not read
current subscriptions from PI9696.local. before making changes:
netaudio_client_get_rx_channels_json: malformed binary response`, and
`netaudio channel list` showed **0** RX channels for a 64-channel receiver.
netaudio parses at most 16 entries per page (the protocol's page size), so any
device with more than 16 channels cannot be inspected or routed.

**Fix:** page size 16 (`.min(16)`). Verified on the dev server: with the patch,
`netaudio channel list` shows all 64 RX channels and a bulk 64-channel
subscribe from a *stock* 64-channel transmitter succeeds (`MODIFIED RX 64@ITEST-RX
<- 64@ITEST-SRC (verified)`). Included in
`inferno-patches/0001-arc-bulk-unsubscribe-page-size-capability-probes.patch`.

## U3 — TX always dithers 16/24-bit output (REPORT F4)

**Where:** `flows_tx.rs:169-170` → `samples_utils.rs:58-63`:
`sample + rand_u8 − rand_u8 + 128`, then truncate, for every 16/24-bit packet.

**Measured:** for 24-bit-exact input, 24.99% of samples arrive ±1 LSB
(theory 25%), independent of value, and digital silence arrives as ±1 LSB
noise (~−145 dBFS RMS). A receiver of an undithered transmitter is bit-exact
(proven with `inferno-patches/test-only-no-tx-dither.patch`).

**Change wanted:** dither only when actually reducing word length. The
ALSA plugin's samples are 32-bit, so a 32→24 reduction legitimately wants
dither *if the low 8 bits are non-zero*. A per-instance `TX_DITHER=off|tpdf`
setting (default `tpdf`) would serve both cases; PI9696 plays back 24-bit
WAVs and wants `off`.

## U4 — Encoding is hard-coded to 24-bit

`device_server/settings.rs:127`: `bits_per_sample: 24, // TODO make it
configurable`. The 32-bit branch of the transmitter passes no dither, so a
configurable encoding (`ENCODING=16|24|32`) is the other way to a bit-exact
path, and would let a recorder match a 32-bit source.

## U5 — `plugin_stop` can deadlock (`snd_pcm_drop` never returns)

**Where:** `alsa_pcm_inferno/src/lib.rs`, `plugin_stop`:
`commands_sender.blocking_send(Command::StopTransmitter)` while holding the
plugin's common mutex. The source already notes `// TODO blocking_send inside
mutex, risk of deadlock?`.

**Found:** PI9696 called `snd_pcm_drop` to stop TX after playback. On a take
that stopped right after starting, the call never returned (the device-server
task was not draining commands, e.g. a transmitter still waiting for its start
timestamp), and since the app held its own mutex the whole app froze.
PI9696 now never calls the stop path (it overwrites the ring with silence
instead, `b29761a`), which is why it cannot make TX truly idle after a playback.

**Change wanted:** don't block inside the mutex: `try_send`, or release the lock
and use an async/oneshot handshake with a timeout. With that fixed, PI9696
can stop TX (no packets) after every playback, which is the owner's Q1 intent.

## U6 — Ring replayed when nothing writes

When the application stops writing without an underrun being detected
(nothing calls into ALSA), the transmitter keeps sending the existing ring
forever. PI9696 measured a 42 ms loop of a take's last audio at −12 dBFS at a
subscriber after playback ended. **Change wanted:** on an underrun, or when the
read position passes the write position, transmit zeros (and clear the ring)
rather than stale samples.

## U7 — Plugin buffer cap is in bytes, not frames

`HW_BUFFER_BYTES` tops out at 524,288 bytes whatever the channel count: 2048
frames at 64 ch, 1024 frames (21 ms) at 128 ch/48 kHz. Writers at high
channel counts get a buffer smaller than inferno's own 10 ms latency budget
plus scheduling jitter. **Change wanted:** scale the cap with channels (or
express it in frames/time).

## U8 — Settings/probes are per IP, but instances are per port

netaudio sends settings and capability probes to the fixed port 8700 and keys
devices by IP. A second inferno instance on an `ALT_PORT` block (PI9696's TX
holder, `PI9696-TX` on 10300-10303) never sees them, and netaudio refuses to
probe an IP with two devices ("control address 192.0.2.69 is ambiguous across
devices"). Not strictly an inferno bug. PI9696's fix is the planned single
instance for RX+TX (README Known Limitations #1), which U2 then makes visible.

## U9 — statime: no PTPv1 master

`teodly/statime` (`inferno-dev`, `244f20a`): a PTPv1 port that wins BMCA logs
`trying to act as master in PTPv1, not implemented yet` and sends no Sync, so
two inferno hosts with no hardware leader cannot sync over PTPv1. PTPv2 works
(DEPLOYMENT.md has the recipe). Related: a master never exports the usrvclock
overlay, because only steering exports, so the master host needs the stub for its own
inferno.

## U10 — inferno2pipe logging

The default log level is debug, and a reorder-buffer loss is logged once *per
channel* ("Lost N samples … in channel id k"), so at 128 channels one event is
128 lines. A per-event summary line, and an info default, would make the
receive side readable. PI9696 counts these in-app (`inferno_log.go`).

Also seen: every new subscription logs `Lost 65536 samples … (reorder buffer
timeout)` once while the buffer fills; this is harmless but looks like a fault.

## U11 — inferno2pipe README CLI

Already recorded in DEPLOYMENT.md ("Reporting issues upstream"): the README
documents `./save_to_file N` / `sample_rate=`, while v0.5.4 takes `-c`/`-o` and
`INFERNO_SAMPLE_RATE`.

## U12 — netaudio `device list` sample-rate column

With U2 applied, `netaudio device show` shows the rate, but `device list`
logs "Probed sample rates" and still prints the column blank. The likely cause
is that the list renders before the multicast reply is folded in. This is netaudio
(chris-ritsen/network-audio-controller), not inferno; check against a
hardware device before reporting.
