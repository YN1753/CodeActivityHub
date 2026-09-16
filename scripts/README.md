# CodeActivityHub 浏览器提交同步脚本

`codeactivityhub-tampermonkey.user.js` 跨**洛谷、力扣、AtCoder、Codeforces** 记录刷题提交结果。

设计目标：**可靠记录用户的最终提交结果**，而不是捕获原始 HTTP 请求/响应包。
拿到什么就用什么——能从响应里读判定就读，读不到就去结果页读 DOM，绝不重放请求、不主动轮询平台。

## 安装与配置

1. 启动后端：`air` 或 `go run ./backend/cmd/server`。
2. 登录 CodeActivityHub，进入 **系统设置 → 浏览器事件同步**，点「**生成新 Token**」，
   给这台设备起个名字（如"家里的电脑"），**立即复制弹出的明文**。
   > 明文只在生成/轮换时显示这一次，库里只存哈希，之后无法回显。
3. Tampermonkey 新建脚本，粘贴模板安装。所有可配置项都集中在脚本正文最上方的
   **配置区**（见下表），本机开发的默认值不用改。
4. **配置**：点 Tampermonkey 图标 → 本脚本 → 「设置 CodeActivityHub（Endpoint / Token）」，
   填入服务地址和上一步复制的 Token。配置存在脚本管理器里，不用改脚本文件；
   菜单里还有「查看待确认提交」。
5. 在 OJ 提交一次即可。

### 配置变量（脚本正文最上方）

| 变量 | 默认值 | 含义 |
|---|---|---|
| `FILE_ENDPOINT` | `http://127.0.0.1:2053` | 后端入口地址。搬到服务器后改成 `https://你的域名`（结尾不带 `/`），并在文件头补一行 `@connect` 白名单（见下节） |
| `FILE_TOKEN` | `''`（空） | 上报专用 Token（系统设置 → 浏览器事件同步生成）。建议留空走菜单填写，脚本可放心备份/分享；直接写死也可以，但写过后别分享脚本文件 |
| `DEBUG` | `false` | 排错开关。排查问题或刚搬服务器验证链路时改 `true`，控制台打印 `[CodeActivityHub]` 每一步日志，确认后改回 |
| `PENDING_TTL` | `15 * 60 * 1000` | 待确认记录过期时间（15 分钟），没等到判定就丢弃，一般不用改 |

> **配置优先级**：Tampermonkey 菜单（GM_setValue）> 上述代码常量。菜单里保存过值的话，改常量不会生效，直接在菜单里改即可。

### 搬到服务器（域名下来后三步）

1. **脚本正文**：把顶部配置区的 `FILE_ENDPOINT` 改成 `https://你的域名`（结尾不带 `/`）；
   并在文件头按注释补一行 `// @connect      你的域名` —— GM_xmlhttpRequest 只允许访问
   白名单里的域名，不加的话首次上报会被 Tampermonkey 拦截或弹授权询问。
2. **服务器**：在 Web 端重新生成一条新 Token（系统设置 → 浏览器事件同步）。Token 跟数据库走，
   本机旧 Token 在服务器上无效，上报会 401，脚本会弹「Token 已失效」通知。
3. **菜单**：在「设置 CodeActivityHub（Endpoint / Token）」里填服务器地址和新 Token ——
   菜单优先级高于代码常量，只改常量不改菜单是不生效的。

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
 * @property {string} id          幂等键 `site:problemId:提交秒`（UTC 到秒精度），直接作为后端 raw_id
 * @property {string} site        codeforces | atcoder | luogu | leetcode
 * @property {string} problemId   平台内唯一题号（CF 的 1A、洛谷的 P1001、力扣的 slug）
 * @property {string} problemTitle
 * @property {string} problemUrl
 * @property {string} language
 * @property {number} submitTime  ms
 * @property {string} remoteId    平台提交号（力扣 submission_id / 洛谷 rid），其余为空
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
 * @property {string} url
 * @property {*} requestBody  @property {string} responseBody
 * @property {HTMLFormElement|null} form
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

- **只认终态**：`In queue` / `Running` / `Judging` / `WJ` / `Pretests passed` / `Happy New Year`
  等中间态一律不写入后端（错题本规则是"非 AC 即错题"，写半截记录会污染数据）。
- **上报失败自动重试**：提交终态确认后调用 `POST /api/ingest/submission`，**只有后端返回 2xx
  才从本地队列移除记录**；网络超时 / 5xx / 400 等失败会按 1s → 3s → 8s 递增退避最多重试 3 次，
  重试耗尽才丢弃（DEBUG 日志会记下失败原因与状态码）；401（Token 失效）不重试、直接丢弃并弹通知。
- **不上传代码正文**：上报体只含结构化字段（平台、题号、判定、语言、时间、链接等），
  **绝不发送任何请求体 / 响应体原文**，代码、cookie、token 等敏感内容不会离开浏览器。
- **幂等**：`raw_id` 由「平台 + 题号 + 提交秒（UTC，到秒精度）」生成，后端按
  `(user_id, platform, raw_id)` 去重，重复触发不会产生重复记录。（秒级精度下，同一分钟内的
  第二次提交不会再被「分钟级」去重误吞。）

## 已知限制

- **暂不支持 AcWing**：脚本头部已移除 `acwing.com` 的匹配规则，避免"声明支持却什么都不做"的
  无效加载。如需支持请新增对应 Adapter。
- **洛谷已实测校准（v2.3.0，2026-09-15）**，实测结论：题目页要先点「提交答案」标签才有编辑器；
  编辑器是 CodeMirror 6；记录页的判定是**页面直出**的（「评测状态」旁 `<span class="lcolor--*">`
  里是 `Accepted` / `Unaccepted`），既不走 XHR 也不走 WebSocket——所以脚本在结果页会每 2.5 秒
  重读一次 DOM 直到出终态（约 2.5 分钟后停止）。
- **CF / AtCoder 仍未实测**：DOM 选择器是按公开页面结构推断的（CF 有 Cloudflare、AtCoder 要登录，
  命令行核实不了）。**请用 `DEBUG = true` 在真实站点提交一次**，把控制台 `[CodeActivityHub]`
  开头的输出发回来即可校准。
- **Codeforces 判定新增**：`Idleness limit exceeded`（交互题怠惰超限）已映射为 `ILE`；
  `Pretests passed` / `Happy New Year` 视为非终态，继续等待系统测试出最终结果。
- 上报字段统一强转成字符串：洛谷的 `lang`、CF 的 `programTypeId`、AtCoder 的 `LanguageId`
  都是数字 id，直接透传会让后端反序列化失败返回 400。
- 语言字段存的是平台原始 id（如洛谷 `28`），没有翻译成 `C++14` 之类的名字。
- 不采集难度与算法标签（页面里拿不到），浏览器上报的记录在看板上 tags/difficulty 为空。
