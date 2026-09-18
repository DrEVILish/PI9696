# AGENTS.md

Raspberry Pi digital audio recorder: records an Inferno (AES67/Dante) network stream via a FIFO into ffmpeg → WAV on `/rec/<date>/`, with an SSD1322 OLED + buttons/encoder front panel and a token-auth WebUI. No hardware is present dev environment.

After each Bug fix or feature request, each must be commited as seperate items with notes and information so that any mistakes or issues can be easily reverted.
After each commit and build, restart the pi9696 service.

## Conventions

- Docs: `README.md` is the design guide (features/UX/UI/architecture), `WIRING.md` is GPIO/power, `PROJECT_STATUS.md` is the design record (decisions, status, feature history). Keep lamp/pin tables in sync with `hardware/lamps.go`.
- The OLED uses fixed 256×64 layout with specific font contexts (`statusbar`/`header`/`menu`/`selected`/`details`/`alert`); new menu text must fit one 256px line or it silently overflows.
