# CodeActivityHub

跨平台算法刷题数据看板，保留 GitHub 风格年度热力图、提交流、标签统计和错题本；后端已切换为 Go，数据同步改为**浏览器事件推送 + 手动读取聚合数据**。

## 现在的同步方式

CodeActivityHub 不执行后台定时轮询。需要历史数据、公开用户资料、题库和比赛时，由 Go 后端在用户点击“刷新看板”或打开对应页面时按需调用公开接口；只有提交后的实时事件使用浏览器中的 `scripts/codeactivityhub-tampermonkey.user.js`：

1. 你在 OJ 页面正常提交题目；
2. 脚本监听页面已经发出的 `fetch` / `XMLHttpRequest` 提交请求，并读取页面已经收到的判题结果；
3. 脚本将结构化副本 POST 到 `/api/ingest/submission`；
4. Go 后端按 `(user_id, platform, raw_id)` 幂等写入 SQLite；
5. 看板在页面切换或点击“刷新看板”时读取最新数据，实时事件到达后也可手动刷新查看。

脚本不会重放 OJ 请求，不会主动请求题库/提交历史，不会上传完整代码正文；请求正文会截断并脱敏 `code/source_code/program/cookie/csrf/password` 字段。请仍然遵守各平台服务条款和浏览器脚本规则。

## 目录

```text
backend/cmd/server/main.go          # Gin + GORM 服务入口
backend/internal/                   # 配置、数据库、模型、接口和鉴权
frontend/                           # Vite + Vue 前端
scripts/codeactivityhub-tampermonkey.user.js
data/                               # SQLite 数据目录（首次启动自动创建）
```

## 热重载开发（air）

推荐的开发方式，一条命令搞定前后端：

```bash
cd /Users/starry/Documents/projects/CodeActivityHub
air          # 未安装则先执行：go install github.com/air-verse/air@latest
```

访问 `http://127.0.0.1:2053`。行为说明：

- 改动 `backend/**/*.go` → 自动重新编译并重启；
- 改动 `frontend/index.html`、`frontend/src/**` → 先重建 `frontend/dist`（后端直接服务该目录）再重启；
- 只在前端源文件有真实变更时才跑 `npm run build`，纯后端改动不会被拖慢；
- 编译失败时保留旧进程继续运行，不会掉端口。

只想跑后端、不想碰前端构建，把 `.air.toml` 里的 `pre_cmd` 注释掉即可。

## 本地运行

首次启动或修改前端后：

```bash
cd /Users/starry/Documents/projects/CodeActivityHub/frontend
npm install
npm run build
```

生产模式（访问 `http://127.0.0.1:2053`）：

```bash
cd /Users/starry/Documents/projects/CodeActivityHub
go test ./...
go run ./backend/cmd/server
```

开发模式需要两个终端：

```bash
# 终端 1：后端
go run ./backend/cmd/server

# 终端 2：前端，访问 http://127.0.0.1:5173
cd frontend && npm run dev
```

可通过环境变量调整：

```bash
CODEACTIVITYHUB_ADDR=:2053 \
CODEACTIVITYHUB_DB=./data/codeactivityhub.db \
CODEACTIVITYHUB_FRONTEND_DIST=./frontend/dist \
CODEACTIVITYHUB_HTTP_PROXY=http://127.0.0.1:10808 \
go run ./backend/cmd/server
```

`CODEACTIVITYHUB_HTTP_PROXY`：为所有出站 OJ 请求（同步历史提交、校验账号）指定代理。
国内直连 Codeforces 时通时断，建议配置；未设置时保持 Go 默认行为（尊重 `HTTP_PROXY` 等环境变量）。
出站请求默认带 800ms 同域节流（`CODEACTIVITYHUB_MIN_INTERVAL_MS` 可调），对站点友好。
另：网络超时/断连是瞬时的，只会记为 `warning`（顶部琥珀点），不会挂"连接异常"红条。

