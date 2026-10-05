# Inferno — upstream issues and changes wanted

Things found while building and testing PI9696 that belong in inferno (the
AoIP stack, pinned at `0501a56` on the `DrEVILish/inferno` fork's `dev`)
or its companions, statime and netaudio, rather than in this repo. Per
DEPLOYMENT.md, nothing is filed upstream without the maintainer's consent; this
file is the record until then.

Each entry gives where the problem is, how it was found, its effect on PI9696,
and the change wanted. Three fixes (U1, U2, U13) were first prototyped in
[`inferno-patches/`](inferno-patches/); they are now commits on the fork's
`dev` (see "Fork commits, 2026-10-04" below), which is the source of truth.

| # | Component | Issue | PI9696 impact | Status |
|---|---|---|---|---|
| U1 | inferno ARC | Bulk unsubscribe (`0x3014`) only removes the first channel | netaudio bulk remove leaves channels subscribed | **fixed in the fork** (`96811a3`) |
| U2 | inferno info server | Sample-rate / encoding probes (`0x0081`/`0x0083`) unanswered | rate not visible in netaudio (Q4 requirement) | **fixed in the fork** (`b512c60`) |
| U3 | inferno TX | TPDF dither always applied to 16/24-bit output | playback not bit-transparent; "silence" is ±1 LSB noise | **resolved**: upstream's `TX_SOURCE_BIT_DEPTH` (in the fork since `ef39a28`); pi9696 sets it to 24 (two-Pi test bit-perfect) |
| U4 | inferno settings | Encoding hard-coded to 24-bit | no PCM32 (undithered) option | change wanted |
| U5 | ALSA plugin | `plugin_stop` blocking-sends under its own mutex | `snd_pcm_drop` can hang the app forever | change wanted (worked around) |
| U6 | ALSA plugin | Ring not cleared on underrun; replayed while nothing writes | stale-audio loop after playback (worked around) | change wanted |
| U7 | ALSA plugin | ~~Buffer capped at 524,288 bytes~~ (wrong: the cap is 65,536 frames at any channel count) | none | **withdrawn** |
| U8 | inferno (multi-instance) | Settings probes always go to port 8700 | the TX instance on ALT_PORT can never answer probes | design note |
| U9 | statime | No PTPv1 master ("not implemented yet") | two inferno hosts with no hardware leader must use PTPv2 | upstream limitation |
| U10 | inferno2pipe | Logs at debug by default; one "Lost" line per channel | noisy stderr at 128 ch (counted in-app since `b08d6da`) | change wanted |
| U11 | docs | `inferno2pipe/README.md` documents a CLI the v0.5.4 binary doesn't take | install confusion | already in DEPLOYMENT.md |
| U12 | netaudio | `device list` leaves Sample Rate blank even when the device answers | rate visible only via `device show` | investigate (netaudio) |
| **U13** | inferno ARC | RX channel list paged 32 (controllers take 16); a short last page carries zeroed padding | **a receiver with >16 channels, or a transmitter with 33–63, 65–95 … channels, cannot be read or routed by netaudio** | **fixed in the fork** (`dbd9570`) |
| U14 | inferno2pipe | A blocked FIFO write stalls the whole runtime | a slow reader takes down ARC (no replies) and media (kernel drops) | change wanted |
| U15 | inferno (all servers) | Malformed control packets and adverts panic server tasks (upstream issue teodly/inferno#49) | one packet from any LAN host stops routing, flows or TX until restart | **fixed in the fork**, branch `fix/issue-49-malformed-packets` |
| U16 | ALSA plugin | A panic in any plugin callback aborts the host process; one is reachable by querying the PCM before prepare (teodly/inferno#8) | the app hosts the plugin in-process (`PI9696_INPROC_RX`), so a plugin panic kills the recorder | **fixed in the fork** (`20d639a`, `2af5592`) |
| U17 | inferno RX | Stale audio after an RX channel is disconnected: clicks from the previous ring cycle, and sometimes one channel replaying its last 0.34 s until the stream stops (teodly/inferno#41) | old audio lands in a take after a source is unrouted | **fixed in the fork** (`d6f04ab`, `66d67fa`, `d2c936f`; test `b837e3d`) |
| U18 | inferno TX | A transmitter restart (the ALSA plugin's underrun recovery) dropped every TX flow | each underrun cut every receiver off for ~5-6 s until it re-requested | **fixed in the fork** (`382dc90`) |
| U19 | inferno RX/TX | The realtime loops woke the ALSA application on every packet | thousands of needless wakeups a second; the largest CPU cost of an idle recorder with live flows | **fixed in the fork** (`3881fff`) |
| U20 | inferno info | No Product Version announced (the field at product info 0x12C was commented out) | controllers show a blank Product Version | **fixed in the fork** (`2bf6974`; pi9696 announces its own version) |
| U21 | inferno info | Clock status sent in a header-only layout with no per-port records | controllers show no clock role; netaudio "Clock Port State: Unknown (0x0000)" | **fixed in the fork** (`2d5eb4d`) |
| U22 | inferno ARC | Device settings (0x1100) and property directory (0x1102) answered with zero bytes | controllers show no sample rate and no latency | **fixed in the fork** (`ee9eece`) |
| U23 | inferno ARC | TX flow labels query (0x2204) unanswered | "received unknown opcode1 0x2204" on every controller poll | **fixed in the fork** (`1491622`) |
| U24 | inferno ARC | No handler for set device name (0x1001) | renaming the device from a controller did nothing | **fixed in the fork** (`a67a337`; the host applies it, see pi9696 devicename.go) |
| U25 | searchfire (mDNS) | Channel services (`TX1@<name>`) advertise their own host name (`tx1@<name>.local`) instead of the device's | netaudio 0.2.x groups by host and reports "Failed to get a service by type" / device name / channel counts for phantom per-channel devices | **open**: needs a host-name setter in searchfire, a GitLab submodule outside DrEVILish (decision pending) |

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

## U13 — Channel-list paging: RX pages too large, short pages padded

**Where:** `arc_server.rs` (`get_receive_channels`, `get_transmit_channels`,
`get_transmit_channels_friendly_names`, each `paginate_respond(...,
channels.len().min(32), ...)`) and `proto_arc.rs` `serialize_items`.

**Found:** in the channel-count sweep, `netaudio subscription add` to PI9696
aborted above 16 channels (`could not read current subscriptions ...
netaudio_client_get_rx_channels_json: malformed binary response`). A first
reading blamed one page size for every list. That was wrong: stock 17-, 32-
and 64-channel *transmitters* list fine. The rules below come from feeding
inferno's captured pages, and edited copies of them, to netaudio 0.3.14's own
page parser (`netaudio.core.binding.parse_page`), and from `netaudio channel
list` against stock devices on the dev server:

| Stock device | netaudio result | Why |
|---|---|---|
| RX 1–16 | OK | one page, 1–16 entries |
| RX 17, 24, 32, 33, 64, 128 (any count above 16) | device dropped ("device not found") | RX page holds up to 32 entries; netaudio accepts at most **16** per receive page |
| TX 1–32, 64, 96, 128 | OK | TX pages of **32** are accepted; every page is full |
| TX 33, 48, 65, 127 (not a multiple of 32) | device dropped | short last page is **padded** |

*Padding:* `serialize_items` reserves `space_items` entry slots before
writing any strings, so a page with fewer entries (the last page, or one
cut by `PACKET_SIZE_SOFT_LIMIT` = 800 bytes) has zeroed slots between the
entry table and the strings. netaudio rejects that page as malformed; the
same page with the gap removed parses. For a short *receive* page, netaudio
also requires the first byte (which inferno sets to the page size) to equal
the entry count: 16/1 fails, 1/1 parses. Every single-page list already
satisfies this, which is why devices of ≤16 RX / ≤32 TX channels never
showed the bug. Whether the rule is netaudio being strict or inferno breaking
the protocol cannot be settled without a hardware capture; packing the page
satisfies both readings.

**Fix** (in `inferno-patches/0001-…`): RX page size 16 and TX 32
(`RX_CHANNELS_PAGE_SIZE`, `TX_CHANNELS_PAGE_SIZE`); a new `serialize_page`
for the channel lists that reserves only as many slots as entries remain,
moves the string area down over any unused slots (each entry type
implements `PagedEntry::shift_heap_offsets`), and writes the entry count in
both header bytes. Unit tests cover a short last page, full pages and a
soft-limit cut. The flow lists (`query_tx_flows`, `query_rx_flows`) still use
`serialize_items`. Their records contain nested offsets, so they would need
the same treatment record by record, and nothing here reads them in pages yet.

**Verified** on the dev server with the patched `inferno2pipe` and ALSA
plugin: `netaudio channel list` reads every channel of RX devices at 1, 2,
15–17, 24, 31–33, 48, 63–65, 96, 127 and 128 channels and of TX devices at
the same counts. Stock fails at every RX count above 16 and at TX 33/48/65/127. A bulk subscribe of 24 and
of 40 channels between two patched devices is `verified` per channel by
netaudio, and a bulk remove of all of them (U1) is verified too. An earlier
version of this patch (all pages 16, still padded) only worked when the count
was a multiple of 16; that is what the round-2 sweep used.

## U14 — inferno2pipe stalls completely when its FIFO reader is slow

**Found:** in the channel sweep, while the unit's metering ffmpeg could not
keep up (or, above 64 ch, could not start), `inferno2pipe`'s FIFO write
blocked. With it went everything else on its runtime: raw ARC requests got no
reply at all (128 of 128 subscribe batches timed out), and the kernel dropped
its media packets at ~12,000-14,000 `RcvbufErrors`/s. A recorder must not lose its control plane
because a consumer is slow.

**Change wanted:** write the pipe from a dedicated thread behind a bounded
queue, count and log overruns, and keep ARC/mDNS/flow reception on threads
that never block on the output. PI9696 removed the trigger (lean metering,
`65b26cd`), but any stall in the consumer (SD hiccup, CPU spike) still
propagates.

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

## U7 — withdrawn: there is no per-byte buffer cap

The original entry said `HW_BUFFER_BYTES` tops out at 524,288 bytes whatever
the channel count (1024 frames, 21 ms, at 128 ch). That is wrong. The plugin
offers power-of-two buffers of 1024 to **65,536 frames, at any channel
count** (`alsa_pcm_inferno/src/lib.rs`, `buffer_sizes` scales the byte list
by `num_channels`); 524,288 bytes is the 65,536-frame maximum at 2 channels,
misread from a 2-channel debug line. Measured with `aplay -v` on the dev
server: `--buffer-time=2000000` gives `buffer_size 65536` at both 2 and 128
channels (1.37 s at 48 kHz). Because sizes are powers of two, a request is
rounded down: 120 ms gives 4096 frames (85 ms).

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

## U15 — Malformed packets panic server tasks (teodly/inferno#49)

**Where it is fixed:** the owner's fork, `DrEVILish/inferno`: the seven
commits of `fix/issue-49-malformed-packets` were fast-forwarded into `dev`
(`9767558` -> `06993a1`) on 2026-10-03, and the test unit runs that build.
Each commit is revertible on its own. As with every inferno change, nothing
goes upstream.

The upstream issue lists four handlers. An audit of every place inferno parses
network input found more, all fixed:

| Commit | Area | Reachable panics removed |
|---|---|---|
| `3203a4f` | `deserialize_items` (ARC rename, subscribe) | count clamped against the whole payload; slice of a payload shorter than 2 bytes |
| `ce8583a` | flow control 0x0100 / 0x0102 (UDP 4455) | unchecked channel count, offsets and string reads; out-of-range channel accepted; `flows_tx` unwrapped on RX-only devices |
| `d7de045` | TX thread | packet larger than the buffer (bits, fpp, channel count), fpp 0 spin, channel index out of range; multicast activation after delete |
| `def859f` | ARC 0x3014, 0x2201/0x2202, renames | unchecked lengths and ids; descriptor offset underflow; bundle assert; a channel label over the DNS limit panicked mDNS, and again on every start once saved |
| `789d551` | receive side (mDNS adverts, flows) | `nchan=0`, odd encodings, huge latency, multicast slot out of range, more than 32 receive flows |
| `3baa1dd` | info multicast clock stats | short or non-ASCII clock-stats file |
| `06993a1` | supervision | ARC, CMC and flow-control servers restart after a panic (100 ms .. 5 s backoff) |

Parsing moved into pure functions with unit tests (every truncation, oversized
counts, offset underflow, seeded random mutations). Results: 114 unit tests
pass on the Pi and the dev server (92 on `dev`), the `loopback_trx`
integration test passes on the dev server, and netaudio channel lists and
bulk subscribe behave as on stock. Not changed: 0x3014 still removes only the
first listed channel (U1), and channel lists above 16 RX channels still need
U13; both are next.

## Fork commits, 2026-10-04 (U13, U1, U2 restored; U16)

**What went wrong:** U1, U2 and U13 were verified in September as one patch
file (`inferno-patches/0001-…`) and a scratch checkout, but never committed to
the fork. Rebuilding the unit from the fork's `dev` for the #49 fixes
(2026-10-03) therefore dropped them. From then on netaudio listed PI9696 as
TX 0 / RX 0 ("malformed binary response" on the 32-channel receive list),
could not read or remove its subscriptions in bulk, and showed no sample rate.

Now on the fork's `dev`, one commit per fix, on top of `06993a1`:

| Commit | Fix | Tests |
|---|---|---|
| `dbd9570` | U13: packed channel-list pages, receive pages of 16 | short last page, full pages, soft-limit cut, 32-entry RX list paged as 16 + 16 |
| `96811a3` | U1: 0x3014 bulk unsubscribe removes every listed channel | 3-channel bulk remove; out-of-range ids reported, the rest handled |
| `b512c60` | U2: answer sample-rate (0x0081) and encoding (0x0083) probes | exact reply bytes |
| `20d639a` | U16: plugin pointer safe before prepare (#8) | `alsa_pcm_inferno/test_status_before_prepare.sh` (aborted before, passes now) |
| `2af5592` | U16: every plugin callback wrapped in `catch_unwind`; `env_logger` `try_init` | `ffi_guard` unit tests |
| `d0521f0` | U1 follow-up: a removed subscription is cleared before the reply, so netaudio's readback confirms it | verified on the unit |

121 `inferno_aoip` unit tests and the `loopback_trx` integration test pass on
the dev server (searchfire's mDNS `client_and_server` test times out there;
it is untouched by these commits). Verified on the test unit after
deploying the plugin: `netaudio device list` shows PI9696 with TX 32 / RX 32,
`netaudio channel list` reads all 64 channels, `device show` reports 48 kHz,
PCM24 and the supported rates and encodings, and a bulk `subscription remove`
of three channels clears all three. With `2af5592` netaudio's immediate
readback still listed them and printed "FAILED" (the entry was cleared only
on the subscriber task); `d0521f0` clears it before the reply, and netaudio
now reports each removal as "verified".

## U16 — Plugin panics abort the host (teodly/inferno#8)

**Found:** reproduced on the dev server with a 20-line C client: open the
`inferno` PCM and call `snd_pcm_status` before `hw_params`/`prepare`. ALSA
calls the plugin's pointer callback, which unwrapped `stream_info` (set only
in prepare). A panic cannot unwind out of an `extern "C"` function, so the
process aborted with "panic in a function that cannot unwind", the same trace
as #8 (`snd_pcm_status` -> `plugin_pointer`). The plugin's callbacks also
unwrap lock results and channel sends to the device server, any of which
would take the host down the same way. **Fixed** by `20d639a` and `2af5592`.

## Upstream issues reviewed 2026-10-04

The open teodly/inferno issues were triaged for PI9696 (read only; nothing
posted upstream):

| Issue | Relevance | Outcome |
|---|---|---|
| #8 plugin panic aborts JACK | high: in-process plugin | fixed (U16) |
| #41 stale audio on RX route disconnect | medium: could reach a take | **fixed** (U17, below) |
| #23 clock-stats not updated when the leader disappears | low | PI9696 gates recording on statime's observation socket, not clock-stats |
| #26, #45 interop with specific hardware and controllers (Red 8Pre; a controller's unknown opcode 0x3010) | unknown | need captures from that hardware |
| #36 flow timeouts at 250 µs sender latency | low | sender-side clocking (per the maintainer) |
| #13 32-bit timestamps | none (arm64) | fixed upstream |
| #1, #2, #3, #7, #9, #28, #40 | none | features and docs |

## U17 — Stale audio after an RX disconnect (teodly/inferno#41)

**Reproduced** on the dev server with `alsa_pcm_inferno/test_disconnect_tail.py`
(fork `b837e3d`): one plugin instance transmits a counting ramp, a second
captures it with arecord, a raw ARC request subscribes it and a raw 0x3014
unsubscribes it mid-capture, and the capture after the disconnect is checked
for anything but the real tail and silence. netaudio is not involved, because
it refuses to route instances hosted on the dev server ("invalid mac").

Two faults, both visible to an application reading the plugin's buffer:

- **Clicks** (every run): received samples are written ahead of the media
  clock by the channel's shift (start offset + latency), but the
  SilenceWriter, and hole closing for live channels, cleared only up to the
  clock, once per closing tick (42.7 ms with arecord's period). The reader
  ran past the cleared range between ticks and played a few samples from the
  previous ring cycle each time. Fixed by `d6f04ab`: silence and hole closing
  run two closing intervals ahead on the plugin path. (inferno2pipe reads
  through `readable_pos`, never saw it, and is unchanged.)
- **Endless replay** (about 1 run in 3): the receive thread learned the start
  time only when a packet arrived, so a channel connected before the first
  packet kept its ring position on the absolute timeline, and nothing it
  wrote afterwards cleared that channel's buffer: after a disconnect it
  replayed its last 0.34 s until the stream stopped. Fixed by `d2c936f`
  (start time polled before commands, connected sinks rebased when it
  arrives; a closed start channel no longer panics the receive thread). This
  is the path the recorder takes when inferno restores saved subscriptions
  before capture starts.

`66d67fa` closes a related ordering race: a flow's socket removed before its
channels' disconnects arrive now silences the sinks still attached instead of
dropping them.

Results: before, every run failed; after, 12 of 12 pass, with the stream
itself unchanged (same length, no gaps). All `inferno_aoip` tests and
`loopback_trx` pass. Deployed on the test unit: netaudio lists TX 32 / RX 32,
all 64 channels and 48 kHz, and a 3-channel subscribe / bulk remove is
verified.

## U18 — A transmitter restart dropped every flow

**Found** by pi9696's two-Pi `test/interop/e2e_bitperfect.py` (record on the
unit, play back, record the playback on a second Pi, compare bit for bit):
the second Pi heard 0.6 s of a 21 s playback. The ALSA plugin recovers from
a playback underrun by stopping and restarting the transmitter
(`plugin_pointer` -> EPIPE -> prepare -> StopTransmitter + StartTransmitter;
its own FIXME calls the break unacceptable), and `stop_transmitter` dropped
the `FlowsTransmitter` with every flow. Receivers then waited out their
keepalive timeout and re-requested.

**Fixed** by `382dc90`: the shutdown future snapshots the live unicast flows
(index, cookie, destination, layout) and `transmit()` restores them into the
new transmitter with the same handles, so receivers keep streaming.
Measured with the test's `--stall-at` mode (a 400 ms freeze of the app
forcing an underrun): before, longest gap 6.07 s; after, "transmitter
restarted with 4 flow(s) kept" and a 1.18 s gap, every received frame exact.

The underruns themselves came from pi9696 (fixed there: idle feeder, TryLock
pump, gapless hand-over) and the dither from not setting
`TX_SOURCE_BIT_DEPTH` (U3). With all fixes the test is bit-perfect: every
one of 32,768,000 samples (32 ch x 21.3 s) identical at the second Pi.

## U19 — The realtime loops woke the application on every packet

**Found** profiling the idle recorder (subscribed both ways to a 2-channel
interface, no take running): ~20k `ppoll`/eventfd `read`/`write` syscalls
per 5 s. The RX and TX loops called the `TransferNotifier` callback on every
iteration. The plugin's callback writes its poll eventfd, so each packet woke
the app's blocked `snd_pcm_readi`/`writei`. The app found less than a period
available and went back to sleep: ~2300 RX and ~1300 TX wakeups a second.

**Fixed** by `3881fff`: `NotifyThrottle` spaces notifications at least a
quarter of the ALSA period apart. The TX branch that precedes a long wait
still always notifies. Result: the eventfd syscalls drop out of the profile
and the process falls from 31% to 24% of one core. The remainder is real
packet I/O for the live flows (and the OLED render, fixed in pi9696).


## U20-U24 - Controller interop (device view)

**Found** comparing pi9696 with the LAN's hardware interface in netaudio
and on the wire (conmon packets on 224.0.0.231:8702, ARC on 4440), and
checking crafted packets against netaudio's own parser. What a controller
showed blank or unknown for pi9696, and the fix in the fork:

- Product Version (U20, `2bf6974`): the interface carries 01 01 00 03
  (1.1.3) at product info 0x12C. New setting PRODUCT_VERSION; pi9696 sets
  it to its app version.
- Clock role / port state (U21, `2d5eb4d`): clock status now uses the
  hardware layout with a record per PTP port; inferno reports itself as a
  follower (PTP state 9) of statime's leader.
- Sample rate and latency (U22, `ee9eece`): real 0x1100 property values
  (0x8020 rate; 0x8204/0x8205/0x8301/0x8302/0x8306 latency) and the 0x1102
  property directory, instead of zero bytes.
- 0x2204 TX flow labels (U23, `1491622`): an empty page instead of no
  reply.
- Renames (U24, `a67a337`): 0x1001 set/reset name is handed to the host
  through NAME_REQUEST_PATH; pi9696 adopts the name and restarts the
  device under it.

Verified on the test unit with netaudio: Product Version 1.20.0, Sample
Rate 48 kHz, Active/Configured/Default Latency 10 ms, Latency Range
1-40 ms, Clock Role Follower, Primary v1 Multicast Follower; `netaudio
device name` renames the unit end to end. Also in the fork: STATE_DIR
(`0501a56`) pins the saved-state directory, which was keyed by an
IP-derived device id.
