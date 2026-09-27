#!/bin/sh
# inferno-loopback.sh - prove audio passes IN and OUT of the inferno
# environment on a single host, with no Dante hardware present.
#
# Two inferno instances are built from the same library:
#
#   pi9696tx    ALSA virtual device (alsa_pcm_inferno). The tone is *played
#               into* it, so it TRANSMITS the audio onto the Dante network.
#               This is the "audio OUT of inferno" direction.
#   pi9696rx    inferno2pipe, subscribed to pi9696tx, writing received audio
#               to a file. This is the "audio IN to inferno" direction and is
#               the same tool/CLI the PI9696 app runs for recording.
#   pi9696arec  optional second ALSA device, *capturing from* inferno, to
#               prove the reverse ALSA direction (Dante RX -> ALSA capture)
#               that a Dante output path would use.
#
# Requires a usrvclock server exporting the clock overlay (see DEPLOYMENT.md).
# Without one, inferno starts but transmission fails with
# "no clock available (timeout waiting for overlay update)".
#
# Usage: ./inferno-loopback.sh [seconds]     (default 20)

set -eu

DUR=${1:-20}
RATE=48000
CHANS=2
TONE_HZ=1000
WORK=${WORK:-/tmp/inferno-loopback}
HERE=$(dirname "$0")
I2P="$HERE/inferno/target/release/inferno2pipe"

# instances must be told apart: inferno binds fixed UDP ports (ARC 4440, CMC
# 8800, info 8700) so only one instance can use the defaults. ALT_PORT moves
# the whole block; PROCESS_ID keeps the Dante instance IDs unique.
RX_NAME=pi9696rx
TX_NAME=pi9696tx
AR_NAME=pi9696arec

