# PI9696 — Wiring Reference

Pin layouts, component wiring, and testing for the Raspberry Pi 5 audio recorder.

---

## GPIO Pinout

```
     3.3V  (1) (2)  5V          GPIO0  (27) (28)  GPIO1
    GPIO2  (3) (4)  5V          GPIO5  (29) (30)  GND
    GPIO3  (5) (6)  GND         GPIO6  (31) (32)  GPIO12 ← REC lamp
    GPIO4  (7) (8)  GPIO14      GPIO13 (33) (34)  GND
      GND  (9) (10) GPIO15      GPIO19 (35) (36)  GPIO16 ← PLAY lamp
   GPIO17 (11) (12) GPIO18      GPIO26 (37) (38)  GPIO20
   GPIO27 (13) (14) GND         GND    (39) (40)  GPIO21
   GPIO22 (15) (16) GPIO23
     3.3V (17) (18) GPIO24
   GPIO10 (19) (20) GND
    GPIO9 (21) (22) GPIO25
   GPIO11 (23) (24) GPIO8
      GND (25) (26) GPIO7
```

---

## Component Wiring

### 1. OLED Display (SSD1322, 256×64, SPI)

| Display | Pi Pin | GPIO | Function |
|---------|--------|------|----------|
| VCC     | 1      | —    | 3.3V     |
| GND     | 6      | —    | Ground   |
| D0      | 23     | 11   | SCLK     |
| D1      | 19     | 10   | MOSI     |
| CS      | 24     | 8    | CS       |
| DC      | 22     | 25   | DC       |
| RES     | 18     | 24   | Reset    |

- Short wires (< 6 in), twist SCLK+MOSI, 100nF cap near VCC.

### 2. Rotary Encoder (EC11 + push)

| Encoder | Pi Pin | GPIO | Function |
|---------|--------|------|----------|
| VCC     | 17     | —    | 3.3V     |
| GND     | 6      | —    | Ground   |
| A       | 11     | 17   | CLK      |
| B       | 13     | 27   | DT       |
| SW      | 15     | 22   | Button   |

- Internal pull-ups in software; optional 100nF caps on A/B for debounce.

### 3. Transport Buttons

| Button | Pi Pin | GPIO | Function |
|--------|--------|------|----------|
| Record | 29     | 5    | Start    |
| Stop   | 31     | 6    | Stop     |
| Play   | 33     | 13   | Play     |
| GND    | any GND | —  | Common   |

- Internal pull-ups in software; momentary normally-open switches.

### 4. Button Lamps

| Lamp | Pi Pin | GPIO | Behaviour |
|------|--------|------|-----------|
| REC  | 32     | 12   | Lit while recording |
| PLAY | 36     | 16   | Solid while playing; 250 ms blink while paused |
| STOP | —      | —    | None (Round-4 decision) |

- Digital output, change-only writes (only when state changes).
- Anode → resistor (330Ω–1kΩ) → GPIO; cathode → GND.

### 5. Audio (Ethernet Only)

No analog or USB audio I/O — audio is AES67/Dante over Ethernet via Inferno.
Playback currently goes to local ALSA (target: route out through Inferno — Known Gaps #1).

---

## Power Budget

| Component | mA | Notes |
|-----------|----|-------|
| Raspberry Pi 5 | 800–1200 | Base |
| OLED Display | 50–150 | Varies with brightness |
| Encoder | 5 | Minimal |
| Buttons | <1 | When not pressed |
| Button lamps | 6–60 | Per lamp, depends on resistor |
| **Total** | **~1000** | **5V (5W)** |

**Supply:** 5V 3A (15W) minimum; 5V 5A (25W) recommended.

---

## Construction

### Front Panel Layout

```
[OLED Display    ] [Encoder] [REC] [STOP] [PLAY]
|---- 140mm ----| |--15mm-| |----- 90mm -----|
```

### Connectors (Rear Panel)

- Ethernet (Inferno AoIP)
- Power input
- USB (recording export / config import)

### Assembly Notes

- Rack: 19" (482.6mm), 1U (44.45mm)
- SPI traces short and matched length, ground plane under digital
- Power traces > 20 mil, 100µF bulk caps near power input

---

## Testing

### Power-Up Sequence

1. Power only → verify 3.3V/5V rails, GPIO pull-up voltages
2. Display test: `sudo ./test-hardware display`
3. Encoder test: `sudo ./test-hardware encoder`
4. Button test: `sudo ./test-hardware buttons`
5. Complete test: `sudo ./test-hardware all`

### Audio Verification

```bash
# Verify Inferno stream + FIFO pipeline
cd inferno && INFERNO_SAMPLE_RATE=48000 ./target/release/inferno -c 2 -o /tmp/test.fifo
ffmpeg -f s32le -ar 48000 -ac 2 -i /tmp/test.fifo -c:a pcm_s24le test.wav
file test.wav
```

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| No display | SPI enabled? (`lsmod \| grep spi`), wiring, 3.3V supply |
| Garbled display | SPI clock < 10 MHz, DC pin, loose connections |
| No rotation | A/B phase wiring, pull-ups, multimeter during rotation |
| Erratic counting | Debounce caps (10k + 100nF), EMI shielding |
| No audio | `[INF]` in app? Ethernet link? AoIP stream? FIFO flowing? |
| Poor audio quality | Sample rate/channels match Inferno config? |

---

**⚠ Warning:** Reference design — verify all connections before applying power.