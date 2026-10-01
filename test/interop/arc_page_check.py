#!/usr/bin/env python3
"""Feed the ARC channel-list pages in a capture to netaudio's own page parser.

Shows, page by page, whether netaudio accepts what an inferno device sent,
which is how INFERNO-UPSTREAM.md U13 was pinned down. Capture with e.g.
`tcpdump -i any -w x.pcap 'udp port 4440'` while running `netaudio channel list`.

Usage: arc_page_check.py <capture.pcap> [--from IP]
"""
import argparse, struct, sys

sys.path.insert(0, "/usr/local/lib/python3.13/dist-packages")
from netaudio.core import binding

KINDS = {"3000": "rx", "2000": "tx_info", "2010": "tx_friendly"}
LINK_HEADER = {1: 14, 113: 16, 276: 20}  # ethernet, Linux cooked v1, v2


def udp_payloads(path):
    f = open(path, "rb")
    link = struct.unpack("<I", f.read(24)[20:24])[0]
    off = LINK_HEADER[link]
    while True:
        h = f.read(16)
        if len(h) < 16:
            return
        d = f.read(struct.unpack("<IIII", h)[2])[off:]
        ihl = (d[0] & 15) * 4
        src = ".".join(map(str, d[12:16]))
        sport, dport = struct.unpack(">HH", d[ihl:ihl + 4])
        yield src, sport, dport, d[ihl + 8:]


p = argparse.ArgumentParser()
p.add_argument("pcap")
p.add_argument("--from", dest="src", help="only pages sent by this IP")
a = p.parse_args()

requests = {}
for src, sport, dport, u in udp_payloads(a.pcap):
    op = u[6:8].hex()
    if op not in KINDS:
        continue
    if dport == 4440:  # request: remember its start channel by transaction id
        requests[(sport, u[4:6])] = struct.unpack(">H", u[12:14])[0]
        continue
    if a.src and src != a.src:
        continue
    start = requests.get((dport, u[4:6]))
    if start is None:
        continue
    head = f"{src} op={op} start={start:3d} code=0x{u[8:10].hex()} bytes0/1={u[10]}/{u[11]} len={len(u)}"
    try:
        records = binding.parse_page(KINDS[op], bytes(u), start)
        print(f"{head}  OK ({len(records)} entries)")
    except binding.NetaudioCoreError as e:
        print(f"{head}  REJECTED: {e}")
