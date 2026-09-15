// ==UserScript==
// @name         CodeActivityHub 提交记录同步
// @namespace    https://github.com/codeactivityhub
// @version      2.3.0
// @description  跨洛谷 / 力扣 / AtCoder / Codeforces 记录刷题提交结果。SPA 站点走 fetch/XHR hook 一次拿全；原生表单站点拆成"提交时记题号 + 结果页读判定"两段。
// @match        https://codeforces.com/*
// @match        https://www.luogu.com.cn/*
// @match        https://leetcode.com/*
// @match        https://leetcode.cn/*
// @match        https://atcoder.jp/*
// @noframes
// @grant        GM_xmlhttpRequest
// @grant        GM_setValue
// @grant        GM_getValue
// @grant        GM_registerMenuCommand
// @grant        GM_notification
// @connect      localhost
// @connect      127.0.0.1
// 【部署到服务器时】在下面加一行白名单：// @connect    你的域名
// 必须与脚本正文配置区 FILE_ENDPOINT 的域名一致；不加的话首次上报会被
// Tampermonkey 拦截或弹出授权询问。
// ==/UserScript==

/**
 * 设计要点
 * --------
 * 目标：可靠记录用户的**最终提交结果**，而不是捕获原始 HTTP 请求/响应包。
 *
 * 两类站点：
 *   A. SPA（力扣、洛谷）—— 提交走 AJAX，请求与结果都在同一页面生命周期内，
 *      用 fetch/XHR hook 一次拿全。
 *   B. 原生 <form>（Codeforces、AtCoder）—— submit 后是整页导航，JS 拿不到响应。
 *      拆两段：① submit 事件读 FormData 拿题号并暂存；② 结果页读判定后合并上报。
 *
 * 统一数据结构见 PendingSubmission；每个平台一个 Adapter，实现 4 个方法。
 */

