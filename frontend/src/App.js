import { createApp, ref, onMounted, computed, nextTick, watch } from 'vue';
import * as echarts from 'echarts';
import './style.css';

// --- 各处筛选下拉的静态选项 ---
const PROBLEM_PLATFORM_OPTIONS = [
  { value: "codeforces", label: "Codeforces" },
  { value: "leetcode", label: "LeetCode" },
  { value: "atcoder", label: "AtCoder" },
  { value: "luogu", label: "洛谷" }
];
const SUB_PLATFORM_OPTIONS = [
  { value: "all", label: "全部平台" },
  { value: "codeforces", label: "Codeforces" },
  { value: "leetcode", label: "LeetCode" },
  { value: "atcoder", label: "AtCoder" },
  { value: "luogu", label: "洛谷" },
  { value: "acwing", label: "AcWing" }
];
const VERDICT_OPTIONS = [
  { value: "all", label: "全部状态" },
  { value: "AC", label: "已通过" },
  { value: "WA", label: "未通过" }
];
// 错题集平台筛选 chips 的短标签（与热力图 chips 一致）
const MISTAKE_PLATFORM_SHORT = { codeforces: "CF", atcoder: "AT", luogu: "LG", leetcode: "LC", acwing: "AW" };
const MISTAKE_SORT_OPTIONS = [
  { value: "fails", label: "按失败次数" },
  { value: "recent", label: "按最近尝试" }
];

// 原生 <select> 的弹出层由操作系统渲染（macOS 上是 Liquid Glass 玻璃材质），CSS 无法定制，
// 所以用与 ui-card / ui-input 同一套设计令牌的自定义组件替代；模板在 index.html 的 #ui-select-tpl。
const UiSelect = {
  name: "UiSelect",
  template: "#ui-select-tpl",
  props: {
    modelValue: { type: [String, Number], default: "" },
    options: { type: Array, default: () => [] }, // [{ value, label }]
    placeholder: { type: String, default: "请选择" },
    dense: { type: Boolean, default: false },
    disabled: { type: Boolean, default: false }
  },
  emits: ["update:modelValue"],
  data() {
    return { open: false, highlight: 0, dropUp: false, alignRight: false };
  },
  computed: {
    currentOption() {
      return this.options.find(o => o.value === this.modelValue) || null;
    },
    currentLabel() {
      return this.currentOption ? this.currentOption.label : this.placeholder;
    }
  },
  methods: {
    toggle() {
      if (this.open) this.close(); else this.openMenu();
    },
    openMenu() {
      if (this.disabled || this.open) return;
      const idx = this.options.findIndex(o => o.value === this.modelValue);
      this.highlight = idx >= 0 ? idx : 0;
      this.open = true;
      // 点击外部关闭用 document 级 mousedown，而不是 fixed 遮罩：
      // 顶栏的 backdrop-filter 等属性会把 fixed 后代劫持成相对顶栏定位，遮罩在任何布局下都不可靠。
      document.addEventListener("mousedown", this.onOutsideMousedown, true);
      this.$nextTick(() => {
        const trigger = this.$refs.trigger.getBoundingClientRect();
        const popup = this.$refs.popup;
        if (!popup) return;
        // 右侧放不下改右对齐，下方放不下向上弹
        if (trigger.left + popup.offsetWidth > window.innerWidth - 8 && trigger.right - popup.offsetWidth >= 8) {
          this.alignRight = true;
        }
        if (trigger.bottom + popup.offsetHeight + 8 > window.innerHeight && trigger.top - popup.offsetHeight - 8 > 0) {
          this.dropUp = true;
        }
        this.scrollHighlightIntoView();
      });
    },
    close() {
      if (!this.open) return;
      this.open = false;
      document.removeEventListener("mousedown", this.onOutsideMousedown, true);
    },
    choose(option) {
      this.$emit("update:modelValue", option.value);
      this.close();
      this.$nextTick(() => this.$refs.trigger && this.$refs.trigger.focus());
    },
    moveHighlight(delta) {
      const n = this.options.length;
      if (!n) return;
      this.highlight = (this.highlight + delta + n) % n;
      this.scrollHighlightIntoView();
    },
    scrollHighlightIntoView() {
      this.$nextTick(() => {
        const el = this.$refs.popup && this.$refs.popup.querySelector(`[data-index="${this.highlight}"]`);
        if (el) el.scrollIntoView({ block: "nearest" });
      });
    },
    onOutsideMousedown(e) {
      if (this.$refs.root && !this.$refs.root.contains(e.target)) this.close();
    },
    onKeydown(e) {
      if (!this.open) {
        if (["Enter", " ", "ArrowDown", "ArrowUp"].includes(e.key)) {
          e.preventDefault();
          this.openMenu();
        }
        return;
      }
      switch (e.key) {
        case "ArrowDown": e.preventDefault(); this.moveHighlight(1); break;
        case "ArrowUp": e.preventDefault(); this.moveHighlight(-1); break;
        case "Home": e.preventDefault(); this.highlight = 0; this.scrollHighlightIntoView(); break;
        case "End": e.preventDefault(); this.highlight = this.options.length - 1; this.scrollHighlightIntoView(); break;
        case "Enter": case " ":
          e.preventDefault();
          if (this.options[this.highlight]) this.choose(this.options[this.highlight]);
          break;
        case "Escape": e.preventDefault(); this.close(); break;
        case "Tab": this.close(); break;
      }
    }
  },
  beforeUnmount() {
    this.close();
  }
};

