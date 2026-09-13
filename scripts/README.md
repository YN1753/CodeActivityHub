# CodeActivityHub 浏览器提交同步脚本

`codeactivityhub-tampermonkey.user.js` 跨**洛谷、力扣、AtCoder、Codeforces** 记录刷题提交结果。

设计目标：**可靠记录用户的最终提交结果**，而不是捕获原始 HTTP 请求/响应包。
拿到什么就用什么——能从响应里读判定就读，读不到就去结果页读 DOM，绝不重放请求、不主动轮询平台。

## 安装与配置

1. 启动后端：`air` 或 `go run ./backend/cmd/server`。
2. 登录 CodeActivityHub，进入 **系统设置 → 浏览器事件同步**，点「**生成新 Token**」，
   给这台设备起个名字（如"家里的电脑"），**立即复制弹出的明文**。
   > 明文只在生成/轮换时显示这一次，库里只存哈希，之后无法回显。
3. Tampermonkey 新建脚本，粘贴模板安装（默认地址 `http://127.0.0.1:2053`）。
4. **配置**：点 Tampermonkey 图标 → 本脚本 → 「设置 CodeActivityHub（Endpoint / Token）」，
   填入服务地址和上一步复制的 Token。配置存在脚本管理器里，不用改脚本文件；
   菜单里还有「查看待确认提交」。
5. 公网部署时需在脚本头补一行 `@connect your-domain.example`。
6. 在 OJ 提交一次即可。

### Token 管理

- **独立于登录会话**：不会像登录 token 那样 7 天过期，改密码时会自动全部吊销。
- **只能提交记录**：读不到看板数据（访问其他接口返回 403），也不能管理其它 Token。
- **每台设备一条**：设置页按名称列出，能看到 `token_hint`、创建时间和最后使用时间——
  据此判断哪台设备的脚本还在跑。
- **轮换**：某条 Token 可能泄露时点「轮换」，旧值立即失效，把新的填回脚本即可。
- **吊销**：设备不再使用直接吊销，会话本身不受影响。

## 两类提交机制

| 平台 | 机制 | ① 提交信息 | ② 最终结果 |
|---|---|---|---|
| 力扣 | AJAX（SPA） | `POST /problems/<slug>/submit/` → 请求体取 slug、lang；响应取 `submission_id` | 前端轮询 `GET /submissions/detail/<id>/check/`，`state=SUCCESS` 时取 `status_msg` |
| 洛谷 | AJAX（SPA） | `POST /fe/api/problem/submit/` → 请求体取 pid、lang；响应取 `rid` | 前端轮询 `/record/<rid>` 或 `/fe/api/record/detail` |
| Codeforces | 原生 `<form>` | `submit` 事件 + `FormData` → `submittedProblemCode`、`programTypeId` 的 select 文本 | 跳转 `/contest/<id>/my`、`/problemset/status` 后读 verdict 单元格 |
| AtCoder | 原生 `<form>` | `submit` 事件 + `FormData` → `data.TaskScreenName`、`data.LanguageId` 的 option 文本 | 跳转 `/contests/<id>/submissions/me` 后读结果列短码（AC/WA/TLE…） |

- **绿色类（力扣、洛谷）**：提交与结果在同一页面生命周期内完成，一次搞定。
- **蓝色类（Codeforces、AtCoder）**：必须跨导航两段式。第一段只暂存题号，**不写后端**；
  第二段在结果页读到终态才上报。刷新结果页会重试，直到判题出结果（队列 15 分钟过期）。

## 数据结构

```js
/**
 * 待确认提交记录（暂存在页面会话，跨导航存活）
 * @typedef {Object} PendingSubmission
 * @property {string} id          幂等键 `site:problemId:提交分钟`，直接作为后端 raw_id
 * @property {string} site        codeforces | atcoder | luogu | leetcode
 * @property {string} problemId   平台内唯一题号（CF 的 1A、洛谷的 P1001、力扣的 slug）
 * @property {string} problemTitle
 * @property {string} problemUrl
 * @property {string} language
 * @property {number} submitTime  ms
 * @property {string} remoteId    平台提交号（力扣 submission_id / 洛谷 rid），其余为空
 * @property {number} attempts    已尝试确认次数
 */
```

确认后映射成后端 `POST /api/ingest/submission` 的请求体：
`platform / raw_id / problem_id / problem_title / verdict / code_language / submitted_at / submission_url`。

## Adapter 接口

每个平台一个对象，实现下面 4 个方法（见脚本内 `ADAPTERS`）：

```js
/**
 * @typedef {Object} SubmitCtx   // 提交上下文
 * @property {'ajax'|'form'} kind
 * @property {string} method  @property {string} url
 * @property {*} requestBody  @property {string} responseBody
 * @property {number} status  @property {HTMLFormElement|null} form
 *
 * @typedef {Object} Adapter
 * @property {string} site
 * @property {(host:string)=>boolean} match
 * @property {(ctx:SubmitCtx)=>PendingSubmission|null} detectSubmit               // ① 提交阶段
 * @property {(ctx:SubmitCtx, p:PendingSubmission)=>string|null} resolveFromResponse // ②A 异步响应
 * @property {(path:string)=>boolean} isResultPage                                // ②B 是不是结果页
 * @property {(p:PendingSubmission)=>string|null} resolveFromPage                 // ②B 结果页 DOM
 */
```

新增平台 = 加一个 Adapter 对象，其余逻辑（队列、重试、上报、脱敏）不用动。

## 可靠性约定

- **只认终态**：`In queue` / `Running` / `Judging` / `WJ` 等中间态一律不写入后端
  （错题本规则是"非 AC 即错题"，写半截记录会污染数据）。
- **宁可丢一条**：匹配不到判定就留在队列里等下一次刷新，超时（15 分钟）丢弃。
- **不上传代码正文**：请求体按字段名精确脱敏后截断到 5000 字符，仅用于排错。
- **幂等**：`raw_id` 由「平台 + 题号 + 提交分钟」生成，后端按
  `(user_id, platform, raw_id)` 去重，重复触发不会产生重复记录。

## 已知限制

- 结果页的 DOM 选择器是按公开页面结构推断的（CF 有 Cloudflare、AtCoder 与洛谷要登录，
  无法用命令行核实）。**请用 `DEBUG = true` 在真实站点提交一次**，把控制台
  `[CodeActivityHub]` 开头的输出发回来即可校准。
- 不采集难度与算法标签（页面里拿不到），浏览器上报的记录在看板上 tags/difficulty 为空。
