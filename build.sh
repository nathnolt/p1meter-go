#!/usr/bin/env bash
# Builds everything you need into ./dist
#   ./build.sh            -> 64-bit Raspberry Pi (aarch64)   [default]
#   ./build.sh armv7      -> 32-bit Raspberry Pi OS
#   ./build.sh pc         -> this computer (for trying it out)
# Optional: PI_USER=myuser ./build.sh   (user the systemd service runs as, default "pi")
set -euo pipefail
cd "$(dirname "$0")"

TARGET="${1:-arm64}"
PI_USER="${PI_USER:-pi}"
VERSION="${VERSION:-1.4.0}"

case "$TARGET" in
  arm64) export GOOS=linux GOARCH=arm64 ;;
  armv7)            export GOOS=linux GOARCH=arm GOARM=7 ;;
  pc)               unset GOOS GOARCH GOARM ;;
  *) echo "unknown target '$TARGET' (use: arm64, armv7, pc)"; exit 1 ;;
esac
export CGO_ENABLED=0

mkdir -p dist/data/logs/master dist/data/logs/user
rm -f dist/p1meter dist/p1meter-linux-*
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/p1meter .

cat > dist/data/config.example.json <<'JSON'
{
  "host": "192.168.1.50",
  "token": "PUT-YOUR-32-CHARACTER-TOKEN-HERE",
  "userName": "local/p1-deno-app",
  "pairedAt": "2026-01-01T12:00:00Z"
}
JSON

cat > dist/p1meter.service <<EOF2
[Unit]
Description=P1 meter dashboard and logger
After=network-online.target time-sync.target
Wants=network-online.target

[Service]
User=$PI_USER
WorkingDirectory=/home/$PI_USER/p1meter
ExecStart=/home/$PI_USER/p1meter/p1meter -addr :8001 -data /home/$PI_USER/p1meter/data
# Optional password for the web UI:
# Environment=P1_AUTH=admin:change-me
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF2

echo "Built dist/ for $TARGET:"
(cd dist && find . -not -path './data/logs/*/*' | sort)
