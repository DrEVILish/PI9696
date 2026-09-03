#!/bin/bash
# PI9696 rebuild + restart helper.
#
# After editing Go source, run this from the project directory to rebuild the
# binary and restart the systemd service. Avoids the "edited code, forgot to
# rebuild/restart" trap. Requires sudo for the service restart (the service
# itself runs as root) and for GPIO/SPI access.
#
# Usage:
#   ./restart-pi9696.sh            # rebuild + restart
#   ./restart-pi9696.sh logs       # tail the service logs instead
#   ./restart-pi9696.sh status     # show service status instead

set -e

cd "$(dirname "$0")"

if [ "$1" = "logs" ]; then
    exec journalctl -u pi9696 -f
fi
if [ "$1" = "status" ]; then
    exec systemctl status pi9696 --no-pager -l
fi

echo "==> Building pi9696 binary"
go mod tidy
go build -ldflags "-X main.BuildTime=$(date -u '+%Y-%m-%d_%H:%M:%S')" -o pi9696 .

echo "==> Restarting pi9696 service"
sudo systemctl restart pi9696.service || {
    echo "Service restart failed. Check: systemctl status pi9696"
    echo "Or view logs: journalctl -u pi9696 -f"
    exit 1
}

echo "==> Service restarted"
sudo systemctl status pi9696.service --no-pager -l | head -12