cleanup() {
    pkill -x aplay    2>/dev/null || true
    pkill -x arecord  2>/dev/null || true
    pkill -x inferno2pipe 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# -x for the path (command -v does not accept a path in some shells),
# command -v for the PATH-resolved tools.
[ -x "$I2P" ] || { echo "missing inferno receiver: $I2P (cargo build --release in inferno/)"; exit 1; }
for f in netaudio ffmpeg python3; do
    command -v "$f" >/dev/null 2>&1 || { echo "missing required tool: $f"; exit 1; }
done
[ -S /tmp/ptp-usrvclock ] || { echo "no usrvclock socket at /tmp/ptp-usrvclock - start a clock source first"; exit 1; }

cleanup
sleep 1
# Subscriptions live in inferno's state dir, so a stale one from a previous run
# can make the "Unresolved" list look settled while no audio actually flows.
# Start from a clean slate so what we verify is what this run established.
rm -rf "${HOME:-/root}/.local/state/inferno_aoip"
rm -rf "$WORK"
mkdir -p "$WORK"

echo "== generating ${DUR}s ${TONE_HZ}Hz test tone"
ffmpeg -hide_banner -loglevel error -f lavfi \
    -i "sine=frequency=${TONE_HZ}:duration=$((DUR + 60))" \
    -f s32le -ar "$RATE" -ac "$CHANS" "$WORK/tone.raw" -y

echo "== starting receiver (inferno2pipe) and transmitter (alsa virtual device)"
INFERNO_NAME=$RX_NAME INFERNO_RX_CHANNELS=$CHANS INFERNO_SAMPLE_RATE=$RATE \
    "$I2P" -c "$CHANS" -o "$WORK/rx.raw" > "$WORK/rx.log" 2>&1 &

INFERNO_NAME=$TX_NAME INFERNO_PROCESS_ID=1 INFERNO_ALT_PORT=10100 \
    INFERNO_TX_CHANNELS=$CHANS INFERNO_RX_CHANNELS=0 INFERNO_SAMPLE_RATE=$RATE \
    aplay -D inferno -f S32_LE -r "$RATE" -c "$CHANS" "$WORK/tone.raw" > "$WORK/tx.log" 2>&1 &

INFERNO_NAME=$AR_NAME INFERNO_PROCESS_ID=2 INFERNO_ALT_PORT=10200 \
    INFERNO_RX_CHANNELS=$CHANS INFERNO_TX_CHANNELS=0 INFERNO_SAMPLE_RATE=$RATE \
    arecord -D inferno -d "$((DUR + 60))" -c "$CHANS" -r "$RATE" -f S32_LE \
    "$WORK/arec.raw" > "$WORK/arec.log" 2>&1 &

# wait for all three devices to advertise themselves before wiring them up
echo "== waiting for devices to appear"
i=0
while [ $i -lt 30 ]; do
    devs=$(netaudio device list 2>/dev/null | tail -n +2 | awk '{print $1}')
    if echo "$devs" | grep -q "$RX_NAME" &&
       echo "$devs" | grep -q "$TX_NAME" &&
       echo "$devs" | grep -q "$AR_NAME"; then
        break
    fi
    i=$((i + 1))
    sleep 1
done
netaudio device list 2>/dev/null | tail -n +2 | awk '{print "   " $1, $2, "tx:" $7, "rx:" $8}'

echo "== subscribing receivers to the transmitter"
# netaudio is chatty and occasionally races device readiness, so retry rather
# than trusting a single call - and never let its exit code abort the run.
subscribe() {
    attempt=0
    while [ $attempt -lt 5 ]; do
        if netaudio subscription add --tx "tx:$1@$TX_NAME" --rx "rx:$1@$RX_NAME" >/dev/null 2>&1 &&
           netaudio subscription add --tx "tx:$1@$TX_NAME" --rx "rx:$1@$AR_NAME" >/dev/null 2>&1; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 2
    done
    echo "   warning: could not subscribe channel $1"
    return 1
}
c=1
while [ $c -le "$CHANS" ]; do
    subscribe "$c" || true
    c=$((c + 1))
done
netaudio subscription list 2>/dev/null | tail -n +2 | awk '{print "   " $0}'

echo "== letting audio flow for ${DUR}s"
sleep "$DUR"

verify() {
    label=$1
    raw=$2
    wav="$WORK/$label.wav"
    ffmpeg -hide_banner -loglevel error -f s32le -ar "$RATE" -ac "$CHANS" \
        -i "$raw" -c:a pcm_s24le "$wav" -y

    # non-silence check: mean_volume well above -90 dBFS means real audio arrived
    mean=$(ffmpeg -hide_banner -i "$wav" -af volumedetect -f null - 2>&1 |
           sed -n 's/.*mean_volume: \(-\?[0-9.]*\) dB/\1/p' | head -1)
    # dominant frequency by rising zero-crossing count (fine for a pure tone).
    # Sample a window 2s in: the head of the capture is silence, because the
    # device is open before the subscription exists.
    hz=$(python3 - "$raw" "$CHANS" "$RATE" <<'EOF'
import struct, sys
path, chans, rate = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
with open(path, "rb") as f:
    f.seek(rate * chans * 4 * 2)          # skip first 2 seconds
    data = f.read(rate * chans * 4)       # analyse 1 second
if len(data) < 4:
    print(-1)
    raise SystemExit
s = struct.unpack("<%di" % (len(data) // 4), data)[0::chans]
print(sum(1 for i in range(1, len(s)) if s[i-1] < 0 <= s[i]))
EOF
)
    echo "   $label: mean=${mean}dBFS  tone=${hz}Hz  (source ${TONE_HZ}Hz)"
    # awk for the float compares: POSIX test -gt is integer-only. Require both a
    # real level and a plausible dominant frequency, so a quiet noise floor
    # can't pass as "audio arrived".
    [ -n "$mean" ] && [ -n "$hz" ] && awk -v m="$mean" -v h="$hz" -v t="$TONE_HZ" \
        'BEGIN { exit !(m+0 > -60 && h+0 > t*0.85 && h+0 < t*1.15) }'
}

rc=0
echo "== verifying captured audio"
if verify "inferno2pipe (Dante RX -> file)" "$WORK/rx.raw"; then
    echo "   OK: audio received from inferno's Dante transmit"
else
    echo "   FAIL: no audio received by inferno2pipe"
    rc=1
fi

# Informational only. On a single host every instance shares one IP, so the
# unicast flow addresses the transmitter advertises (192.0.2.69:<alt_port>)
# cannot be told apart per receiver, and this second receiver commonly records
# silence. Proving the Dante-RX -> ALSA direction properly needs the receiver on
# a second host/IP. Do not treat a failure here as a pass/fail of the build.
if verify "alsa capture (Dante RX -> ALSA)" "$WORK/arec.raw"; then
    echo "   OK: second ALSA receiver also captured audio"
else
    echo "   SKIP: second ALSA receiver silent (expected on a single host - see comment)"
fi

if [ $rc -eq 0 ]; then
    echo "== PASS: audio passed out of inferno (ALSA -> Dante TX) and back in (Dante RX -> inferno2pipe)"
else
    echo "== FAIL"
    echo "   rx log:  $WORK/rx.log"
    echo "   tx log:  $WORK/tx.log"
    echo "   arec log: $WORK/arec.log"
fi
exit $rc