## Tampermonkey 配置

登录 CodeActivityHub 后，进入 **系统设置 → 浏览器事件同步**，点「生成新 Token」并立即复制弹出的明文
（明文只显示一次，库里只存哈希）；然后点 Tampermonkey 图标 → 本脚本 →
「设置 CodeActivityHub（Endpoint / Token）」，填入服务地址与 Token，并为公网域名添加 `@connect`。
详细说明见 `scripts/README.md`。

脚本 Token 是**独立于登录会话的长效凭证**：不会 7 天过期，只能调用提交接入接口（其他接口返回 403），
可在设置页按设备逐条轮换或吊销，改密码时自动全部吊销。

## API 摘要

- `POST /api/auth/register`、`POST /api/auth/login`、`GET /api/auth/me`
- `GET /api/stats/overview`、`/heatmap`、`/tags`、`/mistakes`、`/submissions`
- `GET/POST /api/settings`
- `POST /api/ingest/submission`：单条浏览器事件接入（接受长效脚本 Token 或登录会话 Token）
- `POST /api/ingest/submissions`：批量补传
- `GET /api/ingest/events`：查看最近事件审计记录
- `GET /api/ingest/tokens`：列出当前用户的长效脚本 Token（只返回 `token_hint`）
- `POST /api/ingest/tokens`：签发新 Token（明文仅本次返回）
- `POST /api/ingest/tokens/:id/rotate`：轮换，旧值立即失效
- `DELETE /api/ingest/tokens/:id`：吊销
- `POST /api/sync`：用户主动触发的手动历史提交同步（Codeforces、LeetCode、AtCoder 等公开接口）
- `GET /api/problems?platform=…&keyword=…&difficulty=…&tag=…&solved=…`：检索本地题库（关键词/难度/标签/已解决，均走 SQL）
- `POST /api/problems/sync?platform=…`：手动触发该平台题库全量同步（后台执行、边拉边入库，进度在 `/api/problems` 的 `sync` 字段）
- `GET /api/contests?platform=all`：按需读取比赛日历
- `POST /api/verify`：按需验证公开用户账号

所有受保护接口使用 `Authorization: Bearer <session-token>`。生产环境建议为脚本单独创建低权限账号、使用 HTTPS，并定期退出登录/更换 Token。

## 与旧版的差异

- **后端**：FastAPI/Python → Go `Gin + GORM` + SQLite。
- **同步**：取消后台定时轮询；历史数据由用户主动点击后通过公开接口手动同步，提交后的新事件由浏览器监听并增量接入。
- **用户体验**：保留看板、热力图、标签和错题本；设置页新增脚本 Token 与安装说明，轮询配置标记为停用。
- **兼容**：复用已有 `users`、`sessions`、`submissions`、`user_configs` 等表结构，原有数据无需导出。

## 从 Release 部署（Linux 免编译）

推送到 GitHub 后，CI（`.github/workflows/release.yml`）会在每次 push 到 main 和打 `v*` 标签时：
跑后端测试 → 构建前端 → 交叉编译出 **linux/amd64 与 linux/arm64** 的静态二进制
（纯 Go SQLite 驱动，无需 CGO），连同 `frontend/dist` 一起打成 `codeactivityhub-linux-<arch>.tar.gz`；
push main 时产物在 Actions 的 Artifacts 里，打标签时自动挂到 GitHub Release。

手动部署：

```bash
tar -xzf codeactivityhub-linux-amd64.tar.gz -C /opt/codeactivityhub --strip-components=1
cd /opt/codeactivityhub && ./codeactivityhub-server   # 默认 :2053，数据库 data/ 自动创建
```

或用包内的 `codeactivityhub.service` 注册 systemd（`systemctl enable --now codeactivityhub`），
路径/端口可用 `CODEACTIVITYHUB_ADDR` / `CODEACTIVITYHUB_DB` / `CODEACTIVITYHUB_FRONTEND_DIST` 覆盖。
