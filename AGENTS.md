# AGENTS.md

Raspberry Pi digital audio recorder: records an Inferno (AES67/Dante) network stream via a FIFO into ffmpeg → WAV on `/rec/<date>/`, with an SSD1322 OLED + buttons/encoder front panel and a token-auth WebUI. No hardware is present dev environment.

After each Bug fix or feature request, each must be commited as seperate items with notes and information so that any mistakes or issues can be easily reverted.
After each commit and build, restart the pi9696 service.

## Environments

- `pi9696-test.drevilish.com` is a reverse proxy to the pi9696 **test** server, `192.168.10.69`, serving on port 80. The test server runs on the target hardware.
- `pi9696-dev.drevilish.com` is a reverse proxy to the pi9696 **dev** server, `192.168.10.162`, serving on port 8080.
- Both machines are reachable via `ssh root@<ip-address>`. The repo is installed under `/opt` on each.

## Conventions

- Docs: `README.md` is the single design document (features/UX/UI/architecture/decisions/status); `WIRING.md` is GPIO/power. There is no separate design record. Keep lamp/pin tables in sync with `hardware/lamps.go`.
- The OLED uses fixed 256×64 layout with specific font contexts (`statusbar`/`header`/`menu`/`selected`/`details`/`alert`/`recording`); new menu text must fit one 256px line or it silently overflows.