const app = createApp({
  setup() {
    // --- Auth State ---
    let cachedUser = null;
    try {
      cachedUser = JSON.parse(localStorage.getItem("codeactivityhub_user") || "null");
    } catch (e) {
      cachedUser = null;
    }

    const token = ref(localStorage.getItem("codeactivityhub_token") || "");
    const currentUser = ref(cachedUser);
    const isAuthChecking = ref(!!token.value && !currentUser.value);
    const isLoggedIn = computed(() => !!currentUser.value);
    
    const authMode = ref("login"); // 'login' | 'register'
    const authForm = ref({ username: "", password: "", confirmPassword: "" });
    const authError = ref("");
    const isAuthLoading = ref(false);

    const pwdForm = ref({ oldPassword: "", newPassword: "", confirmNewPassword: "" });
    const isChangingPwd = ref(false);

    // --- Dashboard & Platform State ---
    const overview = ref({
      stats: { total_ac: 0, total_subs: 0, today_ac: 0, today_subs: 0, streak: 0, platforms: {} },
      platforms_status: [],
      last_sync_time: ""
    });

    const currentTab = ref("overview"); // 'overview', 'submissions', 'mistakes', 'settings'
    const searchKeyword = ref("");
    const dateFilter = ref("all"); // 'all', 'today', '7d', '30d', 'year', 'custom'
    const startDate = ref("");
    const endDate = ref("");
    const verdictFilter = ref("all"); // 'all', 'AC', 'WA'
    const selectedTag = ref("");
    const currentPage = ref(1);
    const pageSize = ref(30);

    const rawHeatmap = ref([]);
    const heatmapFilter = ref("all");
    const selectedHeatmapYear = ref(new Date().getFullYear());
    const availableHeatmapYears = ref([new Date().getFullYear()]);
    const tagStats = ref([]);
    const mistakes = ref([]);
    const submissions = ref([]);
    const subFilter = ref("all");

    // --- Contests State ---
    const contests = ref([]);
    const problems = ref([]);
    const problemsPlatform = ref("codeforces");
    const problemsPage = ref(1);
    // 洛谷等平台每页条数固定且与请求值不同，以后端返回的实际 limit 计算页数。
    const problemsLimit = ref(30);
    const problemsTotal = ref(0);
    const totalProblemPages = ref(1);
    const isLoadingProblems = ref(false);
    // 题库检索与同步：数据已全量入库，关键词/难度/标签/解决状态在服务端 SQL 过滤
    const problemsKeyword = ref("");
    const problemsDifficulty = ref("");
    const problemsTag = ref("");
    const problemsSolved = ref("all"); // 'all' | 'solved' | 'unsolved'
    const problemsFacets = ref({ difficulties: [], tags: [], solved: 0, total: 0, updated_at: "" });
    const problemSync = ref({});
    const contestFilter = ref("all"); // 'all', 'codeforces', 'atcoder', 'luogu'
    const contestSearch = ref("");
    const isSyncingContests = ref(false);
    // 日历页按"进行中 / 即将开始 / 已结束"分组展示，这两个开关控制长列表的展开
    const showAllUpcoming = ref(false);
    const showAllPast = ref(false);
    const nowTimestamp = ref(Math.floor(Date.now() / 1000));

    const platformStatusMap = ref({});
    const isSyncing = ref(false);
    const isSaving = ref(false);

    // --- 长效脚本 Token（独立于登录会话，可逐条轮换/吊销） ---
    const ingestTokens = ref([]);
    const isLoadingIngestTokens = ref(false);
    const isIssuingToken = ref(false);
    const tokenActionId = ref(0);
    const freshToken = ref("");        // 仅生成/轮换后显示一次
    const freshTokenName = ref("");

    // --- 平台多账号：同一平台可保存多个，单选启用 ---
    const platformMeta = {
      codeforces: { label: "Codeforces", dot: "bg-blue-500", handleLabel: "用户名 (Handle)", handlePlaceholder: "输入 CF 用户名 (如 MCGA_WJJ)", cookie: false, authText: "公开 API 免 Cookie" },
      leetcode: { label: "LeetCode", dot: "bg-amber-500", handleLabel: "用户名 (Username)", handlePlaceholder: "输入 LeetCode 用户名", cookie: false, authText: "公开 GraphQL 免 Cookie" },
      atcoder: { label: "AtCoder", dot: "bg-purple-500", handleLabel: "用户名 (Handle)", handlePlaceholder: "输入 AtCoder 用户名 (如 FarmingWAs)", cookie: false, authText: "公开 API 免 Cookie" },
      luogu: { label: "洛谷", dot: "bg-sky-500", handleLabel: "UID (纯数字)", handlePlaceholder: "输入 UID 纯数字 (如 1940760)", cookie: true, cookieLabel: "Cookie（__client_id 值，或整段 cookie）", cookiePlaceholder: "F12 → Network → 复制整段 cookie", authText: "UID 用公开主页校验；同步历史需要 __client_id + _uid（UID 会自动补上）" },
      acwing: { label: "AcWing", dot: "bg-indigo-500", handleLabel: "空间 ID (纯数字)", handlePlaceholder: "输入空间 ID 纯数字 (如 360946)", cookie: true, cookieLabel: "Cookie (sessionid)", cookiePlaceholder: "输入 sessionid 字符串", authText: "接口暂未适配，仅浏览器脚本实时接入" }
    };
    const accounts = ref([]);
    const isLoadingAccounts = ref(false);
    const isVerifyingAccount = ref({});
    const accountForms = ref(Object.fromEntries(
      Object.keys(platformMeta).map(key => [key, { name: "", handle: "", cookie: "" }])
    ));

    const userMenuOpen = ref(false);
    const toggleUserMenu = () => { userMenuOpen.value = !userMenuOpen.value; };
    const closeUserMenu = () => { userMenuOpen.value = false; };

    const showGuide = ref(false);
    const toast = ref({ show: false, message: "", type: "success" });
    const warningBanner = ref("");

    const toastClass = computed(() => {
      switch (toast.value.type) {
        case "success": return "bg-emerald-50 text-emerald-800 border-emerald-200";
        case "info": return "bg-amber-50 text-amber-800 border-amber-200";
        default: return "bg-rose-50 text-rose-800 border-rose-200";
      }
    });
    const toastIcon = computed(() => (toast.value.type === "success" ? "✓" : toast.value.type === "info" ? "!" : "✕"));

    // ECharts 实例引用
    let heatmapChart = null;
    let tagBarChart = null;
    let platformPieChart = null;

    // --- Toast 消息提示 ---
    const showToast = (message, type = "success") => {
      toast.value = { show: true, message, type };
      setTimeout(() => {
        toast.value.show = false;
      }, 3500);
    };

    // --- 安全统一 API 请求包装器 (彻底杜绝浏览器/CDN缓存) ---
    // 清理本地会话。修改密码/退出登录/收到 401 时复用同一套逻辑。
    const clearLocalSession = () => {
      token.value = "";
      localStorage.removeItem("codeactivityhub_token");
      localStorage.removeItem("codeactivityhub_user");
      currentUser.value = null;
      isAuthChecking.value = false;
    };

    const apiFetch = async (url, options = {}) => {
      options.headers = options.headers || {};
      options.headers["Cache-Control"] = "no-cache, no-store, must-revalidate";
      options.headers["Pragma"] = "no-cache";
      options.headers["Expires"] = "0";
      options.cache = "no-store";
      if (token.value) {
        options.headers["Authorization"] = `Bearer ${token.value}`;
      }
      
      // 为 GET 请求自动附加当前毫秒时间戳参数，强制浏览器向服务器请求最新数据
      let reqUrl = url;
      if (!options.method || options.method.toUpperCase() === "GET") {
        const sep = reqUrl.includes("?") ? "&" : "?";
        reqUrl = `${reqUrl}${sep}_t=${Date.now()}`;
      }
      
      const res = await fetch(reqUrl, options);
      if (res.status === 401) {
        clearLocalSession();
        authError.value = "登录会话已过期，请重新登录";
        throw new Error("UNAUTHORIZED");
      }
      return res;
    };

    // --- Auth Actions ---
    const handleLogin = async () => {
      authError.value = "";
      if (!authForm.value.username.trim() || !authForm.value.password) {
        authError.value = "请完整填写用户名与密码";
        return;
      }
      isAuthLoading.value = true;
      try {
        const res = await fetch("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            username: authForm.value.username.trim(),
            password: authForm.value.password
          })
        });
        const data = await res.json();
        if (res.ok && data.success) {
          token.value = data.token;
          localStorage.setItem("codeactivityhub_token", data.token);
          localStorage.setItem("codeactivityhub_user", JSON.stringify(data.user));
          currentUser.value = data.user;
          isAuthChecking.value = false;
          showToast(`欢迎回来，${data.user.username}！`, "success");
          authForm.value = { username: "", password: "", confirmPassword: "" };
          await reloadAllData();
        } else {
          authError.value = data.detail || data.message || "登录失败，请检查账号密码";
        }
      } catch (e) {
        if (e.message !== "UNAUTHORIZED") {
          authError.value = "网络请求失败: " + e.message;
        }
      } finally {
        isAuthLoading.value = false;
      }
    };

    const handleRegister = async () => {
      authError.value = "";
      const uname = authForm.value.username.trim();
      const pwd = authForm.value.password;
      const cpwd = authForm.value.confirmPassword;

      if (!uname || !pwd || !cpwd) {
        authError.value = "请完整填写注册信息";
        return;
      }
      if (uname.length < 3 || uname.length > 32) {
        authError.value = "用户名长度须在 3-32 位之间";
        return;
      }
      if (pwd.length < 6) {
        authError.value = "密码长度不能少于 6 位";
        return;
      }
      if (pwd !== cpwd) {
        authError.value = "两次输入的密码不一致";
        return;
      }

      isAuthLoading.value = true;
      try {
        const res = await fetch("/api/auth/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ username: uname, password: pwd })
        });
        const data = await res.json();
        if (res.ok && data.success) {
          token.value = data.token;
          localStorage.setItem("codeactivityhub_token", data.token);
          localStorage.setItem("codeactivityhub_user", JSON.stringify(data.user));
          currentUser.value = data.user;
          isAuthChecking.value = false;
          showToast("账户注册成功！", "success");
          authForm.value = { username: "", password: "", confirmPassword: "" };
          await reloadAllData();
        } else {
          authError.value = data.detail || data.message || "注册失败";
        }
      } catch (e) {
        if (e.message !== "UNAUTHORIZED") {
          authError.value = "网络请求异常: " + e.message;
        }
      } finally {
        isAuthLoading.value = false;
      }
    };

    const handleLogout = async () => {
      try {
        if (token.value) {
          await apiFetch("/api/auth/logout", { method: "POST" });
        }
      } catch (e) {
        // ignore
      } finally {
        clearLocalSession();
        showToast("已成功退出登录", "success");
      }
    };

    const handleChangePassword = async () => {
      if (!pwdForm.value.oldPassword || !pwdForm.value.newPassword) {
        showToast("请填写原密码与新密码", "error");
        return;
      }
      if (pwdForm.value.newPassword.length < 6) {
        showToast("新密码长度不能少于 6 位", "error");
        return;
      }
      if (pwdForm.value.newPassword !== pwdForm.value.confirmNewPassword) {
        showToast("两次输入的新密码不一致", "error");
        return;
      }

      isChangingPwd.value = true;
      try {
        const res = await apiFetch("/api/auth/password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            old_password: pwdForm.value.oldPassword,
            new_password: pwdForm.value.newPassword
          })
        });
        const data = await res.json();
        if (res.ok && data.success) {
          showToast("密码修改成功！请重新登录", "success");
          pwdForm.value = { oldPassword: "", newPassword: "", confirmNewPassword: "" };
          // 后端已删除该用户全部会话，再调 /logout 只会拿到 401；
          // 这里直接清理本地状态即可。
          setTimeout(() => {
            clearLocalSession();
          }, 1500);
        } else {
          showToast(data.detail || data.message || "修改密码失败", "error");
        }
      } catch (e) {
        showToast("请求失败: " + e.message, "error");
      } finally {
        isChangingPwd.value = false;
      }
    };

    // --- Submissions Filter & Pagination ---
    const availableTags = computed(() => {
      const set = new Set();
      (submissions.value || []).forEach(s => {
        (s.tags || []).forEach(t => set.add(t));
      });
      return Array.from(set).sort();
    });

    // 自定义下拉（ui-select）的选项源
    const heatmapYearOptions = computed(() =>
      (availableHeatmapYears.value || []).map(y => ({ value: y, label: `${y} 年` }))
    );
    const tagFilterOptions = computed(() => [
      { value: "", label: "全部标签" },
      ...availableTags.value.map(t => ({ value: t, label: t }))
    ]);

    // --- 本地日期工具 ---
    // 后端 date 字段按展示时区（Asia/Shanghai）计算自然日；前端此前用
    // toISOString() 取的是 UTC 日期，在 UTC+8 下每天 00:00-08:00 会整体偏移一天，
    // 导致"今天/近7天"筛选漏掉或混入记录。统一改用本地日期字符串比较。
    const localDateStr = (d) => {
      const pad = (n) => String(n).padStart(2, "0");
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
    };
    const shiftDays = (n) => {
      const d = new Date();
      d.setDate(d.getDate() - n);
      return localDateStr(d);
    };
    const submissionDate = (s) => s.date || (s.submitted_at || "").split(" ")[0];

    const filteredSubmissions = computed(() => {
      let list = submissions.value || [];

      // 1. 平台过滤
      if (subFilter.value !== "all") {
        list = list.filter(s => s.platform === subFilter.value);
      }

      // 2. 状态过滤
      if (verdictFilter.value === "AC") {
        list = list.filter(s => s.verdict === "AC");
      } else if (verdictFilter.value === "WA") {
        list = list.filter(s => s.verdict !== "AC");
      }

      // 3. 算法标签过滤
      if (selectedTag.value) {
        list = list.filter(s => (s.tags || []).includes(selectedTag.value));
      }

      // 4. 日期范围过滤
      if (dateFilter.value !== "all") {
        const today = new Date();
        const todayStr = localDateStr(today);

        if (dateFilter.value === "today") {
          list = list.filter(s => submissionDate(s) === todayStr);
        } else if (dateFilter.value === "7d") {
          const d7Str = shiftDays(7);
          list = list.filter(s => {
            const d = submissionDate(s);
            return d && d >= d7Str;
          });
        } else if (dateFilter.value === "30d") {
          const d30Str = shiftDays(30);
          list = list.filter(s => {
            const d = submissionDate(s);
            return d && d >= d30Str;
          });
        } else if (dateFilter.value === "year") {
          const yStr = `${today.getFullYear()}-01-01`;
          list = list.filter(s => {
            const d = submissionDate(s);
            return d && d >= yStr;
          });
        } else if (dateFilter.value === "custom") {
          if (startDate.value) {
            list = list.filter(s => {
              const d = submissionDate(s);
              return !d || d >= startDate.value;
            });
          }
          if (endDate.value) {
            list = list.filter(s => {
              const d = submissionDate(s);
              return !d || d <= endDate.value;
            });
          }
        }
      }

      // 5. 关键词即时搜索
      if (searchKeyword.value.trim()) {
        const kw = searchKeyword.value.trim().toLowerCase();
        list = list.filter(s => 
          (s.problem_id && s.problem_id.toLowerCase().includes(kw)) ||
          (s.problem_title && s.problem_title.toLowerCase().includes(kw)) ||
          (s.code_language && s.code_language.toLowerCase().includes(kw)) ||
          (s.date && s.date.includes(kw)) ||
          (s.submitted_at && s.submitted_at.includes(kw)) ||
          (s.tags && s.tags.some(t => t.toLowerCase().includes(kw)))
        );
      }
      return list;
    });

    const totalPages = computed(() => {
      return Math.ceil(filteredSubmissions.value.length / pageSize.value) || 1;
    });

    const paginatedSubmissions = computed(() => {
      const start = (currentPage.value - 1) * pageSize.value;
      return filteredSubmissions.value.slice(start, start + pageSize.value);
    });

    const setSubFilter = (p) => {
      subFilter.value = p;
      currentPage.value = 1;
    };

    const setDateFilter = (preset) => {
      dateFilter.value = preset;
      currentPage.value = 1;
    };

    // 筛选条件变化后必须回到第一页：否则停留在第 N 页会 sliced 出空数组，
    // 提示"暂无匹配提交记录"，但实际有匹配结果。
    watch(
      [subFilter, verdictFilter, selectedTag, searchKeyword, startDate, endDate],
      () => { currentPage.value = 1; }
    );

    const resetFilters = () => {
      dateFilter.value = "all";
      startDate.value = "";
      endDate.value = "";
      subFilter.value = "all";
      verdictFilter.value = "all";
      selectedTag.value = "";
      searchKeyword.value = "";
      currentPage.value = 1;
    };

    // 状态 pill 分三档语义色：通过 / 超限类(warn) / 错误类(error)，判题中与未知保持中性灰
    const verdictPillClass = (v) => {
      if (v === "AC") return "bg-emerald-50 text-emerald-700 border-emerald-200";
      if (["TLE", "MLE", "OLE"].includes(v)) return "bg-amber-50 text-amber-700 border-amber-200";
      if (["WA", "CE", "RE", "IE", "ERROR"].includes(v)) return "bg-rose-50 text-rose-700 border-rose-200";
      return "bg-slate-100 text-slate-500 border-slate-200";
    };

    // 难度只给可识别的通用档位着色（力扣风格与中文等价词），数字分值等沿用表格默认色
    const difficultyClass = (d) => {
      const s = String(d || "").trim().toLowerCase();
      if (/^(easy|简单|入门)$/.test(s)) return "text-emerald-700";
      if (/^(medium|中等)$/.test(s)) return "text-amber-700";
      if (/^(hard|困难|较难)$/.test(s)) return "text-rose-700";
      return "";
    };

    const hasActiveFilters = computed(() =>
      subFilter.value !== "all" || verdictFilter.value !== "all" || !!selectedTag.value ||
      searchKeyword.value.trim() !== "" || dateFilter.value !== "all" || !!startDate.value || !!endDate.value
    );

    const pageSizeOptions = [
      { value: 30, label: "30 条" },
      { value: 50, label: "50 条" },
      { value: 100, label: "100 条" }
    ];
    const changePageSize = (n) => {
      pageSize.value = Number(n);
      currentPage.value = 1;
    };

    // --- 错题集：平台筛选 + 排序 + 概览统计 ---
    const mistakePlatformFilter = ref("all");
    const mistakeSort = ref("fails"); // 'fails' 失败次数优先 | 'recent' 最近尝试优先

    const mistakePlatformChips = computed(() => {
      const counts = {};
      (mistakes.value || []).forEach(m => { counts[m.platform] = (counts[m.platform] || 0) + 1; });
      return [
        { value: "all", label: "全部" },
        ...Object.keys(counts).map(p => ({ value: p, label: MISTAKE_PLATFORM_SHORT[p] || p }))
      ];
    });

    const filteredMistakes = computed(() => {
      let list = mistakes.value || [];
      if (mistakePlatformFilter.value !== "all") {
        list = list.filter(m => m.platform === mistakePlatformFilter.value);
      }
      const sorted = [...list];
      sorted.sort((a, b) => mistakeSort.value === "recent"
        ? String(b.submitted_at || "").localeCompare(String(a.submitted_at || ""))
        : ((b.fail_times || 0) - (a.fail_times || 0)) || String(b.submitted_at || "").localeCompare(String(a.submitted_at || "")));
      return sorted;
    });

    const setMistakePlatform = (p) => { mistakePlatformFilter.value = p; };
    const setMistakeSort = (s) => { mistakeSort.value = s; };

    // 概览统计基于全量错题（不受列表筛选影响），方便先看全局再下钻
    const mistakeStats = computed(() => {
      const list = mistakes.value || [];
      const byPlatform = {};
      list.forEach(m => { byPlatform[m.platform] = (byPlatform[m.platform] || 0) + 1; });
      const recentSince = shiftDays(7);
      const recent = list.filter(m => { const d = submissionDate(m); return d && d >= recentSince; });
      const stubborn = list.filter(m => (m.fail_times || 0) >= 3);
      const worst = [...list].sort((a, b) => (b.fail_times || 0) - (a.fail_times || 0))[0] || null;
      return { total: list.length, byPlatform, stubborn: stubborn.length, recent: recent.length, worst };
    });

    // --- Contest Computed & Helpers ---
    const getContestStatus = (c) => {
      const now = nowTimestamp.value;
      const st = c.start_timestamp;
      const dur = c.duration_seconds || 7200;
      const et = st + dur;
      if (now < st) return "BEFORE";
      if (now < et) return "CODING";
      return "FINISHED";
    };

    const formatContestCountdown = (c) => {
      const now = nowTimestamp.value;
      const st = c.start_timestamp;
      const dur = c.duration_seconds || 7200;
      const et = st + dur;

      if (now < st) {
        const diff = st - now;
        const days = Math.floor(diff / 86400);
        const hours = Math.floor((diff % 86400) / 3600);
        const mins = Math.floor((diff % 3600) / 60);
        const secs = diff % 60;
        if (days > 0) {
          return `还有 ${days} 天 ${hours} 小时 ${mins} 分`;
        } else if (hours > 0) {
          return `还有 ${hours} 小时 ${mins} 分 ${secs} 秒`;
        } else {
          return `还有 ${mins} 分 ${secs} 秒`;
        }
      } else if (now < et) {
        const remaining = et - now;
        const hours = Math.floor(remaining / 3600);
        const mins = Math.floor((remaining % 3600) / 60);
        const secs = remaining % 60;
        if (hours > 0) {
          return `🔥 正在进行中 (剩余 ${hours}小时${mins}分)`;
        } else {
          return `🔥 正在进行中 (剩余 ${mins}分${secs}秒)`;
        }
      } else {
        return "已结束";
      }
    };

    const filteredContests = computed(() => {
      let list = contests.value || [];
      if (contestFilter.value !== "all") {
        list = list.filter(c => c.platform === contestFilter.value);
      }
      if (contestSearch.value.trim()) {
        const kw = contestSearch.value.trim().toLowerCase();
        list = list.filter(c => 
          (c.name || "").toLowerCase().includes(kw) || 
          (c.rule_type || "").toLowerCase().includes(kw) || 
          (c.platform || "").toLowerCase().includes(kw)
        );
      }
      return list;
    });

    const upcomingContestsCount = computed(() => {
      const now = nowTimestamp.value;
      return (contests.value || []).filter(c => (c.start_timestamp + (c.duration_seconds || 7200)) > now).length;
    });

    // --- 日历页三分组：进行中 / 即将开始 / 已结束（都基于筛选结果） ---
    const liveContests = computed(() =>
      filteredContests.value
        .filter(c => getContestStatus(c) === "CODING")
        .sort((a, b) => (a.start_timestamp + (a.duration_seconds || 7200)) - (b.start_timestamp + (b.duration_seconds || 7200)))
    );
    const upcomingContests = computed(() =>
      filteredContests.value
        .filter(c => getContestStatus(c) === "BEFORE")
        .sort((a, b) => a.start_timestamp - b.start_timestamp)
    );
    const pastContests = computed(() =>
      filteredContests.value
        .filter(c => getContestStatus(c) === "FINISHED")
        .sort((a, b) => b.start_timestamp - a.start_timestamp)
    );
    // 折叠渲染上限：倒计时每秒重渲染，几百张卡会卡顿（与旧分页同样的历史教训）。
    const shownUpcomingContests = computed(() =>
      showAllUpcoming.value ? upcomingContests.value : upcomingContests.value.slice(0, 24)
    );
    const shownPastContests = computed(() =>
      showAllPast.value ? pastContests.value : pastContests.value.slice(0, 8)
    );

    // 倒计时分段：返回 { d, h, m, s }，h/m/s 补零，天数不进位。
    const countdownParts = (diffSeconds) => {
      const diff = Math.max(0, Math.floor(diffSeconds));
      const pad = (n) => String(n).padStart(2, "0");
      return {
        d: Math.floor(diff / 86400),
        h: pad(Math.floor((diff % 86400) / 3600)),
        m: pad(Math.floor((diff % 3600) / 60)),
        s: pad(diff % 60),
      };
    };
    // 即将开始：距开赛的倒计时分段
    const formatCountdownTo = (c) => countdownParts(c.start_timestamp - nowTimestamp.value);
    // 进行中：距结束的剩余时间（HH:MM:SS，超一天带天数）
    const formatRemaining = (c) => {
      const p = countdownParts((c.start_timestamp + (c.duration_seconds || 7200)) - nowTimestamp.value);
      return p.d > 0 ? `${p.d}天 ${p.h}:${p.m}:${p.s}` : `${p.h}:${p.m}:${p.s}`;
    };
    // 开赛时间："09-14 周日 19:35"
    const formatContestDate = (c) => {
      if (!c.start_timestamp) return "";
      const dt = new Date(c.start_timestamp * 1000);
      const pad = (n) => String(n).padStart(2, "0");
      const week = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"][dt.getDay()];
      return `${pad(dt.getMonth() + 1)}-${pad(dt.getDate())} ${week} ${pad(dt.getHours())}:${pad(dt.getMinutes())}`;
    };
    // 平台标识：复用设置页 platformMeta 的配色，保证全站一致
    const contestPlatformDot = (p) => (platformMeta[p] || {}).dot || "bg-slate-400";
    const contestPlatformLabel = (p) => (platformMeta[p] || {}).label || p;

    const topUpcomingContests = computed(() => {
      const now = nowTimestamp.value;
      return (contests.value || [])
        .filter(c => (c.start_timestamp + (c.duration_seconds || 7200)) > now)
        .slice(0, 3);
    });

    // --- Data Loaders (绑定当前用户) ---
    const loadOverview = async () => {
      try {
        const res = await apiFetch("/api/stats/overview");
        const data = await res.json();
        if (data && data.stats) {
          overview.value = {
            stats: {
              total_ac: data.stats.total_ac || 0,
              total_subs: data.stats.total_subs || 0,
              today_ac: data.stats.today_ac || 0,
              today_subs: data.stats.today_subs || 0,
              streak: data.stats.streak || 0,
              platforms: data.stats.platforms || {}
            },
            platforms_status: data.platforms_status || [],
            last_sync_time: data.last_sync_time || overview.value.last_sync_time || ""
          };
        }

        if (data.platforms_status && Array.isArray(data.platforms_status)) {
          const newStatusMap = { ...platformStatusMap.value };
          data.platforms_status.forEach(p => {
            newStatusMap[p.platform] = p;
          });
          platformStatusMap.value = newStatusMap;
        }
        
        // 红色横幅只留给真正的连接异常（如凭证失效、接口报错）。
        // warning 是瞬时超时（网络波动/站点限流），账号配置没问题——
        // 靠顶部琥珀点和同步后的提示反馈即可，不该挂一条常驻红条吓人。
        const problems = (data.platforms_status || []).filter(p => p.status === "error");
        warningBanner.value = problems.length
          ? "连接异常 → " + problems
              .map(p => `${platformMeta[p.platform]?.label || p.platform}：${p.message || "未获取到具体原因"}`)
              .join("；")
          : "";
      } catch (e) {
        console.error("加载 Overview 失败:", e);
      }
    };

    const loadHeatmap = async () => {
      try {
        const res = await apiFetch(`/api/stats/heatmap?platform=${heatmapFilter.value}&year=${selectedHeatmapYear.value}`);
        const data = await res.json();
        rawHeatmap.value = data.heatmap || [];
        if (data.available_years && data.available_years.length > 0) {
          availableHeatmapYears.value = data.available_years;
        }
        if (data.year) {
          selectedHeatmapYear.value = data.year;
        }
        renderHeatmap();
      } catch (e) {
        console.error("加载 Heatmap 失败:", e);
      }
    };

    const loadTags = async () => {
      try {
        const res = await apiFetch("/api/stats/tags");
        const data = await res.json();
        tagStats.value = data.tags || [];
        renderTagBarChart();
      } catch (e) {
        console.error("加载 Tag 统计失败:", e);
      }
    };

    const loadMistakes = async () => {
      try {
        const res = await apiFetch("/api/stats/mistakes?limit=50");
        const data = await res.json();
        mistakes.value = data.mistakes || [];
      } catch (e) {
        console.error("加载错题集失败:", e);
      }
    };

    const loadSubmissions = async () => {
      try {
        const res = await apiFetch("/api/stats/submissions?limit=2000");
        const data = await res.json();
        submissions.value = data.submissions || [];
      } catch (e) {
        console.error("加载提交记录流失败:", e);
      }
    };

    const loadProblems = async () => {
      isLoadingProblems.value = true;
      try {
        const params = new URLSearchParams({
          platform: problemsPlatform.value,
          page: problemsPage.value,
          limit: problemsLimit.value,
          keyword: problemsKeyword.value,
          difficulty: problemsDifficulty.value,
          tag: problemsTag.value,
          solved: problemsSolved.value,
        });
        const res = await apiFetch(`/api/problems?${params.toString()}`);
        const data = await res.json();
        if (!res.ok) throw new Error(data.message || data.detail || "题库加载失败");
        problems.value = data.problems || [];
        problemsTotal.value = data.total || 0;
        if (data.limit) problemsLimit.value = data.limit;
        totalProblemPages.value = Math.max(1, Math.ceil((data.total || 0) / (data.limit || problemsLimit.value)));
        problemsFacets.value = data.facets || problemsFacets.value;
        const prev = problemSync.value;
        problemSync.value = data.sync || {};
        if (problemSync.value.error && !prev.error) {
          showToast("题库同步失败: " + problemSync.value.error, "error");
        }
      } catch (e) {
        problems.value = [];
        showToast("加载题库失败: " + e.message, "error");
      } finally {
        isLoadingProblems.value = false;
      }
    };

    const changeProblemsPlatform = (platform) => {
      problemsPlatform.value = platform;
      problemsPage.value = 1;
      loadProblems();
    };

    const changeProblemsLimit = (n) => {
      problemsLimit.value = Number(n);
      problemsPage.value = 1;
      loadProblems();
    };

    // ---------- 题库检索与同步 ----------
    const problemDifficultyOptions = computed(() => [
      { value: "", label: "全部难度" },
      ...(problemsFacets.value.difficulties || []).map(d => ({ value: d.value, label: `${d.value}（${d.count}）` }))
    ]);
    const problemTagOptions = computed(() => [
      { value: "", label: "全部标签" },
      ...(problemsFacets.value.tags || []).map(t => ({ value: t.value, label: `${t.value}（${t.count}）` }))
    ]);
    const problemSolvedOptions = [
      { value: "all", label: "全部" },
      { value: "unsolved", label: "未解决" },
      { value: "solved", label: "已解决" }
    ];

    const hasProblemsFilters = computed(() =>
      !!(problemsKeyword.value.trim() || problemsDifficulty.value || problemsTag.value || problemsSolved.value !== "all"));

    const resetProblemsFilters = () => {
      problemsKeyword.value = "";
      problemsDifficulty.value = "";
      problemsTag.value = "";
      problemsSolved.value = "all";
      problemsPage.value = 1;
      loadProblems();
    };

    const syncProblems = async () => {
      try {
        const res = await apiFetch(`/api/problems/sync?platform=${encodeURIComponent(problemsPlatform.value)}`, { method: "POST" });
        const data = await res.json();
        if (!res.ok) throw new Error(data.message || data.detail || "同步启动失败");
        problemSync.value = data.sync || problemSync.value;
        showToast(`题库同步已开始（${problemsPlatform.value}）`, "info");
      } catch (e) {
        showToast("启动同步失败: " + e.message, "error");
      }
    };

    // 关键词输入防抖后检索；难度/标签/解决状态变化立即检索
    let problemsKeywordTimer = null;
    watch(problemsKeyword, () => {
      clearTimeout(problemsKeywordTimer);
      problemsKeywordTimer = setTimeout(() => {
        problemsPage.value = 1;
        loadProblems();
      }, 400);
    });
    watch([problemsDifficulty, problemsTag, problemsSolved], () => {
      problemsPage.value = 1;
      loadProblems();
    });

    // 同步进行中每 1.5 秒轮询：loadProblems 顺带带回进度和已入库的数据
    let problemsSyncTimer = null;
    watch(() => problemSync.value.running, (running, prev) => {
      if (running && !prev) {
        problemsSyncTimer = setInterval(() => { loadProblems(); }, 1500);
      } else if (!running && prev) {
        clearInterval(problemsSyncTimer);
        loadProblems();
      }
    });

    const changeProblemsPage = (delta) => {
      problemsPage.value = Math.min(totalProblemPages.value, Math.max(1, problemsPage.value + delta));
      loadProblems();
    };

    let lastContestsLoadedAt = 0;
    const loadContests = async (force = false) => {
      // 切换标签页时 3 分钟内复用已有数据，避免反复整表下载。
      if (!force && contests.value.length && Date.now() - lastContestsLoadedAt < 3 * 60 * 1000) return;
      try {
        const res = await apiFetch("/api/contests?platform=all");
        const data = await res.json();
        contests.value = data.contests || [];
        lastContestsLoadedAt = Date.now();
      } catch (e) {
        console.error("加载比赛日程失败:", e);
      }
    };

    const syncContestsNow = async () => {
      isSyncingContests.value = true;
      try {
        const res = await apiFetch("/api/contests/sync", { method: "POST" });
        const data = await res.json();
        if (data.success) {
          showToast(data.message || "比赛日程刷新成功！", "success");
          await loadContests(true);
        } else {
          showToast(data.message || "刷新比赛异常", "error");
        }
      } catch (e) {
        showToast("刷新比赛请求失败: " + e.message, "error");
      } finally {
        isSyncingContests.value = false;
      }
    };

    // --- 长效脚本 Token（独立于登录会话，可逐个轮换/吊销） ---
    const loadIngestTokens = async () => {
      isLoadingIngestTokens.value = true;
      try {
        const res = await apiFetch("/api/ingest/tokens");
        const data = await res.json();
        ingestTokens.value = data.tokens || [];
      } catch (e) {
        console.error("加载脚本 Token 列表失败:", e);
      } finally {
        isLoadingIngestTokens.value = false;
      }
    };

    const createIngestToken = async () => {
      const name = window.prompt("给这个 Token 起个名字（例如：家里的电脑 / 实验室机器）", "浏览器脚本");
      if (name === null) return;
      isIssuingToken.value = true;
      try {
        const res = await apiFetch("/api/ingest/tokens", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: name.trim() })
        });
        const data = await res.json();
        if (data.success) {
          freshToken.value = (data.token && data.token.token) || "";
          freshTokenName.value = (data.token && data.token.name) || name.trim();
          showToast("Token 已生成，请立即复制（之后不再显示明文）", "success");
          await loadIngestTokens();
        } else {
          showToast(data.detail || data.message || "生成失败", "error");
        }
      } catch (e) {
        showToast("生成失败: " + e.message, "error");
      } finally {
        isIssuingToken.value = false;
      }
    };

    const rotateIngestToken = async (tk) => {
      if (!window.confirm(`轮换「${tk.name}」？旧 Token 会立即失效，所有使用它的脚本都需要更新。`)) return;
      tokenActionId.value = tk.id;
      try {
        const res = await apiFetch(`/api/ingest/tokens/${tk.id}/rotate`, { method: "POST" });
        const data = await res.json();
        if (data.success) {
          freshToken.value = (data.token && data.token.token) || "";
          freshTokenName.value = tk.name;
          showToast("已轮换，旧 Token 立即失效", "success");
          await loadIngestTokens();
        } else {
          showToast(data.detail || data.message || "轮换失败", "error");
        }
      } catch (e) {
        showToast("轮换失败: " + e.message, "error");
      } finally {
        tokenActionId.value = 0;
      }
    };

    const revokeIngestToken = async (tk) => {
      if (!window.confirm(`吊销「${tk.name}」？使用该 Token 的脚本会立即失效。`)) return;
      tokenActionId.value = tk.id;
      try {
        const res = await apiFetch(`/api/ingest/tokens/${tk.id}`, { method: "DELETE" });
        const data = await res.json();
        if (data.success) {
          showToast(data.message || "Token 已吊销", "success");
          if (freshTokenName.value === tk.name) dismissFreshToken();
          await loadIngestTokens();
        } else {
          showToast(data.detail || data.message || "吊销失败", "error");
        }
      } catch (e) {
        showToast("吊销失败: " + e.message, "error");
      } finally {
        tokenActionId.value = 0;
      }
    };

    const copyFreshToken = async () => {
      if (!freshToken.value) return;
      try {
        await navigator.clipboard.writeText(freshToken.value);
        showToast("Token 已复制，粘贴到 Tampermonkey 的脚本设置里", "success");
      } catch (e) {
        showToast("复制失败，请手动选中复制", "error");
      }
    };

    const dismissFreshToken = () => {
      freshToken.value = "";
      freshTokenName.value = "";
    };

    const isTokenActive = (tk) => !tk.revoked_at;

    const loadSettings = async () => {
      try {
        const res = await apiFetch("/api/settings");
        const data = await res.json();
        platformStatusMap.value = data.status || {};
        await Promise.all([loadIngestTokens(), loadAccounts()]);

      } catch (e) {
        console.error("加载设置失败:", e);
      }
    };

    const accountsFor = (platform) => (accounts.value || [])
      .filter(acc => acc.platform === platform)
      .sort((a, b) => (b.selected ? 1 : 0) - (a.selected ? 1 : 0) || a.id - b.id);

    const loadAccounts = async () => {
      isLoadingAccounts.value = true;
      try {
        const res = await apiFetch("/api/accounts");
        const data = await res.json();
        accounts.value = data.accounts || [];
      } catch (e) {
        console.error("加载平台账号失败:", e);
      } finally {
        isLoadingAccounts.value = false;
      }
    };

    const addAccount = async (platform, verify = true) => {
      const form = accountForms.value[platform] || {};
      if (!form.handle || !form.handle.trim()) {
        showToast("请先填写账号", "error");
        return;
      }
      try {
        const res = await apiFetch("/api/accounts", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ platform, name: form.name, handle: form.handle, cookie: form.cookie || "", verify })
        });
        const data = await res.json();
        if (data.success) {
          const acc = data.account || {};
          if (verify && acc.status !== "ok") {
            showToast("已保存，但验证未通过: " + (acc.message || "未知原因"), "error");
          } else {
            showToast("账号已保存" + (acc.selected ? "并启用" : ""), "success");
          }
          accountForms.value[platform] = { name: "", handle: "", cookie: "" };
          await Promise.all([loadAccounts(), loadOverview()]);
        } else {
          showToast(data.detail || data.message || "账号保存失败", "error");
        }
      } catch (e) {
        showToast("账号保存失败: " + e.message, "error");
      }
    };

    const verifyAccount = async (id) => {
      isVerifyingAccount.value[id] = true;
      try {
        const res = await apiFetch(`/api/accounts/${id}/verify`, { method: "POST" });
        const data = await res.json();
        if (data.valid) {
          showToast(data.message || "验证通过", "success");
        } else if (data.timeout) {
          showToast(data.message || "验证请求超时，请稍后重试", "info");
        } else if (data.supported === false) {
          // 平台没有公开校验接口——不是失败，别用红色报错吓人
          showToast(data.message || "该平台不支持在线校验，请用浏览器脚本接入", "info");
        } else {
          showToast(data.message || "验证失败", "error");
        }
        await Promise.all([loadAccounts(), loadOverview()]);
      } catch (e) {
        showToast("验证请求失败: " + e.message, "error");
      } finally {
        isVerifyingAccount.value[id] = false;
      }
    };

    const selectAccount = async (id) => {
      try {
        const res = await apiFetch(`/api/accounts/${id}/select`, { method: "POST" });
        const data = await res.json();
        if (data.success) {
          showToast(data.message || "已切换启用账号", "success");
          await Promise.all([loadAccounts(), loadOverview()]);
        } else {
          showToast(data.detail || data.message || "切换失败", "error");
        }
      } catch (e) {
        showToast("切换失败: " + e.message, "error");
      }
    };

    const deleteAccount = async (id) => {
      try {
        const res = await apiFetch(`/api/accounts/${id}`, { method: "DELETE" });
        const data = await res.json();
        if (data.success) {
          showToast(data.message || "账号已删除", "success");
          await Promise.all([loadAccounts(), loadOverview()]);
        } else {
          showToast(data.detail || data.message || "删除失败", "error");
        }
      } catch (e) {
        showToast("删除失败: " + e.message, "error");
      }
    };

    const reloadAllData = async () => {
      if (!isLoggedIn.value) return;
      await Promise.all([loadOverview(), loadHeatmap(), loadTags(), loadMistakes(), loadSubmissions(), loadSettings(), loadContests()]);
      await nextTick();
      if (currentTab.value === "overview") {
        renderHeatmap();
        renderTagBarChart();
        renderPlatformPie();
      }
    };

    // --- ECharts 渲染 ---
    const renderHeatmap = () => {
      const chartDom = document.getElementById("heatmap-chart");
      if (!chartDom) return;

      let chart = echarts.getInstanceByDom(chartDom);
      if (!chart) {
        chart = echarts.init(chartDom);
      }
      heatmapChart = chart;

      const targetYear = selectedHeatmapYear.value || new Date().getFullYear();
      const startDateStr = `${targetYear}-01-01`;
      const endDateStr = `${targetYear}-12-31`;

      // 补全全年每一天：没有提交的日子也要占一个 0 值格子，
      // 这样空天由 visualMap 统一配色，tooltip 能显示"没有提交"。
      const countByDate = {};
      (rawHeatmap.value || []).forEach(item => {
        if (item.date && item.date.startsWith(String(targetYear))) {
          countByDate[item.date] = item.count;
        }
      });
      const heatMapData = [];
      const cursor = new Date(targetYear, 0, 1);
      const endDate = new Date(targetYear, 11, 31);
      const pad = (n) => String(n).padStart(2, "0");
      while (cursor <= endDate) {
        const ds = `${cursor.getFullYear()}-${pad(cursor.getMonth() + 1)}-${pad(cursor.getDate())}`;
        heatMapData.push([ds, countByDate[ds] || 0]);
        cursor.setDate(cursor.getDate() + 1);
      }

      // 方形格子随容器宽度自适应。两个坑（canvas 实测得出）：
      // 1. calendar 同时给 left 和 right 时，ECharts 会用盒子宽度拉伸格子、无视
      //    显式 cellSize —— 所以这里只给 left，right 必须留空；
      // 2. 描边画在格子内部，步进就是 cellSize 本身，不需要做描边补偿。
      // 星期标签渲染在网格左侧约 18px（canvas 实测），居中时把它折进去使整体对称。
      // 用 echarts 自己的 getWidth/getHeight，避免容器尺寸读取时机问题。
      const dayLabelOverhang = 18;
      const chartW = chart.getWidth() || chartDom.clientWidth || 800;
      const chartH = chart.getHeight() || chartDom.clientHeight || 160;
      const jan1 = new Date(targetYear, 0, 1);
      const leap = (targetYear % 4 === 0 && targetYear % 100 !== 0) || targetYear % 400 === 0;
      const daysInYear = leap ? 366 : 365;
      const cols = Math.ceil((((jan1.getDay() + 6) % 7) + daysInYear) / 7);
      const cell = Math.max(13, Math.min(18,
        Math.floor((chartW - 34) / cols), Math.floor((chartH - 34) / 7)));
      const calendarLeft = Math.max(dayLabelOverhang + 8, Math.floor((chartW - cols * cell - dayLabelOverhang) / 2) + dayLabelOverhang);

      const option = {
        tooltip: {
          trigger: "item",
          appendToBody: true,
          confine: false,
          padding: [8, 12],
          backgroundColor: "#ffffff",
          borderColor: "#e5e7eb",
          borderWidth: 1,
          textStyle: {
            color: "#111827",
            fontFamily: "JetBrains Mono",
            fontSize: 11
          },
          extraCssText: "border-radius: 8px; box-shadow: 0 4px 16px rgba(16, 24, 40, 0.12); z-index: 99999;",
          formatter: function (p) {
            const d = new Date(p.value[0] + "T00:00:00");
            const week = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"][d.getDay()];
            const n = p.value[1];
            const line2 = n > 0
              ? `<span class="text-blue-600 font-bold">${n} 次提交</span>`
              : `<span class="text-slate-400">没有提交</span>`;
            return `<div class="font-mono text-xs font-semibold text-slate-900">${p.value[0]} ${week}</div><div class="text-xs font-mono mt-1">${line2}</div>`;
          }
        },
        // 阶梯式分档比线性映射更接近 GitHub 的观感：低活跃有区分度，
        // 高活跃不会早早"顶格"成一种颜色
        visualMap: {
          show: false,
          type: "piecewise",
          pieces: [
            { min: 1, max: 1, color: "#bfdbfe" },
            { min: 2, max: 3, color: "#93c5fd" },
            { min: 4, max: 6, color: "#60a5fa" },
            { min: 7, max: 9, color: "#3b82f6" },
            { min: 10, color: "#2563eb" }
          ],
          outOfRange: {
            color: "#eef2f7"
          }
        },
        calendar: {
          top: 24,
          left: calendarLeft,
          cellSize: [cell, cell],
          range: [startDateStr, endDateStr],
          itemStyle: {
            color: "#eef2f7",
            borderColor: "#ffffff",
            borderWidth: 2,
            borderRadius: 3
          },
          splitLine: { show: false },
          yearLabel: { show: false },
          dayLabel: {
            firstDay: 1,
            nameMap: ["日", "一", "二", "三", "四", "五", "六"],
            color: "#9ca3af",
            fontSize: 10,
            fontFamily: "JetBrains Mono"
          },
          monthLabel: {
            color: "#6b7280",
            fontSize: 11,
            fontFamily: "JetBrains Mono",
            margin: 6
          }
        },
        series: [{
          type: "heatmap",
          coordinateSystem: "calendar",
          data: heatMapData,
          emphasis: {
            itemStyle: {
              borderColor: "#2563eb",
              borderWidth: 1.5,
              shadowBlur: 6,
              shadowColor: "rgba(37, 99, 235, 0.35)"
            }
          }
        }]
      };

      chart.setOption(option, true);
      chart.resize();
    };

    const renderTagBarChart = () => {
      const chartDom = document.getElementById("tag-bar-chart");
      if (!chartDom) return;

      let chart = echarts.getInstanceByDom(chartDom);
      if (!chart) {
        chart = echarts.init(chartDom);
      }
      tagBarChart = chart;

      const topTags = [...(tagStats.value || [])].sort((a, b) => (b.value || 0) - (a.value || 0)).slice(0, 10).reverse();
      const categories = topTags.map(t => t.name);
      const acData = topTags.map(t => t.value);

      const option = {
        tooltip: {
          trigger: "axis",
          axisPointer: { type: "shadow" },
          className: "echarts-tooltip",
          formatter: function (params) {
            const p = params[0];
            return `<div class="font-mono text-xs font-semibold">${p.name}</div><div class="text-xs text-blue-600 mt-1">AC 题数: ${p.value}</div>`;
          }
        },
        grid: {
          left: "3%",
          right: "6%",
          bottom: "3%",
          top: "4%",
          containLabel: true
        },
        xAxis: {
          type: "value",
          splitLine: { lineStyle: { color: "#f1f3f5" } },
          axisLabel: { color: "#6b7280", fontSize: 10, fontFamily: "JetBrains Mono" }
        },
        yAxis: {
          type: "category",
          data: categories,
          axisLine: { lineStyle: { color: "#e5e7eb" } },
          axisLabel: { color: "#374151", fontSize: 11, fontFamily: "Plus Jakarta Sans" }
        },
        series: [{
          name: "AC 题数",
          type: "bar",
          data: acData,
          itemStyle: {
            borderRadius: [0, 4, 4, 0],
            color: "#3b82f6"
          }
        }]
      };

      chart.setOption(option, true);
      chart.resize();
    };

    const renderPlatformPie = () => {
      const chartDom = document.getElementById("platform-pie-chart");
      if (!chartDom) return;

      let chart = echarts.getInstanceByDom(chartDom);
      if (!chart) {
        chart = echarts.init(chartDom);
      }
      platformPieChart = chart;

      const pData = overview.value.stats.platforms || {};
      const data = [
        { value: pData.codeforces?.ac || 0, name: "Codeforces", itemStyle: { color: "#06b6d4" } },
        { value: pData.leetcode?.ac || 0, name: "LeetCode", itemStyle: { color: "#22c55e" } },
        { value: pData.atcoder?.ac || 0, name: "AtCoder", itemStyle: { color: "#a855f7" } },
        { value: pData.luogu?.ac || 0, name: "洛谷", itemStyle: { color: "#3b82f6" } },
        { value: pData.acwing?.ac || 0, name: "AcWing", itemStyle: { color: "#6366f1" } }
      ].filter(d => d.value > 0);

      const option = {
        tooltip: {
          trigger: "item",
          formatter: "{b}: {c} 题 ({d}%)",
          className: "echarts-tooltip"
        },
        legend: {
          bottom: "5%",
          left: "center",
          textStyle: { color: "#6b7280", fontSize: 11, fontFamily: "JetBrains Mono" }
        },
        series: [{
          name: "通过题量分布",
          type: "pie",
          radius: ["45%", "70%"],
          center: ["50%", "45%"],
          avoidLabelOverlap: false,
          itemStyle: {
            borderRadius: 6,
            borderColor: "#ffffff",
            borderWidth: 2
          },
          label: { show: false },
          emphasis: {
            label: {
              show: true,
              fontSize: 14,
              fontWeight: "bold",
              color: "#111827"
            }
          },
          data: data.length ? data : [{ value: 0, name: "暂无数据", itemStyle: { color: "#e5e7eb" } }]
        }]
      };

      chart.setOption(option, true);
      chart.resize();
    };

    // --- Platform & Sync Actions ---
    const setHeatmapFilter = (p) => {
      heatmapFilter.value = p;
      loadHeatmap();
    };

    const setHeatmapYear = (yr) => {
      selectedHeatmapYear.value = yr;
      loadHeatmap();
    };

    const syncNow = async (platform = "all") => {
      isSyncing.value = true;
      try {
        const res = await apiFetch("/api/sync", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ platform })
        });
        const data = await res.json();
        if (data.data?.synced_at) {
          overview.value.last_sync_time = data.data.synced_at;
        }
        if (data.success) {
          showToast(data.data?.message || "公开数据手动同步完成；提交后的新记录由浏览器脚本实时接入。", "success");
        } else {
          showToast(data.detail || data.message || "部分平台同步未成功，请检查状态", "error");
        }
        await reloadAllData();
      } catch (e) {
        if (e.message !== "UNAUTHORIZED") {
          showToast("同步请求失败: " + e.message, "error");
        }
      } finally {
        isSyncing.value = false;
        await nextTick();
        if (currentTab.value === "overview") {
          renderHeatmap();
          renderTagBarChart();
          renderPlatformPie();
        }
      }
    };

    // 平台账号已在各自的卡片里单独保存/验证；这里只触发一次同步。
    const saveSettingsAndSync = async () => {
      isSaving.value = true;
      try {
        await syncNow("all");
      } finally {
        isSaving.value = false;
      }
    };

    // --- Helpers ---
    const formatTimeAgo = (timeStr) => {
      if (!timeStr) return "";
      // 后端统一输出 "YYYY-MM-DD HH:mm:ss"（展示时区，无时区后缀）。
      // Safari 等浏览器对这种无时区字符串的 Date.parse 结果不一致（甚至 NaN），
      // 所以先按本地时间手工解析，确保"刚刚 / N 分钟前"不会算错。
      const local = String(timeStr).trim().match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2}))?/);
      let ts;
      if (local) {
        ts = new Date(
          parseInt(local[1], 10), parseInt(local[2], 10) - 1, parseInt(local[3], 10),
          parseInt(local[4], 10), parseInt(local[5], 10), parseInt(local[6] || "0", 10)
        ).getTime();
      } else {
        // 兼容仍带时区后缀的 RFC3339（同步接口/旧数据）。
        ts = Date.parse(timeStr);
      }
      if (!Number.isFinite(ts)) return timeStr;
      const diffMs = Date.now() - ts;
      const diffSec = Math.floor(diffMs / 1000);

      if (diffSec < 0) return timeStr;
      if (diffSec < 45) return "刚刚";
      if (diffSec < 3600) return `${Math.floor(diffSec / 60)}分钟前`;
      if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}小时前`;
      const d = new Date(ts);
      const hhmm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
      if (diffSec < 86400 * 2) return `昨天 ${hhmm}`;
      if (diffSec < 86400 * 3) return `前天 ${hhmm}`;

      return timeStr;
    };

    const platformBadge = (platform) => {
      switch (platform) {
        case "codeforces": return "CF";
        case "atcoder": return "ATCODER";
        case "luogu": return "LUOGU";
        case "leetcode": return "LEETCODE";
        default: return "ACWING";
      }
    };

    // 顶部平台指示灯：返回圆点后缀（dot-ok / dot-warn / dot-error / dot-muted）。
    // unsupported（平台没有公开校验接口）不是故障，用中性灰点。
    const getStatusClass = (platform) => {
      const s = platformStatusMap.value[platform]?.status;
      if (s === "ok") return "ok";
      if (s === "warning") return "warn";
      if (s === "error") return "error";
      return "muted";
    };

    // --- Lifecycle ---
    onMounted(async () => {
      // 1 秒级时间戳定时器 (仅在需要倒计时的页面跳动，后台标签页自动休眠省电)
      setInterval(() => {
        if (!document.hidden && (currentTab.value === 'contests' || currentTab.value === 'overview')) {
          nowTimestamp.value = Math.floor(Date.now() / 1000);
        }
      }, 1000);

      if (token.value) {
        try {
          const res = await apiFetch("/api/auth/me");
          const data = await res.json();
          currentUser.value = data.user;
          localStorage.setItem("codeactivityhub_user", JSON.stringify(data.user));
          await reloadAllData();
        } catch (e) {
          if (e.message === "UNAUTHORIZED") {
            currentUser.value = null;
            localStorage.removeItem("codeactivityhub_token");
            localStorage.removeItem("codeactivityhub_user");
          }
        } finally {
          isAuthChecking.value = false;
        }
      } else {
        isAuthChecking.value = false;
      }

      window.addEventListener("resize", () => {
        if (currentTab.value === "overview") {
          heatmapChart && heatmapChart.resize();
          tagBarChart && tagBarChart.resize();
          platformPieChart && platformPieChart.resize();
          renderHeatmap(); // 格子尺寸按容器宽度计算，resize 后需要重算
        }
      });

      // 不再定时刷新平台数据；提交由 Tampermonkey 事件推送，页面切换或点击“立即同步”时读取最新聚合结果。
    });

    // 标签页切换自动静默拉取最新数据，彻底告别手动硬刷新
    watch(currentTab, async (tab) => {
      if (tab === "overview") {
        await loadOverview();
        nextTick(() => {
          renderHeatmap();
          renderTagBarChart();
          renderPlatformPie();
        });
      } else if (tab === "submissions") {
        await loadSubmissions();
      } else if (tab === "mistakes") {
        await loadMistakes();
      } else if (tab === "contests") {
        await loadContests();
      } else if (tab === "problems") {
        await loadProblems();
      } else if (tab === "settings") {
        await loadSettings();
      }
    });

    return {
      token,
      currentUser,
      isAuthChecking,
      isLoggedIn,
      authMode,
      authForm,
      authError,
      isAuthLoading,
      pwdForm,
      isChangingPwd,
      handleLogin,
      handleRegister,
      handleLogout,
      handleChangePassword,
      currentTab,
      searchKeyword,
      dateFilter,
      startDate,
      endDate,
      verdictFilter,
      selectedTag,
      availableTags,
      heatmapYearOptions,
      tagFilterOptions,
      problemPlatformOptions: PROBLEM_PLATFORM_OPTIONS,
      subPlatformOptions: SUB_PLATFORM_OPTIONS,
      verdictOptions: VERDICT_OPTIONS,
      currentPage,
      pageSize,
      totalPages,
      paginatedSubmissions,
      filteredSubmissions,
      setDateFilter,
      resetFilters,
      verdictPillClass,
      difficultyClass,
      hasActiveFilters,
      pageSizeOptions,
      changePageSize,
      overview,
      rawHeatmap,
      heatmapFilter,
      selectedHeatmapYear,
      availableHeatmapYears,
      setHeatmapYear,
      tagStats,
      mistakes,
      mistakePlatformFilter,
      mistakeSort,
      mistakePlatformChips,
      mistakeSortOptions: MISTAKE_SORT_OPTIONS,
      filteredMistakes,
      setMistakePlatform,
      setMistakeSort,
      mistakeStats,
      submissions,
      subFilter,
      setSubFilter,
      platformStatusMap,
      isSyncing,
      isSaving,
      ingestTokens,
      isLoadingIngestTokens,
      isIssuingToken,
      tokenActionId,
      freshToken,
      freshTokenName,
      loadIngestTokens,
      createIngestToken,
      rotateIngestToken,
      revokeIngestToken,
      copyFreshToken,
      dismissFreshToken,
      isTokenActive,
      // 平台多账号
      platformMeta,
      accounts,
      accountsFor,
      accountForms,
      isLoadingAccounts,
      isVerifyingAccount,
      addAccount,
      verifyAccount,
      selectAccount,
      deleteAccount,
      userMenuOpen,
      toggleUserMenu,
      closeUserMenu,
      showGuide,
      toast,
      toastClass,
      toastIcon,
      warningBanner,
      setHeatmapFilter,
      syncNow,
      saveSettingsAndSync,
      formatTimeAgo,
      platformBadge,
      getStatusClass,
      // Problems exports
      problems,
      problemsPlatform,
      problemsPage,
      problemsTotal,
      problemsLimit,
      totalProblemPages,
      isLoadingProblems,
      loadProblems,
      changeProblemsPlatform,
      changeProblemsLimit,
      changeProblemsPage,
      problemsKeyword,
      problemsDifficulty,
      problemsTag,
      problemsSolved,
      problemsFacets,
      problemSync,
      problemDifficultyOptions,
      problemTagOptions,
      problemSolvedOptions,
      hasProblemsFilters,
      resetProblemsFilters,
      syncProblems,
      // Contests exports
      contests,
      contestFilter,
      contestSearch,
      isSyncingContests,
      filteredContests,
      liveContests,
      upcomingContests,
      pastContests,
      shownUpcomingContests,
      shownPastContests,
      showAllUpcoming,
      showAllPast,
      upcomingContestsCount,
      topUpcomingContests,
      getContestStatus,
      formatContestCountdown,
      formatCountdownTo,
      formatRemaining,
      formatContestDate,
      contestPlatformDot,
      contestPlatformLabel,
      loadContests,
      syncContestsNow
    };
  }
});

app.component("UiSelect", UiSelect);
app.mount("#app");
