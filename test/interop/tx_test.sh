#!/bin/bash
# One TX-path run: fresh ITEST-RX capture on .162, press Play on pi9696, wait
# for the take to end, stop the capture, report XRUNs/restarts on the Pi side.
# Usage: [RXLAT=ns] [ITEST_RX_HOST=user@host] [ITEST_WORK=dir] tx_test.sh <label>
# Expects the app's stderr in $W/app-sim.err and a WebUI session in $W/cookies.txt.
set -u
L=${1:-run}
W=${ITEST_WORK:-/var/tmp/pi9696-work}
R=${ITEST_RX_HOST:-root@192.168.10.162}
RXLAT=${RXLAT:-10000000}
ssh -o BatchMode=yes $R "RXLAT=$RXLAT; systemctl stop itest-rx 2>/dev/null; systemctl reset-failed itest-rx 2>/dev/null; rm -f $W/txcap_$L.raw; systemd-run -q --unit=itest-rx -p RuntimeMaxSec=400 -E INFERNO_NAME=ITEST-RX -E INFERNO_PROCESS_ID=5 -E INFERNO_ALT_PORT=10500 -E INFERNO_RX_CHANNELS=2 -E INFERNO_TX_CHANNELS=0 -E INFERNO_SAMPLE_RATE=48000 -E INFERNO_RX_LATENCY_NS=${RXLAT:-10000000} -E RUST_LOG=info /opt/inferno-src/inferno/target/release/inferno2pipe -c 2 -o $W/txcap_$L.raw"
sleep 5
ssh -o BatchMode=yes $R "timeout 60 netaudio --no-color subscription add --tx PI9696-TX --rx ITEST-RX 2>&1 | grep -v WARNING"
sleep 5
x0=$(grep -ac XRUN $W/app-sim.err); s0=$(grep -ac 'transmitter stopped' $W/app-sim.err)
echo "[$L] play at $(date +%T)"
curl -s -o /dev/null -b $W/cookies.txt -X POST http://127.0.0.1/api/input/button/play
t0=$(date +%s)
until ! curl -s -b $W/cookies.txt http://127.0.0.1/api/status | grep -q "Playing back" || [ $(( $(date +%s) - t0 )) -gt 240 ]; do sleep 2; done
t1=$(date +%s)
sleep 3
x1=$(grep -ac XRUN $W/app-sim.err); s1=$(grep -ac 'transmitter stopped' $W/app-sim.err)
echo "[$L] playback wall time $((t1 - t0))s for a 60s file; Pi XRUN=$((x1 - x0)) transmitter_restarts=$((s1 - s0))"
ssh -o BatchMode=yes $R "systemctl stop itest-rx; journalctl -u itest-rx --no-pager -o cat --since '-5min' | grep -cE 'timeout \(not receiving' | sed 's/^/[$L] RX media timeouts: /'"
