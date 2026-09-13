// ==UserScript==
// @name         CodeActivityHub 提交记录同步
// @namespace    https://github.com/codeactivityhub
// @version      2.0.0
// @description  跨洛谷 / 力扣 / AtCoder / Codeforces 记录刷题提交结果。SPA 站点走 fetch/XHR hook 一次拿全；原生表单站点拆成"提交时记题号 + 结果页读判定"两段。
// @match        https://codeforces.com/*
// @match        https://www.luogu.com.cn/*
// @match        https://leetcode.com/*
// @match        https://leetcode.cn/*
// @match        https://www.acwing.com/*
// @match        https://atcoder.jp/*
// @grant        GM_xmlhttpRequest
// @grant        GM_setValue
// @grant        GM_getValue
// @grant        GM_registerMenuCommand
// @grant        GM_notification
// @connect      localhost
// @connect      127.0.0.1
// @connect      codeactivityhub.example.com
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

  // ===================== 配置 =====================
  const FILE_ENDPOINT = 'http://127.0.0.1:2053';
  const FILE_TOKEN = '';
  const DEBUG = false;                 // 排错时改 true，控制台会打印每一步
  const PENDING_TTL = 15 * 60 * 1000;  // 待确认记录 15 分钟未确认就丢弃

  const KEY_ENDPOINT = 'codeactivityhub_endpoint';
  const KEY_TOKEN = 'codeactivityhub_token';
  const KEY_QUEUE = 'codeactivityhub_queue';

  const getEndpoint = () => String(GM_getValue(KEY_ENDPOINT) || FILE_ENDPOINT || '').trim().replace(/\/+$/, '');
  const getToken = () => String(GM_getValue(KEY_TOKEN) || FILE_TOKEN || '').trim();
  const isPlaceholder = (v) => !v || v.startsWith('PASTE_');
  // ================================================

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
      const endpoint = prompt('CodeActivityHub 地址（结尾不要带 /）：', getEndpoint() || 'http://127.0.0.1:2053');
      if (endpoint === null) return;
      const token = prompt('脚本 Token（登录 CodeActivityHub 后打开 /api/ingest/token 复制）：', getToken());
      if (token === null) return;
      GM_setValue(KEY_ENDPOINT, endpoint.trim() || 'http://127.0.0.1:2053');
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
    if (NON_TERMINAL_RE.test(lower)) return null;          // 还在判，继续等
    if (/\baccepted\b|答案正确|通过/.test(lower)) return 'AC';
    if (/\bwrong[\s_-]?answer\b|答案错误/.test(lower)) return 'WA';
    if (/\btime[\s_-]?limit\b|时间超限|时间限制|\btle\b/.test(lower)) return 'TLE';
    if (/\bmemory[\s_-]?limit\b|内存超限|内存限制|\bmle\b/.test(lower)) return 'MLE';
    if (/\bcompil\w*[\s_-]?error\b|编译错误/.test(lower)) return 'CE';
    if (/\bruntime[\s_-]?error\b|运行错误|\bre\b/.test(lower)) return 'RE';
    if (/输出超限|ole\b/.test(lower)) return 'OLE';
    return null;
  }

  // 只上报必要的字段；代码正文、cookie 一律不进请求体。
  const SENSITIVE_KEYS = new Set([
    'code', 'source', 'sourcecode', 'source_code', 'typedcode', 'typed_code', 'sourcecode',
    'program', 'cookie', 'csrf', 'csrf_token', 'csrfmiddlewaretoken',
    'password', 'passwd', 'token', '_token', 'sessionid',
  ]);
  const redactObject = (val) => {
    if (!val || typeof val !== 'object') return val;
    if (Array.isArray(val)) return val.map(redactObject);
    const out = {};
    for (const [k, v] of Object.entries(val)) out[k] = SENSITIVE_KEYS.has(String(k).toLowerCase()) ? '[redacted]' : redactObject(v);
    return out;
  };
  const redactQuery = (raw) => raw.split('&').map(part => {
    const i = part.indexOf('=');
    if (i < 0) return part;
    return SENSITIVE_KEYS.has(part.slice(0, i).trim().toLowerCase()) ? part.slice(0, i) + '=[redacted]' : part;
  }).join('&');
  const safeBody = (body) => {
    const raw = text(body).slice(0, 5000);
    if (!raw) return '';
    const parsed = parseJSON(raw);
    if (parsed && typeof parsed === 'object') return JSON.stringify(redactObject(parsed)).slice(0, 5000);
    return raw.includes('=') ? redactQuery(raw) : raw;
  };

  // ===================== 待确认提交队列 =====================

  /**
   * @typedef {Object} PendingSubmission
   * @property {string} id          幂等键 site:problemId:提交分钟，直接作为后端 raw_id
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
    const minute = new Date(now).toISOString().slice(0, 16);
    const pid = String(problemId || '').trim();
    return Object.assign({
      id: `${site}:${pid}:${minute}`,
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
  const luogu = {
    site: 'luogu',
    match: (h) => h.includes('luogu'),
    detectSubmit(ctx) {
      if (ctx.kind !== 'ajax' || !/\/fe\/api\/problem\/submit|\/api\/problem\/submit/.test(ctx.url)) return null;
      const req = parseJSON(text(ctx.requestBody)) || {};
      const pid = pick(req, 'pid', 'problemId', 'problem_id');
      if (!pid) { log('洛谷：请求体里没找到 pid'); return null; }
      const res = parseJSON(ctx.responseBody) || {};
      return makePending('luogu', pid.toUpperCase(), {
        language: pick(req, 'lang', 'language'),
        remoteId: String(pick(res, 'rid', 'data.rid') || ''),
        problemUrl: `https://www.luogu.com.cn/problem/${pid}`,
      });
    },
    // 洛谷是 SPA，提交后前端会自己轮询记录详情，这里跟着读
    resolveFromResponse(ctx, p) {
      if (!/\/record\/|record\/detail/.test(ctx.url)) return null;
      const res = parseJSON(ctx.responseBody) || {};
      const raw = pick(res, 'status', 'data.status', 'data.detail.status', 'data.record.status');
      const direct = classify(String(raw));
      if (direct) return direct;
      const detail = res.data && res.data.detail ? res.data.detail : null;
      return detail ? classify(detail.judgeResult || detail.verdict || detail.status) : null;
    },
    isResultPage: (path) => /^\/record\/(?:list|\d+)/.test(path),
    resolveFromPage(p) { return scanRowsForVerdict(p.problemId, null); },
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
        problemUrl: `https://leetcode.com/problems/${slug}/`,
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

  function report(pending, verdict) {
    const payload = {
      platform: pending.site,
      raw_id: pending.id,
      problem_id: pending.problemId,
      problem_title: pending.problemTitle || pending.problemId,
      verdict,
      code_language: pending.language || '',
      submitted_at: new Date(pending.submitTime).toISOString(),
      submission_url: pending.problemUrl || '',
      source: 'tampermonkey',
      extra_data: { remote_id: pending.remoteId || '' },
    };
    GM_xmlhttpRequest({
      method: 'POST',
      url: `${getEndpoint()}/api/ingest/submission`,
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${getToken()}` },
      data: JSON.stringify(payload),
      timeout: 8000,
      onload: (r) => {
        log('上报完成', r.status, payload);
        if (r.status >= 400) {
          notifyOnce(r.status === 401 ? 'Token 已失效，请重新获取并更新脚本配置' : `上报失败（HTTP ${r.status}）`);
        }
      },
      onerror: () => notifyOnce('无法连接 CodeActivityHub，请确认后端已启动'),
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
    report(pending, verdict);
    queue.remove(pending.id);
    log('已确认并上报', pending.id, verdict);
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
      this.addEventListener('load', () => {
        onAjax({
          kind: 'ajax', method: String(this.__cahMethod || 'GET').toUpperCase(),
          url: String(this.__cahURL || ''), requestBody: body,
          responseBody: this.responseText, status: this.status, form: null,
        });
      });
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

  // 页面一进来就先试一次：结果页刷新几次，直到判题出终态
  resolveAll(null);
  log('已就绪', adapter.site, location.pathname);
})();
