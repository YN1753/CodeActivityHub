#!/bin/sh
set -euo pipefail

APP_DIR=${APP_DIR:-/opt/codeactivityhub}
PORT=${PORT:-2053}
mkdir -p "$APP_DIR"
cd "$APP_DIR"

# 发布前备份上一版可执行文件，供健康检查失败时回滚
BACKUP_BIN="$APP_DIR/codeactivityhub-server.bak"
if [ -f "$APP_DIR/codeactivityhub-server" ]; then
  cp "$APP_DIR/codeactivityhub-server" "$BACKUP_BIN"
fi

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

# 健康检查：轮询 /healthz，连续失败则回滚到上一版产物
health_ok=0
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    health_ok=1
    break
  fi
  sleep 2
done

if [ "$health_ok" -ne 1 ]; then
  echo "部署后健康检查失败：端口 $PORT 的 /healthz 在超时时间内未就绪。" >&2
  if [ -f "$BACKUP_BIN" ]; then
    echo "正在回滚到上一版可执行文件并重启服务..." >&2
    cp "$BACKUP_BIN" "$APP_DIR/codeactivityhub-server"
    systemctl restart codeactivityhub
  else
    echo "未找到上一版备份，无法自动回滚，请人工介入。" >&2
  fi
  exit 1
fi

echo "CodeActivityHub is running at http://127.0.0.1:$PORT"
echo "No background OJ polling is enabled; use scripts/codeactivityhub-tampermonkey.user.js."
