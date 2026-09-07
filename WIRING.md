# PI9696 Audio Recorder - Wiring Reference

## Pin Layout Overview

This document provides the complete wiring reference for connecting all components to the Raspberry Pi 5.

## Raspberry Pi 5 GPIO Pinout Reference

```
     3.3V  (1) (2)  5V
    GPIO2  (3) (4)  5V
    GPIO3  (5) (6)  GND
    GPIO4  (7) (8)  GPIO14
      GND  (9) (10) GPIO15
   GPIO17 (11) (12) GPIO18
   GPIO27 (13) (14) GND
   GPIO22 (15) (16) GPIO23
     3.3V (17) (18) GPIO24
   GPIO10 (19) (20) GND
    GPIO9 (21) (22) GPIO25
   GPIO11 (23) (24) GPIO8
      GND (25) (26) GPIO7
    GPIO0 (27) (28) GPIO1
    GPIO5 (29) (30) GND
    GPIO6 (31) (32) GPIO12
   GPIO13 (33) (34) GND
   GPIO19 (35) (36) GPIO16
   GPIO26 (37) (38) GPIO20
      GND (39) (40) GPIO21
```

## Component Wiring

### 1. OLED Display (SSD1322) - 256x64 Resolution

**Interface:** SPI
**Supply Voltage:** 3.3V

| Display Pin | Pi Pin | GPIO | Description |
|-------------|--------|------|-------------|
| VCC         | 1      | -    | 3.3V Power |
| GND         | 6      | -    | Ground |
| D0 (SCLK)   | 23     | 11   | SPI Clock |
| D1 (MOSI)   | 19     | 10   | SPI Data Out |
| CS          | 24     | 8    | SPI Chip Select |
| DC          | 22     | 25   | Data/Command |
| RES (Reset) | 18     | 24   | Reset |

**Wiring Notes:**
- Use short wires (< 6 inches) for SPI connections
- Twist SCLK and MOSI wires together to reduce interference
- Add 100nF capacitor between VCC and GND near display

### 2. Rotary Encoder (EC11) with Push Button

**Interface:** GPIO with pull-up resistors
**Supply Voltage:** 3.3V

| Encoder Pin | Pi Pin | GPIO | Description |
|-------------|--------|------|-------------|
| VCC         | 17     | -    | 3.3V Power |
| GND         | 6      | -    | Ground (shared) |
| A (CLK)     | 11     | 17   | Encoder A Phase |
| B (DT)      | 13     | 27   | Encoder B Phase |
| SW (Button) | 15     | 22   | Push Button |

**Wiring Notes:**
- Internal pull-ups are enabled in software
- Use shielded cable for encoder if runs are > 4 inches
- Add 100nF capacitors on A and B pins for debouncing (optional)

### 3. Control Buttons

**Interface:** GPIO with internal pull-ups
**Type:** Momentary push buttons (normally open)

| Button   | Pi Pin | GPIO | Description |
|----------|--------|------|-------------|
| Record   | 29     | 5    | Start Recording |
| Stop     | 31     | 6    | Stop Recording, or stop playback |
| Play     | 33     | 13   | Play back most recent recording |
| Common   | 6/9/14 | -    | Ground (any GND pin) |

**Wiring Notes:**
- One side of each button connects to GPIO pin
- Other side of all buttons connects to GND
- Internal pull-ups are enabled in software
- Use quality tactile switches for better feel

### 4. Button Lamps (REC / STOP / PLAY backlights)

**Interface:** GPIO digital output (no PWM/brightness control)
**Type:** Small LED backlights behind each button with a current-limiting resistor
(330Ω-1kΩ for 3.3V logic)

| Lamp  | Pi Pin | GPIO | Description |
|-------|--------|------|-------------|
| REC   | 32     | 12   | Lit while recording is in progress |
| STOP  | 36     | 16   | Lit while the app is running (booting / playback / etc.) |
| PLAY  | TBD    | TBD  | Lit while playback is active |

**Wiring Notes:**
- There are **no separate status LEDs**: these backlights plus the OLED are the status
  indication. Status details (USB, Network, WiFi if enabled, Inferno) display on the OLED's
  status screen.
