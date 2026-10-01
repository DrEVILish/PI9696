#!/bin/bash
# Channel-list round trip (INFERNO-UPSTREAM.md U13): start one inferno device per
# channel count, read it back with `netaudio channel list`, and check that every
# channel number 1..N came back. Runs on a host with inferno and netaudio (the dev
# server), one device at a time.
#
#   MODE=rx|tx  TREE=<inferno checkout with target/release built>  NS="16 17 33 64"
#   CLOCK=<usrvclock socket for the TX plugin, default /run/ptp-usrvclock>
#
# rx uses $TREE's inferno2pipe; tx plays /dev/zero through $TREE's ALSA plugin,
# loaded from a private .asoundrc so the installed plugin is not touched.
W=${W:-/var/tmp/pi9696-work/channel-list-check}
TREE=${TREE:-/opt/inferno-src/inferno}
MODE=${MODE:-rx}
CLOCK=${CLOCK:-/run/ptp-usrvclock}
mkdir -p $W/home
printf 'pcm_type.inferno { lib "%s/target/release/libasound_module_pcm_inferno.so" }\n' "$TREE" > $W/home/.asoundrc
for N in ${NS:-1 16 17 32 33 64}; do
  # inferno sends no mDNS goodbye: wait for the previous device to expire
  for i in $(seq 60); do avahi-browse -atp 2>/dev/null | grep -qi "CLCHK" || break; sleep 2; done
  name=CLCHK$MODE-$N
  if [ $MODE = rx ]; then
    INFERNO_NAME=$name RUST_LOG=warn "$TREE/target/release/inferno2pipe" -c $N -o /dev/null >$W/$name.log 2>&1 & P=$!
  else
    HOME=$W/home INFERNO_CLOCK_PATH=$CLOCK INFERNO_NAME=$name INFERNO_TX_CHANNELS=$N INFERNO_RX_CHANNELS=0 RUST_LOG=warn \
      timeout 40 aplay -q -D inferno -f S32_LE -r 48000 -c $N -t raw /dev/zero >$W/$name.log 2>&1 & P=$!
  fi
  sleep 6
  netaudio -n "$name" --timeout 3 -j channel list >$W/$name.json 2>$W/$name.err
  kill $P 2>/dev/null; wait $P 2>/dev/null
  python3 - $W/$name.json $N $MODE <<'PY'
import json, sys
path, n, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
try:
    device = next(iter(json.load(open(path)).values()))
    numbers = sorted(c["number"] for c in (device.get(f"{mode}_channels") or {}).values())
    print(f"{mode} N={n:3d} read back {len(numbers):3d}  {'OK' if numbers == list(range(1, n + 1)) else 'MISMATCH'}")
except (ValueError, StopIteration, KeyError):
    print(f"{mode} N={n:3d} FAIL (netaudio did not read the device)")
PY
done
rm -rf $W/home
