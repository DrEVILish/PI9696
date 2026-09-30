# AGENTS.md

Raspberry Pi digital audio recorder: records an Inferno (AES67) network stream via a FIFO into ffmpeg → WAV on `/rec/<date>/`, with an SSD1322 OLED + buttons/encoder front panel and a token-auth WebUI. No hardware is present dev environment.

After each Bug fix or feature request, each must be commited as seperate items with notes and information so that any mistakes or issues can be easily reverted.
After each commit and build, restart the pi9696 service.

## Environments

- `test-unit.example` is a reverse proxy to the pi9696 **test** server, `192.0.2.69`, serving on port 80. The test server runs on the target hardware.
- `dev-server.example` is a reverse proxy to the pi9696 **dev** server, `192.0.2.162`, serving on port 8080.
- Both machines are reachable via `ssh root@<ip-address>`. The repo is installed under `/opt` on each.
- **Dev only:** you may install extra tooling (npm, Playwright/Chromium or other browsers, etc.) for screenshots and UI testing.
- **Test:** never install extra applications or packages on the test server. It runs on the target hardware and must stay limited to what the recorder itself needs. Do screenshots and browser-driven testing from the dev server instead.

## Conventions

- Docs: `README.md` is the single design document (features/UX/UI/architecture/decisions/status); `WIRING.md` is GPIO/power. There is no separate design record. Keep lamp/pin tables in sync with `hardware/lamps.go`.
- Terminology: documentation never says "Dante" - always refer to inferno (inferno network, inferno TX/RX, inferno controller). Paraphrase quoted upstream text or tool output rather than reproducing the word.
- The OLED uses fixed 256×64 layout with specific font contexts (`statusbar`/`header`/`menu`/`selected`/`details`/`alert`/`recording`); new menu text must fit one 256px line or it silently overflows.
