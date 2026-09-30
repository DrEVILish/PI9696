#!/bin/bash
# Remote-controller view of the pi9696 devices: netaudio table + the per-channel
# mDNS records (rate/enc/nchan), as seen from whichever host runs this.
echo "== netaudio device list (from $(hostname))"
timeout 45 netaudio --no-color device list 2>/dev/null | grep -E '^(Name|PI9696)'
echo "== netaudio channel counts"
timeout 45 netaudio --no-color -n 'PI9696*' channel list 2>/dev/null | grep -E 'Channels$' | while read -r l; do echo "$l"; done
timeout 45 netaudio --no-color -n 'PI9696*' channel list 2>/dev/null | grep -cE '^\s*[0-9]+\s' | sed 's/^/total channel rows: /'
if command -v avahi-browse >/dev/null; then
  echo "== mDNS _netaudio-chan TXT (rate/enc/nchan) for PI9696*"
  timeout 8 avahi-browse -rtp _netaudio-chan._udp 2>/dev/null | grep '^=' | grep -i 'pi9696' \
    | while IFS=';' read -r _ _ _ name _ _ _ _ _ txt; do
        echo "$name $(grep -oE '"(nchan|rate|enc)=[^"]*"' <<<"$txt" | tr '\n' ' ')"
      done | sort -u
fi
