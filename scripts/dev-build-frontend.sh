#!/bin/sh
# air 的 pre_cmd：只在前端源文件比构建产物新时才重新构建 frontend/dist。
# 后端直接静态服务 frontend/dist，不做这步改了 index.html/App.js 就不会生效；
# 但每次保存 .go 都跑一次 npm build 会白白多等 2 秒，所以这里加了变更判断。
set -e

cd "$(dirname "$0")/.."

need_build=0
if [ ! -f frontend/dist/index.html ]; then
  need_build=1
elif find frontend/src frontend/index.html frontend/public frontend/tailwind.config.js frontend/vite.config.js -newer frontend/dist/index.html -print -quit | grep -q .; then
  need_build=1
fi

if [ "$need_build" = "1" ]; then
  echo "[air] 检测到前端变更，正在构建 frontend/dist ..."
  npm --prefix frontend run build
fi
