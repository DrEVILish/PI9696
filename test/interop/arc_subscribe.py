#!/usr/bin/env python3
"""Subscribe RX 1..N of a device to "TX k"@<tx device> with raw ARC requests.

netaudio's `subscription add` first reads the receiver's current
subscriptions and aborts when that read fails - which it does for a stock
inferno receiver above 16 channels (INFERNO-UPSTREAM.md U13). This sends the same add_subscriptions packets
(built with netaudio's own encoder) straight to the receiver's ARC port, in
batches of 16, and checks each reply. Confirm the result from the audio
(meters), not from a readback.

Usage: arc_subscribe.py <rx_ip> <rx_arc_port> <tx_device> <channels> [--first N]
"""
import argparse, socket, sys

sys.path.insert(0, "/usr/local/lib/python3.13/dist-packages")
from netaudio.dante.device_commands import DanteDeviceCommands

p = argparse.ArgumentParser()
p.add_argument("rx_ip")
p.add_argument("rx_port", type=int)
p.add_argument("tx_device")
p.add_argument("channels", type=int)
p.add_argument("--first", type=int, default=1)
a = p.parse_args()

cmds = DanteDeviceCommands()
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(3.0)
ok = failed = 0
chans = list(range(a.first, a.first + a.channels))
for i in range(0, len(chans), 16):
    batch = [(c, f"TX {c}", a.tx_device) for c in chans[i:i + 16]]
    pkt = cmds.command_add_subscriptions(batch)
    pkt = pkt[0] if isinstance(pkt, tuple) else pkt
    s.sendto(pkt, (a.rx_ip, a.rx_port))
    try:
        data, _ = s.recvfrom(65536)
        code = data[8:10].hex()
        if code in ("0001", "8112"):
            ok += len(batch)
        else:
            failed += len(batch)
            print(f"RX {batch[0][0]}-{batch[-1][0]}: result 0x{code}")
    except socket.timeout:
        failed += len(batch)
        print(f"RX {batch[0][0]}-{batch[-1][0]}: no reply")
print(f"subscribed {ok}/{len(chans)} RX channels of {a.rx_ip}:{a.rx_port} to {a.tx_device} ({failed} failed)")
sys.exit(1 if failed else 0)