(() => {
  'use strict';

  // ===================== 配置区（唯一需要改的地方） =====================
  //
  // ① 服务器地址 ENDPOINT —— CodeActivityHub 后端入口，提交记录上报到这里。
  //      本机开发：'http://127.0.0.1:2053'
  //      搬到服务器后：改成你的域名，如 'https://oj.example.com'（结尾不要带 /），
  //      并在文件头部按注释补一行 @connect 白名单。
  //      注意：这只是"兜底默认值"，Tampermonkey 菜单里保存过的值会优先于它。
  const FILE_ENDPOINT = 'http://127.0.0.1:2053';

  // ② 脚本 Token —— 上报提交专用的令牌，与网站登录会话互不相干。
  //      获取方式：登录 CodeActivityHub → 系统设置 → 浏览器事件同步 → 生成新 Token，
  //      明文只在生成时显示一次（库里只存哈希，之后无法回看）。
  //      建议保持 '' 空，通过 Tampermonkey 菜单填写，这样脚本可以放心备份/分享；
  //      直接把 Token 写在这里也可以，但写过后别再把脚本文件发给别人。
  //      同样是兜底值，菜单配置优先。
  const FILE_TOKEN = '';

  // ③ 排错开关 —— 排查问题或刚搬到服务器验证链路时改成 true，
  //      控制台会打印 [CodeActivityHub] 开头的每一步日志；确认正常后改回 false。
  const DEBUG = false;

  // ④ 待确认记录过期时间 —— 提交后还没等到判定（排队/判题中）时，记录暂存在页面
  //      会话里等下次刷新确认，超过这个时长就丢弃，避免脏数据。一般不用改。
  const PENDING_TTL = 15 * 60 * 1000;  // 15 分钟

  // ---- 以下为内部存储键名与取值逻辑，有历史数据后不要改 ----
  const KEY_ENDPOINT = 'codeactivityhub_endpoint';
  const KEY_TOKEN = 'codeactivityhub_token';
  const KEY_QUEUE = 'codeactivityhub_queue';

  const getEndpoint = () => String(GM_getValue(KEY_ENDPOINT) || FILE_ENDPOINT || '').trim().replace(/\/+$/, '');
  const getToken = () => String(GM_getValue(KEY_TOKEN) || FILE_TOKEN || '').trim();
  const isPlaceholder = (v) => !v || v.startsWith('PASTE_');
  // ====================================================================

  const log = (...args) => DEBUG && console.log('[CodeActivityHub]', ...args);

  let lastNotifyAt = 0;
  const notifyOnce = (text) => {
    const now = Date.now();
    if (now - lastNotifyAt < 30000) return;
    lastNotifyAt = now;
    try { GM_notification({ title: 'CodeActivityHub', text, timeout: 6000 }); } catch (e) { /* 无通知权限 */ }
  };

  try {
    GM_registerMenuCommand('设置 CodeActivityHub（Endpoint / Token）', () => {
      const endpoint = prompt('CodeActivityHub 地址（结尾不要带 /）：', getEndpoint() || FILE_ENDPOINT);
      if (endpoint === null) return;
      const token = prompt('脚本 Token（登录 CodeActivityHub 后打开 /api/ingest/token 复制）：', getToken());
      if (token === null) return;
      GM_setValue(KEY_ENDPOINT, endpoint.trim() || FILE_ENDPOINT);
      GM_setValue(KEY_TOKEN, token.trim());
      notifyOnce('已保存，刷新 OJ 页面后生效。');
    });
    GM_registerMenuCommand('查看 CodeActivityHub 待确认提交', () => {
      const q = queue.read();
      alert(q.length ? q.map(p => `${p.site} / ${p.problemId} / ${new Date(p.submitTime).toLocaleString()}`).join('\n')
        : '暂无待确认的提交。');
    });
  } catch (e) { /* 个别脚本管理器不支持菜单 */ }

  const host = location.hostname;

  // ===================== 通用工具 =====================

  const text = (value) => {
    if (value == null) return '';
    if (typeof value === 'string') return value;
    try { return JSON.stringify(value); } catch (e) { return String(value); }
  };
  const parseJSON = (raw) => { try { return JSON.parse(raw); } catch (e) { return null; } };
  // 支持 "data.rid" 这类嵌套取值；但字段名本身可能就带点（AtCoder 的 data.TaskScreenName），
  // 所以优先按字面 key 取，取不到再按路径拆。
  const pick = (obj, ...keys) => {
    for (const k of keys) {
      const direct = obj == null ? undefined : obj[k];
      const nested = String(k).split('.').reduce((acc, part) => (acc == null ? undefined : acc[part]), obj);
      const v = (direct !== undefined && direct !== null && direct !== '') ? direct : nested;
      if (v !== undefined && v !== null && v !== '') return v;
    }
    return '';
  };

  // 判定归一化：统一成后端认识的档位。返回 null 表示"还没出终态"。
  const TERMINAL = {
    AC: 'AC', WA: 'WA', TLE: 'TLE', MLE: 'MLE', RE: 'RE', CE: 'CE',
    OLE: 'OLE', IE: 'IE', UKE: 'UKE', SKIPPED: 'SKIPPED', ERROR: 'ERROR',
  };
  const NON_TERMINAL_RE = /in\s*queue|running|judging|waiting|pending|testing|compiling|\bwj\b|start/i;

  function classify(text) {
    const s = String(text || '').trim().toUpperCase();
    if (!s) return null;
    if (TERMINAL[s]) return TERMINAL[s];
    const lower = s.toLowerCase();
    // Codeforces 非终态：预测试通过 / 新年彩蛋等，应继续等最终判定（别被误判成终态）
    if (/pretests?\s+passed|happy\s*new\s*year/.test(lower)) return null;
    if (NON_TERMINAL_RE.test(lower)) return null;          // 还在判，继续等
    if (/idleness\s*limit\s*exceeded/.test(lower)) return 'ILE';  // CF 交互题「怠惰超限」
    if (/unaccepted|未通过/.test(lower)) return 'WA';       // 洛谷整题未通过显示 Unaccepted
    if (/\baccepted\b|答案正确|通过/.test(lower)) return 'AC';
    if (/\bwrong[\s_-]?answer\b|答案错误/.test(lower)) return 'WA';
    if (/\btime[\s_-]?limit\b|时间超限|时间限制|\btle\b/.test(lower)) return 'TLE';
    if (/\bmemory[\s_-]?limit\b|内存超限|内存限制|\bmle\b/.test(lower)) return 'MLE';
    if (/\bcompil\w*[\s_-]?error\b|编译错误/.test(lower)) return 'CE';
    if (/\bruntime[\s_-]?error\b|运行错误|\bre\b/.test(lower)) return 'RE';
    if (/输出超限|ole\b/.test(lower)) return 'OLE';
    return null;
  }

  // 只上报上面那段「结构化字段」，绝不发送任何请求体 / 响应体原文，
  // 所以代码正文、cookie、token 等敏感内容本来就不会离开浏览器。

  // ===================== 待确认提交队列 =====================

  /**
   * @typedef {Object} PendingSubmission
   * @property {string} id          幂等键 site:problemId:提交秒（UTC，到秒精度），直接作为后端 raw_id
   * @property {string} site        平台标识
   * @property {string} problemId   平台内唯一题号
   * @property {string} problemTitle
   * @property {string} problemUrl
   * @property {string} language
   * @property {number} submitTime  ms
   * @property {string} remoteId    平台提交号（力扣 submission_id / 洛谷 rid），可为空
   * @property {number} attempts    已尝试确认次数
   */
  const queue = {
    read() {
      try {
        const raw = sessionStorage.getItem(KEY_QUEUE);
        const list = raw ? JSON.parse(raw) : [];
        return (Array.isArray(list) ? list : []).filter(p => p && Date.now() - p.submitTime <= PENDING_TTL);
      } catch (e) { return []; }
    },
    write(list) { try { sessionStorage.setItem(KEY_QUEUE, JSON.stringify(list)); } catch (e) { /* 隐私模式 */ } },
    add(item) {
      const list = queue.read().filter(p => p.id !== item.id);
      list.push(item);
      queue.write(list);
      log('新增待确认提交', item);
    },
    remove(id) { queue.write(queue.read().filter(p => p.id !== id)); },
    bump(id) {
      const list = queue.read();
      const hit = list.find(p => p.id === id);
      if (hit) { hit.attempts = (hit.attempts || 0) + 1; queue.write(list); }
    },
  };

  function makePending(site, problemId, extra) {
    const now = Date.now();
    const second = new Date(now).toISOString().slice(0, 19);
    const pid = String(problemId || '').trim();
    return Object.assign({
      id: `${site}:${pid}:${second}`,
      site, problemId: pid,
      problemTitle: pid, problemUrl: location.href, language: '',
      submitTime: now, remoteId: '', attempts: 0,
    }, extra || {});
  }

  // ===================== Adapter 接口 =====================

  /**
   * @typedef {Object} SubmitCtx
   * @property {'ajax'|'form'} kind
   * @property {string} method
   * @property {string} url
   * @property {*}       requestBody
   * @property {string}  responseBody
   * @property {number}  status
   * @property {HTMLFormElement|null} form
   *
   * @typedef {Object} Adapter
   * @property {string} site
   * @property {(host:string)=>boolean} match
   * @property {(ctx:SubmitCtx)=>PendingSubmission|null} detectSubmit         // ① 提交阶段
   * @property {(ctx:SubmitCtx, p:PendingSubmission)=>string|null} resolveFromResponse // ②A 异步响应
   * @property {(path:string)=>boolean} isResultPage                          // ②B 结果页判定
   * @property {(p:PendingSubmission)=>string|null} resolveFromPage           // ②B 结果页 DOM
   */

  function formFields(form) {
    const out = {};
    try { new FormData(form).forEach((v, k) => { out[String(k)] = String(v == null ? '' : v); }); } catch (e) { /* ignore */ }
    return out;
  }
  function selectText(form, namePattern) {
    try {
      const selects = Array.from(form.querySelectorAll('select[name]'));
      const hit = selects.find(s => namePattern.test(String(s.name || '')));
      const opt = hit && hit.selectedOptions && hit.selectedOptions[0];
      if (opt && opt.textContent) return opt.textContent.trim();
    } catch (e) { /* ignore */ }
    return '';
  }
  // 在结果页表格里找"行文本含题号"的行，再从行里挑出判定单元格
  function scanRowsForVerdict(problemId, cellFilter) {
    const needle = String(problemId || '').toLowerCase();
    if (!needle) return null;
    let rows = [];
    try { rows = Array.from(document.querySelectorAll('tr[data-submission-id], table tr')); } catch (e) { return null; }
    for (const row of rows) {
      const rowText = String(row.textContent || '').toLowerCase();
      if (!rowText.includes(needle)) continue;
      const cells = Array.from(row.querySelectorAll('td, th'));
      // 优先匹配"看起来像判定"的单元格（class / 属性 / 已知的短码）
      for (const cell of cells) {
        if (cellFilter && cellFilter(cell)) {
          const v = classify(cell.textContent);
          if (v) return v;
        }
      }
      const fallback = classify(row.textContent);
      if (fallback && !NON_TERMINAL_RE.test(rowText)) return fallback;
    }
    return null;
  }

  // ---------- Codeforces：原生 form ----------
  const codeforces = {
    site: 'codeforces',
    match: (h) => h.includes('codeforces'),
    detectSubmit(ctx) {
      if (ctx.kind !== 'form' || !/\/submit/.test(ctx.url)) return null;
      const f = formFields(ctx.form);
      const pid = pick(f, 'submittedProblemCode', 'problemCode');
      if (!pid) { log('CF：表单里没找到题号'); return null; }
      return makePending('codeforces', pid.toUpperCase(), {
        language: selectText(ctx.form, /programtypeid|programType/i),
        problemUrl: location.href,
      });
    },
    resolveFromResponse: () => null,      // CF 没有可用于轮询的异步接口
    isResultPage: (path) => /(?:\/problemset|\/contest\/\d+|\/gym\/\d+)\/(?:status|my)(?:\/|$)|^\/submissions(?:\/|$)/.test(path),
    resolveFromPage(p) {
      return scanRowsForVerdict(p.problemId, (cell) => {
        const cls = String(cell.className || '').toLowerCase();
        if (/verdict|status/.test(cls)) return true;
        try { return !!cell.getAttribute('submissionVerdict'); } catch (e) { return false; }
      });
    },
  };

  // ---------- AtCoder：原生 form ----------
  const atcoder = {
    site: 'atcoder',
    match: (h) => h.includes('atcoder'),
    detectSubmit(ctx) {
      if (ctx.kind !== 'form' || !/\/contests\/[^/]+\/submit/.test(ctx.url)) return null;
      const f = formFields(ctx.form);
      const pid = pick(f, 'data.TaskScreenName', 'taskScreenName');
      if (!pid) { log('AtCoder：表单里没找到 taskScreenName'); return null; }
      return makePending('atcoder', pid, {
        language: selectText(ctx.form, /languageid|LanguageId/i),
        problemUrl: location.href,
      });
    },
    resolveFromResponse: () => null,
    isResultPage: (path) => /\/contests\/[^/]+\/submissions(?:\/|$)/.test(path),
    resolveFromPage(p) {
      // AtCoder 的结果列就是 AC / WA / TLE 这类短码，直接按整单元格文本匹配最稳
      return scanRowsForVerdict(p.problemId, (cell) => {
        const t = String(cell.textContent || '').trim().toUpperCase();
        return /^(AC|WA|TLE|MLE|RE|CE|OLE|IE|WJ|WRONG ANSWER)$/.test(t);
      });
    },
  };

  // ---------- 洛谷：AJAX ----------
  // 洛谷 _contentOnly 接口的状态是数字码，先映射成统一档位；
  // 0/1（等待/评测中）刻意不映射，保持"未终态"继续等待。
  const LUOGU_STATUS = { 12: 'AC', 6: 'WA', 5: 'TLE', 4: 'MLE', 7: 'RE', 2: 'CE', 3: 'OLE', 11: 'UKE', 14: 'WA' };
  const luogu = {
    site: 'luogu',
    match: (h) => h.includes('luogu'),
    detectSubmit(ctx) {
      if (ctx.kind !== 'ajax' || !/\/fe\/api\/problem\/submit|\/api\/problem\/submit/.test(ctx.url)) return null;
      const req = parseJSON(text(ctx.requestBody)) || {};
      // 新版提交接口 pid 在 URL 路径里（/fe/api/problem/submit/<pid>），旧版在请求体里
      const pid = pick(req, 'pid', 'problemId', 'problem_id')
        || (String(ctx.url).match(/\/submit\/([A-Za-z0-9_-]+)/) || [])[1] || '';
      if (!pid) { log('洛谷：请求体和 URL 里都没找到 pid'); return null; }
      const res = parseJSON(ctx.responseBody) || {};
      return makePending('luogu', pid.toUpperCase(), {
        language: pick(req, 'lang', 'language'),
        remoteId: String(pick(res, 'rid', 'data.rid', 'currentData.rid', 'sid') || ''),
        problemUrl: `https://www.luogu.com.cn/problem/${pid}`,
      });
    },
    // 洛谷提交后跳到 /record/<rid> 并加载记录 JSON（currentData.record.status）
    resolveFromResponse(ctx, p) {
      if (!/\/record\/|record\/detail/.test(ctx.url)) return null;
      const res = parseJSON(ctx.responseBody) || {};
      // 注意顺序：顶层可能存在 envelope 的 status=200，必须先取记录状态的深路径
      const raw = pick(res, 'currentData.record.status', 'data.record.status', 'record.status', 'data.detail.status');
      const direct = classify(String(raw)) || LUOGU_STATUS[String(raw).trim()];
      if (direct) return direct;
      const record = (res.currentData && res.currentData.record) || (res.data && res.data.detail) || null;
      return record ? classify(record.judgeResult || record.verdict) || LUOGU_STATUS[String(record.status).trim()] : null;
    },
    isResultPage: (path) => /^\/record\/(?:list|\d+)/.test(path),
    // 记录页判定是页面直出的（_<span class="lcolor--*">Unaccepted</span>_ 跟在「评测状态」后面），
    // 走 XHR 拿不到，必须读 DOM
    resolveFromPage(p) {
      const label = Array.from(document.querySelectorAll('span, div, td, th'))
        .find((el) => !el.children.length && /^评测状态$/.test(String(el.textContent || '').trim()));
      if (label) {
        const scope = label.parentElement || label;
        const cells = Array.from(scope.querySelectorAll('span, div, td'));
        for (const el of cells) {
          const v = classify(String(el.textContent || '').trim());
          if (v) return v;
        }
        const whole = classify(String(scope.innerText || '').replace(/^\s*评测状态\s*/, ''));
        if (whole) return whole;
      }
      return scanRowsForVerdict(p.problemId, null);
    },
  };

  // ---------- 力扣：AJAX ----------
  const leetcode = {
    site: 'leetcode',
    match: (h) => h.includes('leetcode'),
    detectSubmit(ctx) {
      if (ctx.kind !== 'ajax' || !/\/problems\/[^/]+\/submit\/?$/.test(ctx.url)) return null;
      const req = parseJSON(text(ctx.requestBody)) || {};
      const m = ctx.url.match(/\/problems\/([^/?#]+)/);
      const slug = m ? m[1] : pick(req, 'questionSlug', 'titleSlug');
      if (!slug) { log('力扣：没能从 URL 取到 slug'); return null; }
      const res = parseJSON(ctx.responseBody) || {};
      return makePending('leetcode', slug, {
        language: pick(req, 'lang', 'language'),
        remoteId: String(pick(res, 'submission_id', 'submissionId') || ''),
        problemTitle: pick(req, 'titleSlug') || slug,
        problemUrl: `${location.origin}/problems/${slug}/`,
      });
    },
    // 力扣提交后前端轮询 check 接口，state=SUCCESS 时才有最终判定
    resolveFromResponse(ctx, p) {
      if (!/\/submissions\/detail\/\d+\/check\//.test(ctx.url)) return null;
      const res = parseJSON(ctx.responseBody) || {};
      if (String(pick(res, 'state')).toUpperCase() !== 'SUCCESS') return null;
      return classify(pick(res, 'status_msg', 'statusMsg', 'status'));
    },
    isResultPage: (path) => /^\/submissions\//.test(path),
    resolveFromPage(p) { return scanRowsForVerdict(p.problemId, null); },
  };

  const ADAPTERS = [codeforces, atcoder, luogu, leetcode];
  const adapter = ADAPTERS.find(a => a.match(host)) || null;
  if (!adapter) return;

  if (isPlaceholder(getToken())) {
    notifyOnce('尚未配置 Token：点 Tampermonkey 图标 → 本脚本 → 设置 CodeActivityHub（Endpoint / Token）');
    return;
  }

  // ===================== 上报 =====================

  // 上报失败重试：最多重试 REPORT_RETRIES 次，间隔递增 1s / 3s / 8s。
  // 只有确认后端返回 2xx 才从队列移除；401（Token 失效）不重试直接丢弃；
  // 其余失败（网络超时 / 5xx / 400 等）退避后重试，耗尽后再丢弃，并在 DEBUG 日志留因。
  const REPORT_RETRIES = 3;
  const REPORT_BACKOFF = [1000, 3000, 8000];

  function report(pending, verdict, attempt) {
    attempt = attempt || 1;
    // 后端这些字段都是字符串：洛谷的 lang / CF 的 programTypeId / AtCoder 的 LanguageId
    // 都是数字 id，直接透传数字会让后端反序列化失败返回 400，所以一律强转。
    const payload = {
      platform: String(pending.site || ''),
      raw_id: String(pending.id || ''),
      problem_id: String(pending.problemId || ''),
      problem_title: String(pending.problemTitle || pending.problemId || ''),
      verdict: String(verdict || ''),
      code_language: String(pending.language || ''),
      submitted_at: new Date(pending.submitTime).toISOString(),
      submission_url: String(pending.problemUrl || ''),
      source: 'tampermonkey',
      extra_data: { remote_id: String(pending.remoteId || '') },
    };
    const giveup = (status, msg) => {
      if (status === 401) {
        notifyOnce('Token 已失效，请重新获取并更新脚本配置');
        queue.remove(pending.id);
        return;
      }
      if (attempt >= 1 + REPORT_RETRIES) {
        log('上报重试耗尽，丢弃记录', pending.id, verdict, 'status=' + status, 'err=' + msg);
        queue.remove(pending.id);
        return;
      }
      const delay = REPORT_BACKOFF[attempt - 1] || 8000;
      log('上报失败，将重试', attempt + 1, '/', 1 + REPORT_RETRIES, 'status=' + status, 'err=' + msg, 'delay(ms)=' + delay);
      setTimeout(() => report(pending, verdict, attempt + 1), delay);
    };
    GM_xmlhttpRequest({
      method: 'POST',
      url: `${getEndpoint()}/api/ingest/submission`,
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${getToken()}` },
      data: JSON.stringify(payload),
      timeout: 8000,
      onload: (r) => {
        if (r.status >= 200 && r.status < 300) {
          queue.remove(pending.id);
          log('已确认并上报', pending.id, verdict);
          return;
        }
        log('上报返回非 2xx', r.status, payload);
        giveup(r.status, null);
      },
      onerror: () => { log('上报网络错误', pending.id, verdict); giveup(null, 'network error'); },
      ontimeout: () => { log('上报超时', pending.id, verdict); giveup(null, 'timeout'); },
    });
  }

  // 尝试把某条待确认记录变成最终结果
  function tryResolve(pending, ctxFromResponse) {
    let verdict = null;
    // ②A 同一页面内的异步响应（SPA 轮询）
    if (!verdict && ctxFromResponse && adapter.resolveFromResponse) {
      verdict = adapter.resolveFromResponse(ctxFromResponse, pending);
    }
    // ②B 结果页 DOM
    if (!verdict && adapter.isResultPage(location.pathname) && adapter.resolveFromPage) {
      verdict = adapter.resolveFromPage(pending);
    }
    if (!verdict) {
      queue.bump(pending.id);
      log('仍未出终态，继续等待', pending.id);
      return false;
    }
    // 成功才在 report 内部移除；失败会重试，不丢记录。
    report(pending, verdict);
    return true;
  }

  function resolveAll(ctxFromResponse) {
    const list = queue.read().filter(p => p.site === adapter.site);
    for (const pending of list) tryResolve(pending, ctxFromResponse);
  }

  // ===================== 捕获层 =====================

  const WRITE_METHODS = new Set(['POST', 'PUT', 'PATCH']);

  function onAjax(ctx) {
    // 先看看是不是一次新提交
    const pending = adapter.detectSubmit(ctx);
    if (pending) queue.add(pending);
    // 再看看这条响应能不能确认之前的待确认记录
    resolveAll(ctx);
  }

  async function captureFetch(input, init) {
    const url = String((typeof input === 'string' ? input : input && input.url) || '');
    const method = String((init && init.method) || 'GET').toUpperCase();
    const requestBody = init && init.body;
    const response = await window.__codeactivityhubOriginalFetch(input, init);
    try {
      const responseBody = await response.clone().text();
      if (WRITE_METHODS.has(method) || /submit|check|record/.test(url)) {
        onAjax({ kind: 'ajax', method, url, requestBody, responseBody, status: response.status, form: null });
      }
    } catch (e) { log('fetch capture failed', e); }
    return response;
  }

  window.__codeactivityhubOriginalFetch = window.fetch;
  window.fetch = captureFetch;

  const originalOpen = XMLHttpRequest.prototype.open;
  const originalSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cahURL = url;
    this.__cahMethod = method;
    return originalOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function (body) {
    if (WRITE_METHODS.has(String(this.__cahMethod).toUpperCase()) || /submit|check|record/.test(String(this.__cahURL || ''))) {
      // once: true 避免同一 XHR 对象重复 send 时重复触发上报
      this.addEventListener('load', () => {
        onAjax({
          kind: 'ajax', method: String(this.__cahMethod || 'GET').toUpperCase(),
          url: String(this.__cahURL || ''), requestBody: body,
          responseBody: this.responseText, status: this.status, form: null,
        });
      }, { once: true });
    }
    return originalSend.apply(this, arguments);
  };

  document.addEventListener('submit', (ev) => {
    const form = ev.target;
    if (!form || String(form.tagName).toUpperCase() !== 'FORM') return;
    const action = form.getAttribute('action') || location.href;
    const pending = adapter.detectSubmit({ kind: 'form', method: 'POST', url: action, requestBody: '', responseBody: '', status: 0, form });
    if (pending) queue.add(pending);
  }, true);

  // 结果页轮询：SPA 站点进入结果页后 DOM 才渲染出判定，而 XHR 钩子可能压根拿不到
  // 判定数据（洛谷已改成页面直出）。所以到了结果页就定时再读一次 DOM，直到出终态。
  let resultPollTimer = null;
  function startResultPolling() {
    if (resultPollTimer) return;
    let tries = 0;
    resultPollTimer = setInterval(() => {
      tries += 1;
      const pending = queue.read().filter((p) => p.site === adapter.site);
      // 队列空了（都确认完）或约 2.5 分钟后收手，不无限占位
      if (!pending.length || tries > 60) {
        clearInterval(resultPollTimer);
        resultPollTimer = null;
        return;
      }
      if (!adapter.isResultPage(location.pathname)) return;
      resolveAll(null);
    }, 2500);
  }

  // SPA 路由变化（history API / 前进后退）同样要触发确认
  const wrapHistory = (fn) => function (...args) {
    const ret = fn.apply(this, args);
    setTimeout(() => {
      if (adapter.isResultPage(location.pathname)) { resolveAll(null); startResultPolling(); }
    }, 400);
    return ret;
  };
  try {
    history.pushState = wrapHistory(history.pushState);
    history.replaceState = wrapHistory(history.replaceState);
    window.addEventListener('popstate', () => {
      if (adapter.isResultPage(location.pathname)) { resolveAll(null); startResultPolling(); }
    });
  } catch (e) { /* 个别站点不允许改写 history */ }

  // 页面一进来就先试一次：结果页会一直轮询，直到判题出终态
  resolveAll(null);
  if (adapter.isResultPage(location.pathname)) startResultPolling();
  log('已就绪', adapter.site, location.pathname);
})();
