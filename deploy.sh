#!/bin/sh
set -eu

APP_DIR=${APP_DIR:-/opt/codeactivityhub}
PORT=${PORT:-2053}
mkdir -p "$APP_DIR"
cd "$APP_DIR"

echo "Building CodeActivityHub frontend..."
npm --prefix frontend install
npm --prefix frontend run build

echo "Building CodeActivityHub Go backend..."
go build -o codeactivityhub-server ./backend/cmd/server

if [ -f codeactivityhub.service ]; then
  sed \
    -e "s#Environment=CODEACTIVITYHUB_ADDR=.*#Environment=CODEACTIVITYHUB_ADDR=:$PORT#" \
    -e "s#Environment=CODEACTIVITYHUB_DB=.*#Environment=CODEACTIVITYHUB_DB=$APP_DIR/data/codeactivityhub.db#" \
    -e "s#Environment=CODEACTIVITYHUB_FRONTEND_DIST=.*#Environment=CODEACTIVITYHUB_FRONTEND_DIST=$APP_DIR/frontend/dist#" \
    codeactivityhub.service > /tmp/codeactivityhub.service
  cp /tmp/codeactivityhub.service /etc/systemd/system/codeactivityhub.service
fi
systemctl daemon-reload
systemctl enable codeactivityhub
systemctl restart codeactivityhub

echo "CodeActivityHub is running at http://127.0.0.1:$PORT"
echo "No background OJ polling is enabled; use scripts/codeactivityhub-tampermonkey.user.js."