- Anode (long leg) → resistor → GPIO pin; cathode (short leg) → GND
- **Software status: not yet implemented.** Per the Round 3 design the former GPIO12/16
  status LEDs were removed from the codebase; driving these three backlights is the
  recorded follow-up (see PROJECT_STATUS, Known Gaps #3). The wiring above is the
  builder's reference for when the software support lands.

### 5. Audio (AoIP / Ethernet)

**Source & playback: audio is carried over the network via Inferno (AES67/Dante).**
There is **no analog or USB audio I/O** in the build:

- Recording captures the Inferno stream into the FIFO pipeline (AES67/Dante -> FIFO ->
  ffmpeg -> WAV).
- Playback currently goes to the **local ALSA output** (`ffmpeg -f alsa default`). Routing
  playback back out through Inferno is the design target but is not yet implemented - the
  Inferno server contract is receive-only today (see PROJECT_STATUS, Known Gaps #4).
- No USB audio interface, no DAC/HAT, no XLR/TRS analog inputs.

## Power Requirements

### Total Power Consumption Estimate

| Component | Current (mA) | Notes |
|-----------|--------------|-------|
| Raspberry Pi 5 | 800-1200 | Base consumption |
| OLED Display | 50-150 | Varies with brightness |
| Encoder | 5 | Minimal |
| Buttons | <1 | When not pressed |
| Button lamps (3, planned) | 6-60 | Depends on resistor value, per lamp |
| **Total** | **~1000mA** | **At 5V (5W)** |

**Power Supply Recommendation:**
- Minimum: 5V 3A (15W) official Raspberry Pi 5 power supply
- Recommended: 5V 5A (25W) for headroom

## Construction Tips

### PCB Layout Recommendations

1. **Power Distribution:**
   - Use thick traces (>20 mil) for power
   - Add ferrite beads on 3.3V lines
   - Place bulk capacitors (100µF) near power input

2. **Signal Integrity:**
   - Keep SPI traces short and matched length
   - Use ground plane under digital signals
   - Separate analog and digital grounds

3. **EMI Reduction:**
   - Use twisted pairs for differential signals
   - Add ground plane fills
   - Keep clock signals away from analog

### Mechanical Assembly

1. **Rack Mount Considerations:**
   - Standard 19" rack width (482.6mm)
   - 1U height (44.45mm)
   - Allow clearance for connectors

2. **Front Panel Layout:**
   ```
   [OLED Display    ] [Encoder] [REC] [STOP] [PLAY]
   |---- 140mm ----| |--15mm-| |----- 90mm -----|
   ```

3. **Connector Placement:**
   - Ethernet (Inferno AoIP): Rear panel
   - Power input: Rear panel
   - USB: Rear panel (recording export / config import)

### Cable Management

1. **Internal Wiring:**
   - Use ribbon cables for GPIO connections
   - Keep high-speed signals (SPI) short
   - Route power cables away from signal cables

2. **External Connections:**
   - Use locking connectors for audio
   - Strain relief on all cables
   - Label all connections clearly

## Testing Procedures

### Initial Power-Up

1. Connect power (no other components)
2. Verify 3.3V and 5V rails
3. Check GPIO pin voltages (should be ~3.3V with pull-ups)

### Component Testing

1. **Display Test:**
   ```bash
   sudo ./test-hardware display
   ```

2. **Encoder Test:**
   ```bash
   sudo ./test-hardware encoder
   ```

3. **Button Test:**
   ```bash
   sudo ./test-hardware buttons
   ```

4. **Complete Test:**
   ```bash
   sudo ./test-hardware all
   ```

### Audio Testing

1. **Verify AoIP Stream Received:**
   Confirm the Inferno server is running (`[INF]` in the app status bar) and that the
   network stream and FIFO are flowing.

2. **Test FIFO Pipeline Recording:**
   ```bash
   cd inferno && INFERNO_SAMPLE_RATE=48000 ./target/release/inferno -c 2 -o /tmp/test.fifo
   ffmpeg -f s32le -ar 48000 -ac 2 -i /tmp/test.fifo -c:a pcm_s24le test.wav
   ```

3. **Verify File:**
   ```bash
   file test.wav
   ```

## Troubleshooting

### Display Issues

**Symptom:** No display output
- Check SPI is enabled: `lsmod | grep spi`
- Verify wiring connections
- Check 3.3V supply voltage
- Ensure CS pin is correctly connected

**Symptom:** Garbled display
- Check SPI clock frequency (max 10MHz for SSD1322)
- Verify DC pin connection
- Check for loose connections

### Encoder Issues

**Symptom:** No rotation detected
- Verify A and B phase connections
- Check for proper pull-up resistors
- Test with multimeter during rotation

**Symptom:** Erratic counting
- Add hardware debouncing capacitors
- Check for electromagnetic interference
- Verify ground connections

### Button Issues

**Symptom:** Buttons not responding
- Check GPIO pin assignments
- Verify ground connections
- Test with multimeter

**Symptom:** False triggering
- Add hardware debouncing (10k + 100nF)
- Check for ground loops
- Shield button wires if necessary

### Audio Issues

**Symptom:** No audio being recorded
- Check Inferno server status (`[INF]` in the app status bar)
- Verify Ethernet link and AoIP stream connectivity
- Check that the FIFO pipeline is flowing (Inferno writes PCM → ffmpeg reads)

**Symptom:** Poor audio quality
- Check sample rate settings
- Verify the AoIP stream's sample rate/channel/bit-depth configuration matches Inferno

## Safety Notes

- Always power off before making connections
- Use ESD precautions when handling components
- Double-check polarity on power connections
- Never exceed 3.3V on GPIO pins
- Use proper fusing on power inputs

## Revision History

| Date | Version | Changes |
|------|---------|---------|
| 2024-01-XX | 1.0 | Initial wiring specification |
| 2026-08-28 | 1.1 | Added Status LEDs (GPIO12/16); Play button no longer marked future |
| 2026-09-04 | 1.2 | Analog/USB audio I/O dropped (AoIP-only); LEDs → REC/STOP/PLAY button lamps; PLAY lamp GPIO TBD |
| 2026-09-07 | 1.3 | GPIO12/16 status LEDs removed from code (button lamps pending in software); playback documented as local ALSA (Inferno out = target, not implemented) |

---

**WARNING:** This is a reference design. Always verify connections before applying power. The authors assume no responsibility for damage caused by incorrect wiring.