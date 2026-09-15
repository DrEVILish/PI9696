# AGENTS.md

Raspberry Pi digital audio recorder: records an Inferno (AES67/Dante) network stream via a FIFO into ffmpeg → WAV on `/rec/<date>/`, with an SSD1322 OLED + buttons/encoder front panel and a token-auth WebUI. No hardware is present in a normal dev environment — everything runs in sim mode.

## Workflow facts that matter most

- **Sim mode is the default dev path.** `PI9696_SIM=1 ./pi9696` runs without SPI/GPIO. The remote-access token is printed to **stderr** (`sim mode: remote access token <T>`). WebUI is on `:8080`. Always launch detach-safe, e.g. `PI9696_SIM=1 setsid ./pi9696 </dev/null &`, or the shell tool hangs waiting on the inherited fd.
- **Config path differs by mode.** `PI9696_SIM=1` → `/tmp/pi9696-config.json`; real Pi → `/etc/pi9696/config.json`; override with `PI9696_CONFIG`. This is resolved once in `init()`, so **env changes at runtime are ignored** (a past bug: per-test env actually wrote the real `/etc` file).
- **WebUI auth flow: POST form value, not header/query.** `curl -c jar -d "token=<T>" :8080/login` (returns 303 + session cookie `pi9696_session`); dashboard is `/dashboard` (`/` forwards to login). Use `-b/-c` cookie jar. The `token=` is a **form value**, not a URL query.
- **USB must be a real mount to count as mounted.** `usbDevicePath()` checks `/proc/mounts` for `/media/usb`. A bare directory is ignored. To test USB features (config export/import, copy files), `mount -t tmpfs none /media/usb` first.
- **The app sim config has no Inferno binary** (`inferno/target/release/inferno`), so in sim the INFERNO-LINK lamp stays off and Start Recording is disabled — expected, not a bug.
- No Makefile. Commands: `go build ./...`, `go vet ./...`, `go test ./...`. One feature/fix per commit with detailed messages; docs updated in a separate commit after features.

## Testing

- 45 tests in `main_test.go` (1 in `logging_test.go`). They run the **real HTTP handlers over `httptest`** (auth, recordings API, ZIP download, settings), the playback/seek lifecycle against a **fake `ffmpeg`** via a PATH shim, and the Inferno worker concurrency against a stub server.
- `go test ./...` shares a single `infernoWorker` across the whole suite. Keep tests mutex-safe and restore package globals (e.g. reset `usbMounted` in cleanup) — tests are run together and order-independence matters.
- `cmd/simcheck` renders every OLED screen to PNG for visual layout checking; it hardcodes its own menu items, so new OLED screens are not covered — but you can drive the *real* menu in sim via the WebUI's on-screen encoder endpoints (`/api/input/encoder/*`, `/api/input/button/*`).

## Architecture notes

- **Layout is unusual**: app logic is in `main.go` (~4100 lines), the WebUI only in `remote.go` (~2700 lines); there is no `pkg/` split. Shared state (transport, meters, config) is package globals guarded by a single package-level `mutex`.
- **Concurrency rules that are load-bearing** (violating them caused past bugs):
  - All shared globals (`isRecording`, `currentState`, meter slices, config, `deviceName`) are touched only under `mutex`. The 100 ms render tick and the WebUI deck (`onEncoderRotate`/`onButtonPress`/`handleWSMeter`) must never read them unlocked.
  - **Inferno lifecycle is `infernoWorker`-owned** — the only goroutine that mutates `infernoCmd`/`infernoState` mid-operation; everything else enqueues a request and returns.
  - **Meter ownership transfers on recording start.** `startMonitor`'s reaping goroutine clears `meterChannelPeak`/`meterChannelRMS` to nil if `monitorCmd==cmd`; `startRecording` only SIGTERMs the monitor before reallocating the slices, so a stale monitor goroutine can wipe a fresh recording's meters. Don't add release-resource-by-goroutine logic that races a reallocation.
  - The recording/finalize goroutines fsync the take content (and `persistConfig` is temp+rename) — but directory-entry fsync is acknowledged-unimplemented. Don't "improve" the FIFO handoff (monitor→record) without reading the comment at `stopMonitor`/`startRecording`; it's an accepted corruption window, not a naive bug.
- **Remote input handlers call the exact same functions as physical hardware** (`onEncoderRotate`/`onButtonPress`), so all state-machine guards apply in WebUI too — keep it that way, don't add web-only state paths.
- `ponytail` plugin (lazy/simplest-solution) is enabled via `.claude/settings.json`; it wants minimal diffs, root-cause fixes, and a runnable assert/test for non-trivial logic.

## Conventions

- Feature history lives at the top of `### Feature History` in `PROJECT_STATUS.md`; running-gap items are tracked in that file's `## Known Gaps & Backlog` (renumbered whenever items are done). Version bumps are recorded there (current ~1.17.x) — not in code.
- Docs: `README.md` is the design guide (features/UX/UI/architecture), `WIRING.md` is GPIO/power, `PROJECT_STATUS.md` is the design record (decisions, status, feature history). Keep lamp/pin tables in sync with `hardware/lamps.go`.
- The OLED uses fixed 256×64 layout with specific font contexts (`statusbar`/`header`/`menu`/`selected`/`details`/`alert`); new menu text must fit one 256px line or it silently overflows.