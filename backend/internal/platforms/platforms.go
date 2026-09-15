package platforms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Platform     string
	CFHandle     string
	LuoguUID     string
	LeetCode     string
	AtCoder      string
	AcWingID     string
	LuoguCookie  string // kept for user-controlled authenticated requests only
	AcWingCookie string
}

type Submission struct {
	RawID        string
	ProblemID    string
	ProblemTitle string
	Verdict      string
	Tags         []string
	Difficulty   string
	Score        int
	SubmittedAt  time.Time
	URL          string
	Language     string
	Extra        map[string]any
}

type Problem struct {
	Platform   string   `json:"platform"`
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Difficulty string   `json:"difficulty,omitempty"`
	Rating     int      `json:"rating,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	URL        string   `json:"url,omitempty"`
}

type Contest struct {
	Platform        string `json:"platform"`
	ID              string `json:"id"`
	Name            string `json:"name"`
	StartTimestamp  int64  `json:"start_timestamp"`
	DurationSeconds int64  `json:"duration_seconds"`
	StartTime       string `json:"start_time"`
	DurationStr     string `json:"duration_str"`
	URL             string `json:"url"`
	RuleType        string `json:"rule_type,omitempty"`
}

type Profile struct {
	Platform string `json:"platform"`
	Handle   string `json:"handle"`
	Rating   string `json:"rating,omitempty"`
	Rank     string `json:"rank,omitempty"`
	Solved   int    `json:"solved"`
	// Note 承载"验证通过但有附带说明"的情况，例如洛谷缺 Cookie 时
	// 只能校验 UID、拿不到提交记录。
	Note string `json:"note,omitempty"`
}

type SyncResult struct {
	Platform    string
	Profile     Profile
	Submissions []Submission
	Message     string
}

type Client struct {
	HTTP *http.Client

	mu             sync.Mutex
	contestCache   []Contest
	contestCacheAt time.Time
	problemCache   map[string]problemCacheEntry

	// 出站节流：对同一域名的两次请求至少间隔 minInterval。
	// 洛谷有反爬风控，请求过密会被拒绝（严重时封 IP），
	// 所以把"别打太快"做成客户端层的硬约束，而不是散在各个调用点。
	gateMu      sync.Mutex
	lastHit     map[string]time.Time
	minInterval time.Duration

	// 洛谷题库分页元数据（perPage/total 全量一致），缓存避免每页重复取第一页。
	luoguMetaMu  sync.Mutex
	luoguPerPage int
	luoguTotal   int
	luoguMetaAt  time.Time
}

// beijingLoc 缓存 Asia/Shanghai 时区，避免数千行记录每条都读磁盘加载时区。
var (
	beijingLoc     *time.Location
	beijingLocOnce sync.Once
)

type problemCacheEntry struct {
	items []Problem
	at    time.Time
}

// defaultMinInterval 同域名两次请求的最小间隔。洛谷记录一页 20 条、
// 700 条历史约 35 页，按这个间隔整轮大约 30 秒，属于"慢但对端友好"。
const defaultMinInterval = 800 * time.Millisecond

func NewClient() *Client {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		// CheckRedirect：洛谷 C3VK 反爬挑战无 Cookie 时返回 302 回到同域并下发
		// Set-Cookie，必须停止自动跟随，让 luoguGetText 里的“带 cookie 手动重发”
		// 逻辑真正生效（默认自动跟随且无 CookieJar，会循环 10 次后报
		// "stopped after 10 redirects"，那套重发逻辑永远走不到）。
		// 同 host 的 302 视为挑战、停止跟随；跨 host 的正常跳转照常跟随，
		// 但保留 10 次上限防止死循环。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			if len(via) > 0 && via[len(via)-1].URL.Host == req.URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	// CODEACTIVITYHUB_HTTP_PROXY：为所有出站 OJ 请求指定代理（如 http://127.0.0.1:10808）。
	// 国内直连 codeforces 时通时断，超时率很高；未设置时保持 Go 默认行为
	// （尊重 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 环境变量）。
	if p := strings.TrimSpace(os.Getenv("CODEACTIVITYHUB_HTTP_PROXY")); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			if base, ok := http.DefaultTransport.(*http.Transport); ok {
				transport := base.Clone()
				transport.Proxy = http.ProxyURL(u)
				httpClient.Transport = transport
			}
		}
	}
	return &Client{
		HTTP:         httpClient,
		problemCache: map[string]problemCacheEntry{},
		lastHit:      map[string]time.Time{},
		minInterval:  configuredMinInterval(),
	}
}

// configuredMinInterval 允许用环境变量调整出站节流间隔（毫秒），
// 默认 800ms；设成 0 表示关闭节流（不建议，容易被站点风控）。
func configuredMinInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEACTIVITYHUB_MIN_INTERVAL_MS"))
	if raw == "" {
		return defaultMinInterval
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return defaultMinInterval
	}
	return time.Duration(ms) * time.Millisecond
}

// waitForSlot 保证对同一域名的请求之间有足够间隔；ctx 取消时立即返回。
func (c *Client) waitForSlot(ctx context.Context, endpoint string) error {
	interval := c.minInterval
	if interval <= 0 {
		return nil
	}
	host := endpoint
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	c.gateMu.Lock()
	wait := time.Duration(0)
	if last, ok := c.lastHit[host]; ok {
		if elapsed := time.Since(last); elapsed < interval {
			wait = interval - elapsed
		}
	}
	// 先占位再等待，避免并发请求同时通过节流
	c.lastHit[host] = time.Now().Add(wait)
	c.gateMu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// cachedContests 让频繁的页面切换不会反复请求上游赛程接口。
func (c *Client) cachedContests(fetch func() ([]Contest, error)) ([]Contest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.contestCache != nil && time.Since(c.contestCacheAt) < 5*time.Minute {
		return c.contestCache, nil
	}
	rows, err := fetch()
	if err != nil {
		return nil, err
	}
	c.contestCache = rows
	c.contestCacheAt = time.Now()
	return rows, nil
}

func (c *Client) cachedProblems(key string, fetch func() ([]Problem, error)) ([]Problem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.problemCache[key]; ok && time.Since(entry.at) < 10*time.Minute {
		return entry.items, nil
	}
	items, err := fetch()
	if err != nil {
		return nil, err
	}
	c.problemCache[key] = problemCacheEntry{items: items, at: time.Now()}
	return items, nil
}

// InvalidateContestCache 供用户手动“刷新比赛”时绕过缓存强制回源。
func (c *Client) InvalidateContestCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contestCache = nil
}

func beijing() *time.Location {
	beijingLocOnce.Do(func() {
		if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
			beijingLoc = loc
		} else {
			beijingLoc = time.FixedZone("CST", 8*3600)
		}
	})
	return beijingLoc
}

func contestStartTime(ts int64) string {
	return time.Unix(ts, 0).In(beijing()).Format("2006-01-02 15:04")
}

func durationString(sec int64) string {
	h, m := sec/3600, (sec%3600)/60
	if h >= 24 {
		return fmt.Sprintf("%dd%02dh", h/24, h%24)
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

func (c *Client) Verify(ctx context.Context, cfg Config) (Profile, error) {
	switch normalize(cfg.Platform) {
	case "codeforces":
		return c.verifyCodeforces(ctx, cfg.CFHandle)
	case "leetcode":
		return c.verifyLeetCode(ctx, cfg.LeetCode)
	case "atcoder":
		return c.verifyAtCoder(ctx, cfg.AtCoder)
	case "luogu":
		return c.verifyLuogu(ctx, cfg)
	case "acwing":
		return c.verifyAcWing(ctx, cfg.AcWingID)
	default:
		return Profile{}, fmt.Errorf("不支持的平台: %s", cfg.Platform)
	}
}

func (c *Client) SyncSubmissions(ctx context.Context, cfg Config) (SyncResult, error) {
	switch normalize(cfg.Platform) {
	case "codeforces":
		return c.syncCodeforces(ctx, cfg.CFHandle)
	case "leetcode":
		return c.syncLeetCode(ctx, cfg.LeetCode)
	case "atcoder":
		return c.syncAtCoder(ctx, cfg.AtCoder)
	case "luogu":
		return c.syncLuogu(ctx, cfg)
	case "acwing":
		return c.syncAcWing(ctx, cfg.AcWingID)
	default:
		return SyncResult{}, fmt.Errorf("不支持的平台: %s", cfg.Platform)
	}
}

// AllProblems 全量枚举平台题库。Codeforces/AtCoder 上游一次给全量（内存缓存后按页切），
// LeetCode/洛谷是真分页；洛谷固定 50 条/页且单页最多返回 50 条，所以统一按
// 50 条一页拉，直到凑满 total、某页为空或某页不满。每拉到一页就回调 onBatch，
// 调用方可以边拉边落库，长同步（洛谷约 350 页）中途失败也不全损。
func (c *Client) AllProblems(ctx context.Context, platform string, onBatch func(rows []Problem, done, total int)) error {
	const pageSize = 50
	done := 0
	total := 0
	for page := 1; page <= 2000; page++ {
		rows, t, err := c.Problems(ctx, platform, page, pageSize)
		if err != nil {
			return err
		}
		if page == 1 {
			total = t
		}
		if len(rows) == 0 {
			break
		}
		done += len(rows)
		if onBatch != nil {
			onBatch(rows, done, total)
		}
		if total > 0 && done >= total {
			break
		}
		if len(rows) < pageSize {
			break
		}
	}
	return nil
}

func (c *Client) Problems(ctx context.Context, platform string, page, limit int) ([]Problem, int, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 30
	}
	switch normalize(platform) {
	case "codeforces":
		return c.codeforcesProblems(ctx, page, limit)
	case "leetcode":
		return c.leetCodeProblems(ctx, page, limit)
	case "atcoder":
		return c.atCoderProblems(ctx, page, limit)
	case "luogu":
		return c.luoguProblems(ctx, page, limit)
	default:
		return nil, 0, fmt.Errorf("暂不支持题库平台: %s", platform)
	}
}

func (c *Client) Contests(ctx context.Context, platform string) ([]Contest, error) {
	switch normalize(platform) {
	case "all", "":
		return c.cachedContests(func() ([]Contest, error) {
			var all []Contest
			var firstErr error
			ok := 0
			for _, p := range []string{"codeforces", "atcoder"} {
				rows, err := c.contestsFromUpstream(ctx, p)
				if err != nil {
					// 单个平台失败不应让整个日历空掉，但要记住错误：
					// 全部失败时必须向上报出错误，否则空结果会被缓存 5 分钟，
					// 用户只看到"暂无比赛"却不知道是上游挂了。
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				ok++
				all = append(all, rows...)
			}
			if ok == 0 && firstErr != nil {
				return nil, firstErr
			}
			return all, nil
		})
	case "codeforces", "atcoder":
		return c.contestsFromUpstream(ctx, platform)
	default:
		return nil, fmt.Errorf("暂不支持比赛数据平台: %s", platform)
	}
}

// contestsFromUpstream 只保留"近 7 天内结束 + 未开始"的比赛，
// 历史全量数据对看板没有意义，且会让前端渲染数千行表格。
func (c *Client) contestsFromUpstream(ctx context.Context, platform string) ([]Contest, error) {
	var rows []Contest
	switch platform {
	case "codeforces":
		var err error
		rows, err = c.codeforcesContests(ctx)
		if err != nil {
			return nil, err
		}
	case "atcoder":
		var err error
		rows, err = c.atCoderContests(ctx)
		if err != nil {
			return nil, err
		}
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	out := make([]Contest, 0, len(rows))
	for _, row := range rows {
		// kenkoooo 数据含 start=0、时长数百年的常驻赛事（如典型 90 题），按正式比赛过滤。
		if row.StartTimestamp > 0 && row.DurationSeconds <= 30*24*3600 && row.StartTimestamp+row.DurationSeconds >= cutoff {
			out = append(out, row)
		}
	}
	return out, nil
}

func normalize(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func (c *Client) getText(ctx context.Context, endpoint string) (string, error) {
	if err := c.waitForSlot(ctx, endpoint); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/json")
	req.Header.Set("User-Agent", "CodeActivityHub/1.0")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("上游响应 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return string(body), err
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	if err := c.waitForSlot(ctx, endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CodeActivityHub/1.0")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("上游响应 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
}

func (c *Client) postGraphQL(ctx context.Context, endpoint, query string, variables map[string]any, out any) error {
	if err := c.waitForSlot(ctx, endpoint); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CodeActivityHub/1.0")
	// 力扣（尤其中国站）会校验来源，缺 Referer 容易被挡
	if u, perr := url.Parse(endpoint); perr == nil && u.Scheme != "" && u.Host != "" {
		req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("上游响应 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GraphQL: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *Client) verifyCodeforces(ctx context.Context, handle string) (Profile, error) {
	if strings.TrimSpace(handle) == "" {
		return Profile{}, fmt.Errorf("Codeforces handle 不能为空")
	}
	var out struct {
		Status  string `json:"status"`
		Comment string `json:"comment"`
		Result  []struct {
			Handle string `json:"handle"`
			Rating int    `json:"rating"`
			Rank   string `json:"rank"`
		} `json:"result"`
	}
	err := c.getJSON(ctx, "https://codeforces.com/api/user.info?handles="+url.QueryEscape(handle), &out)
	if err != nil {
		return Profile{}, err
	}
	if out.Status != "OK" || len(out.Result) == 0 {
		return Profile{}, fmt.Errorf("Codeforces: %s", out.Comment)
	}
	u := out.Result[0]
	return Profile{Platform: "codeforces", Handle: u.Handle, Rating: strconv.Itoa(u.Rating), Rank: u.Rank}, nil
}

func (c *Client) syncCodeforces(ctx context.Context, handle string) (SyncResult, error) {
	profile, err := c.verifyCodeforces(ctx, handle)
	if err != nil {
		return SyncResult{}, err
	}
	var out struct {
		Status string `json:"status"`
		Result []struct {
			ID        int `json:"id"`
			ContestID int `json:"contestId"`
			Problem   struct {
				Index  string   `json:"index"`
				Name   string   `json:"name"`
				Rating int      `json:"rating"`
				Tags   []string `json:"tags"`
			} `json:"problem"`
			Verdict  string `json:"verdict"`
			Creation int64  `json:"creationTimeSeconds"`
			Lang     string `json:"programmingLanguage"`
		} `json:"result"`
	}
	endpoint := "https://codeforces.com/api/user.status?handle=" + url.QueryEscape(handle) + "&from=1&count=1000"
	if err := c.getJSON(ctx, endpoint, &out); err != nil {
		return SyncResult{}, err
	}
	if out.Status != "OK" {
		return SyncResult{}, fmt.Errorf("Codeforces submission API 返回异常")
	}
	rows := make([]Submission, 0, len(out.Result))
	for _, s := range out.Result {
		pid := fmt.Sprintf("%d%s", s.ContestID, s.Problem.Index)
		rows = append(rows, Submission{RawID: strconv.Itoa(s.ID), ProblemID: pid, ProblemTitle: s.Problem.Name, Verdict: cfVerdict(s.Verdict), Tags: s.Problem.Tags, Score: s.Problem.Rating, SubmittedAt: time.Unix(s.Creation, 0), URL: fmt.Sprintf("https://codeforces.com/contest/%d/submission/%d", s.ContestID, s.ID), Language: s.Lang, Extra: map[string]any{"contest_id": s.ContestID, "index": s.Problem.Index}})
	}
	return SyncResult{Platform: "codeforces", Profile: profile, Submissions: rows, Message: fmt.Sprintf("从 Codeforces 获取 %d 条提交", len(rows))}, nil
}

func cfVerdict(v string) string {
	switch strings.ToUpper(v) {
	case "OK":
		return "AC"
	case "WRONG_ANSWER":
		return "WA"
	case "TIME_LIMIT_EXCEEDED":
		return "TLE"
	case "MEMORY_LIMIT_EXCEEDED":
		return "MLE"
	case "COMPILATION_ERROR":
		return "CE"
	case "RUNTIME_ERROR":
		return "RE"
	default:
		return strings.ToUpper(v)
	}
}

func (c *Client) codeforcesProblems(ctx context.Context, page, limit int) ([]Problem, int, error) {
	items, err := c.cachedProblems("codeforces", func() ([]Problem, error) { return c.codeforcesProblemsAll(ctx) })
	if err != nil {
		return nil, 0, err
	}
	return paginateProblems(items, page, limit), len(items), nil
}

func (c *Client) codeforcesProblemsAll(ctx context.Context) ([]Problem, error) {
	var out struct {
		Status string `json:"status"`
		Result struct {
			Problems []struct {
				ContestID   int `json:"contestId"`
				Index, Name string
				Rating      int
				Tags        []string
			} `json:"problems"`
		} `json:"result"`
	}
	if err := c.getJSON(ctx, "https://codeforces.com/api/problemset.problems", &out); err != nil {
		return nil, err
	}
	if out.Status != "OK" {
		return nil, fmt.Errorf("Codeforces problemset API 返回异常: %s", strings.TrimSpace(out.Status))
	}
	rows := make([]Problem, 0, len(out.Result.Problems))
	for _, p := range out.Result.Problems {
		id := fmt.Sprintf("%d%s", p.ContestID, p.Index)
		rows = append(rows, Problem{Platform: "codeforces", ID: id, Title: p.Name, Rating: p.Rating, Tags: p.Tags, URL: fmt.Sprintf("https://codeforces.com/contest/%d/problem/%s", p.ContestID, p.Index)})
	}
	return rows, nil
}

func paginateProblems(items []Problem, page, limit int) []Problem {
	start := (page - 1) * limit
	if start >= len(items) || start < 0 {
		return []Problem{}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func (c *Client) codeforcesContests(ctx context.Context) ([]Contest, error) {
	var out struct {
		Status string `json:"status"`
		Result []struct {
			ID       int    `json:"id"`
			Name     string `json:"name"`
			Phase    string `json:"phase"`
			Start    int64  `json:"startTimeSeconds"`
			Duration int64  `json:"durationSeconds"`
			Type     string `json:"type"`
		} `json:"result"`
	}
	if err := c.getJSON(ctx, "https://codeforces.com/api/contest.list?gym=false", &out); err != nil {
		return nil, err
	}
	if out.Status != "OK" {
		return nil, fmt.Errorf("Codeforces contest API 返回异常: %s", strings.TrimSpace(out.Status))
	}
	rows := make([]Contest, 0, len(out.Result))
	for _, x := range out.Result {
		rows = append(rows, Contest{Platform: "codeforces", ID: strconv.Itoa(x.ID), Name: x.Name, StartTimestamp: x.Start, StartTime: contestStartTime(x.Start), DurationSeconds: x.Duration, DurationStr: durationString(x.Duration), URL: fmt.Sprintf("https://codeforces.com/contests/%d", x.ID), RuleType: x.Type})
	}
	return rows, nil
}

func (c *Client) verifyLeetCode(ctx context.Context, username string) (Profile, error) {
	if strings.TrimSpace(username) == "" {
		return Profile{}, fmt.Errorf("LeetCode 用户名不能为空")
	}
	// 注意：LeetCode 已移除 userPublicProfile 字段（2026-09 实测返回 400），
	// 现用 matchedUser + submitStats；用户名不存在时 matchedUser 为 null。
	const q = `query($username:String!){ matchedUser(username:$username){ username profile{ranking} submitStats{acSubmissionNum{difficulty count}} } }`
	var out struct {
		User struct {
			Username string `json:"username"`
			Profile  struct {
				Ranking int `json:"ranking"`
			} `json:"profile"`
			Stats struct {
				AC []struct {
					Difficulty string `json:"difficulty"`
					Count      int    `json:"count"`
				} `json:"acSubmissionNum"`
			} `json:"submitStats"`
		} `json:"matchedUser"`
	}
	qErr := c.postGraphQL(ctx, "https://leetcode.com/graphql", q, map[string]any{"username": username}, &out)
	if qErr == nil && out.User.Username != "" {
		solved := 0
		for _, x := range out.User.Stats.AC {
			if strings.EqualFold(x.Difficulty, "All") {
				solved = x.Count
			}
		}
		return Profile{Platform: "leetcode", Handle: out.User.Username, Rating: strconv.Itoa(out.User.Profile.Ranking), Solved: solved}, nil
	}

	// 国际站没有这个用户就再试力扣中国站：两站 schema 不同，
	// 中国站的用户查询是 userProfilePublicProfile(userSlug:)。
	const cnQ = `query($userSlug:String!){ userProfilePublicProfile(userSlug:$userSlug){ username siteRanking } }`
	var cn struct {
		Profile struct {
			Username    string `json:"username"`
			SiteRanking int    `json:"siteRanking"`
		} `json:"userProfilePublicProfile"`
	}
	if err := c.postGraphQL(ctx, "https://leetcode.cn/graphql/", cnQ, map[string]any{"userSlug": username}, &cn); err == nil && cn.Profile.Username != "" {
		return Profile{Platform: "leetcode", Handle: cn.Profile.Username,
			Rating: strconv.Itoa(cn.Profile.SiteRanking), Note: "力扣中国站（leetcode.cn）"}, nil
	}

	return Profile{}, fmt.Errorf("LeetCode 用户不存在：请填个人主页 URL 里 /u/ 后面那段用户名（英文/数字），不要填昵称。" +
		"国际站 leetcode.com 与力扣中国站 leetcode.cn 都已尝试")
}

// 力扣有两套站：国际站（leetcode.com）与力扣中国站（leetcode.cn），
// 两者的 schema 与可用接口都不同。verifyLeetCode 探测后会在 Note 里写明站点，
// 这里据此选择 GraphQL 端点与网页前缀。
func leetCodeSiteOf(p Profile) (graphqlEP, webBase string) {
	if strings.Contains(p.Note, "leetcode.cn") {
		return "https://leetcode.cn/graphql/", "https://leetcode.cn"
	}
	return "https://leetcode.com/graphql", "https://leetcode.com"
}

func (c *Client) syncLeetCode(ctx context.Context, username string) (SyncResult, error) {
	profile, err := c.verifyLeetCode(ctx, username)
	if err != nil {
		return SyncResult{}, err
	}
	graphqlEP, _ := leetCodeSiteOf(profile)
	// 力扣中国站没有公开的提交记录接口（recentAcSubmissionList / submissions 等都不存在，
	// 2026-09 实测），提交记录依赖浏览器脚本上报（脚本 @match 已覆盖 leetcode.cn）。
	if strings.Contains(graphqlEP, "leetcode.cn") {
		return SyncResult{Platform: "leetcode", Profile: profile,
			Message: "力扣中国站不提供公开的提交记录接口，提交记录请使用篡改猴脚本接入（已支持）"}, nil
	}
	const q = `query($username:String!,$limit:Int!){ recentAcSubmissionList(username:$username,limit:$limit){ id title titleSlug timestamp lang statusDisplay } }`
	var out struct {
		Rows []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Slug      string `json:"titleSlug"`
			Timestamp string `json:"timestamp"`
			Lang      string `json:"lang"`
			Status    string `json:"statusDisplay"`
		} `json:"recentAcSubmissionList"`
	}
	if err := c.postGraphQL(ctx, graphqlEP, q, map[string]any{"username": username, "limit": 1000}, &out); err != nil {
		return SyncResult{}, err
	}
	// 提交接口只给 titleSlug，先批量换成题号，与题库表（题号）保持一致
	slugs := make([]string, 0, len(out.Rows))
	seen := make(map[string]bool, len(out.Rows))
	for _, x := range out.Rows {
		if x.Slug != "" && !seen[x.Slug] {
			seen[x.Slug] = true
			slugs = append(slugs, x.Slug)
		}
	}
	if len(slugs) > leetCodeSlugResolveLimit {
		slugs = slugs[:leetCodeSlugResolveLimit]
	}
	frontendIDs := c.leetCodeFrontendIDs(ctx, slugs)

	rows := make([]Submission, 0, len(out.Rows))
	for _, x := range out.Rows {
		ts, _ := strconv.ParseInt(x.Timestamp, 10, 64)
		pid := x.Slug
		if v, ok := frontendIDs[x.Slug]; ok && v != "" {
			pid = v
		}
		rows = append(rows, Submission{RawID: x.ID, ProblemID: pid, ProblemTitle: x.Title, Verdict: "AC", SubmittedAt: time.Unix(ts, 0), URL: "https://leetcode.com/problems/" + x.Slug + "/", Language: x.Lang})
	}
	return SyncResult{Platform: "leetcode", Profile: profile, Submissions: rows, Message: fmt.Sprintf("从 LeetCode 获取 %d 条已通过提交", len(rows))}, nil
}

// 单次同步最多解析多少个 slug：受 800ms 节流限制，每 25 个一次请求，
// 300 个约 12 次请求（≈10 秒），超出的用 slug 兜底，避免同步被拖太久。
const leetCodeSlugResolveLimit = 300

// leetCodeFrontendIDs 把力扣的 titleSlug 批量换成前端展示的题号（questionFrontendId）。
// 题库表存的就是题号，提交记录统一成题号后两边才能对上：看板显示 "1. Two Sum"
// 而不是英文 slug，用户体验与"题号"直觉一致。
// 用 GraphQL 别名批量查询（一次 25 个）；拿不到的保持 slug 兜底，不让同步失败。
func (c *Client) leetCodeFrontendIDs(ctx context.Context, slugs []string) map[string]string {
	result := make(map[string]string, len(slugs))
	const perBatch = 25
	for i := 0; i < len(slugs); i += perBatch {
		if err := ctx.Err(); err != nil {
			return result
		}
		end := i + perBatch
		if end > len(slugs) {
			end = len(slugs)
		}
		group := slugs[i:end]
		q := "query{"
		for j, s := range group {
			q += fmt.Sprintf(" q%d: question(titleSlug:%q){ questionFrontendId }", j, s)
		}
		q += "}"
		var out map[string]struct {
			FrontendID string `json:"questionFrontendId"`
		}
		if err := c.postGraphQL(ctx, "https://leetcode.com/graphql", q, map[string]any{}, &out); err != nil {
			// 整批失败就用 slug 兜底，不影响主流程
			continue
		}
		for j, s := range group {
			if v, ok := out[fmt.Sprintf("q%d", j)]; ok && v.FrontendID != "" {
				result[s] = v.FrontendID
			}
		}
	}
	return result
}

func (c *Client) leetCodeProblems(ctx context.Context, page, limit int) ([]Problem, int, error) {
	// 国际站优先；国际站取不到（网络/风控）时回落到力扣中国站——
	// 中国站仍是同一套 V2 schema，字段一致（2026-09 实测）。
	rows, total, err := c.leetCodeProblemsFrom(ctx, "https://leetcode.com/graphql", "https://leetcode.com", false, page, limit)
	if err == nil && len(rows) > 0 {
		return rows, total, nil
	}
	cnRows, cnTotal, cnErr := c.leetCodeProblemsFrom(ctx, "https://leetcode.cn/graphql/", "https://leetcode.cn", true, page, limit)
	if cnErr != nil {
		if err == nil {
			return nil, 0, cnErr
		}
		return nil, 0, err
	}
	return cnRows, cnTotal, nil
}

// leetCodeProblemsFrom 从指定站点取一页题库。
// 注意：problemsetQuestionList 已被 LeetCode 移除（2026-09 实测报错），
// 改用 problemsetQuestionListV2；它没有 total 字段，总数在 totalLength。
// chinese 为真时优先用中文标题（中国站的 translatedTitle）。
func (c *Client) leetCodeProblemsFrom(ctx context.Context, graphqlEP, webBase string, chinese bool, page, limit int) ([]Problem, int, error) {
	const q = `query($skip:Int!,$limit:Int!){ problemsetQuestionListV2(skip:$skip,limit:$limit){ questions{questionFrontendId title translatedTitle titleSlug difficulty topicTags{name}} totalLength hasMore } }`
	var out struct {
		List struct {
			Total     int `json:"totalLength"`
			Questions []struct {
				FrontendID      string                  `json:"questionFrontendId"`
				Title           string                  `json:"title"`
				TranslatedTitle string                  `json:"translatedTitle"`
				Slug            string                  `json:"titleSlug"`
				Difficulty      string                  `json:"difficulty"`
				Tags            []struct{ Name string } `json:"topicTags"`
			} `json:"questions"`
		} `json:"problemsetQuestionListV2"`
	}
	if err := c.postGraphQL(ctx, graphqlEP, q, map[string]any{"skip": (page - 1) * limit, "limit": limit}, &out); err != nil {
		return nil, 0, err
	}
	rows := make([]Problem, 0, len(out.List.Questions))
	for _, x := range out.List.Questions {
		tags := make([]string, 0, len(x.Tags))
		for _, t := range x.Tags {
			tags = append(tags, t.Name)
		}
		title := x.Title
		if chinese && strings.TrimSpace(x.TranslatedTitle) != "" {
			title = x.TranslatedTitle
		}
		rows = append(rows, Problem{Platform: "leetcode", ID: x.FrontendID, Title: title, Difficulty: x.Difficulty, Tags: tags, URL: webBase + "/problems/" + x.Slug + "/"})
	}
	return rows, out.List.Total, nil
}

func (c *Client) atCoderContests(ctx context.Context) ([]Contest, error) {
	var raw []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Start    int64  `json:"start_epoch_second"`
		Duration int64  `json:"duration_second"`
	}
	if err := c.getJSON(ctx, "https://kenkoooo.com/atcoder/resources/contests.json", &raw); err != nil {
		return nil, err
	}
	rows := make([]Contest, 0, len(raw))
	for _, x := range raw {
		rows = append(rows, Contest{Platform: "atcoder", ID: x.ID, Name: x.Title, StartTimestamp: x.Start, StartTime: contestStartTime(x.Start), DurationSeconds: x.Duration, DurationStr: durationString(x.Duration), URL: "https://atcoder.jp/contests/" + x.ID})
	}
	return rows, nil
}
func (c *Client) atCoderProblems(ctx context.Context, page, limit int) ([]Problem, int, error) {
	items, err := c.cachedProblems("atcoder", func() ([]Problem, error) { return c.atCoderProblemsAll(ctx) })
	if err != nil {
		return nil, 0, err
	}
	return paginateProblems(items, page, limit), len(items), nil
}

func (c *Client) atCoderProblemsAll(ctx context.Context) ([]Problem, error) {
	var raw []struct {
		ID           string   `json:"id"`
		Title        string   `json:"title"`
		ContestID    string   `json:"contest_id"`
		ProblemIndex string   `json:"problem_index"`
		Tags         []string `json:"tags"`
	}
	if err := c.getJSON(ctx, "https://kenkoooo.com/atcoder/resources/merged-problems.json", &raw); err != nil {
		return nil, err
	}
	rows := make([]Problem, 0, len(raw))
	for _, x := range raw {
		rows = append(rows, Problem{Platform: "atcoder", ID: x.ID, Title: x.Title, Tags: x.Tags, URL: "https://atcoder.jp/contests/" + x.ContestID + "/tasks/" + x.ID})
	}
	return rows, nil
}

type atCoderSubmission struct {
	ID                  int     `json:"id"`
	ContestID           string  `json:"contest_id"`
	ProblemID           string  `json:"problem_id"`
	EpochSecond         int64   `json:"epoch_second"`
	Result              string  `json:"result"`
	ProgrammingLanguage string  `json:"language"`
	Point               float64 `json:"point"`
}

func (c *Client) atCoderSubmissions(ctx context.Context, handle string) ([]atCoderSubmission, error) {
	if strings.TrimSpace(handle) == "" {
		return nil, fmt.Errorf("AtCoder 用户名不能为空")
	}
	endpoint := "https://kenkoooo.com/atcoder/atcoder-api/v3/user/submissions?user=" + url.QueryEscape(handle) + "&from_second=0"
	var raw []atCoderSubmission
	if err := c.getJSON(ctx, endpoint, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) verifyAtCoder(ctx context.Context, handle string) (Profile, error) {
	raw, err := c.atCoderSubmissions(ctx, handle)
	if err != nil {
		return Profile{}, err
	}
	seen := map[string]bool{}
	for _, item := range raw {
		if strings.EqualFold(item.Result, "AC") {
			seen[item.ProblemID] = true
		}
	}
	return Profile{Platform: "atcoder", Handle: handle, Solved: len(seen)}, nil
}
func (c *Client) syncAtCoder(ctx context.Context, handle string) (SyncResult, error) {
	raw, err := c.atCoderSubmissions(ctx, handle)
	if err != nil {
		return SyncResult{}, err
	}
	profile, err := c.verifyAtCoder(ctx, handle)
	if err != nil {
		return SyncResult{}, err
	}
	rows := make([]Submission, 0, len(raw))
	for _, item := range raw {
		verdict := strings.ToUpper(strings.TrimSpace(item.Result))
		if verdict == "" {
			verdict = "UNKNOWN"
		}
		rows = append(rows, Submission{RawID: strconv.Itoa(item.ID), ProblemID: item.ProblemID, ProblemTitle: item.ProblemID,
			Verdict: atCoderVerdict(verdict), Score: int(item.Point), SubmittedAt: time.Unix(item.EpochSecond, 0),
			URL: "https://atcoder.jp/contests/" + item.ContestID + "/submissions/" + strconv.Itoa(item.ID), Language: item.ProgrammingLanguage,
			Extra: map[string]any{"contest_id": item.ContestID, "result": item.Result, "point": item.Point}})
	}
	return SyncResult{Platform: "atcoder", Profile: profile, Submissions: rows, Message: fmt.Sprintf("从 AtCoder 获取 %d 条提交", len(rows))}, nil
}

func atCoderVerdict(v string) string {
	switch v {
	case "AC":
		return "AC"
	case "WA":
		return "WA"
	case "TLE":
		return "TLE"
	case "MLE":
		return "MLE"
	case "CE":
		return "CE"
	case "RE":
		return "RE"
	default:
		return v
	}
}

// ErrUnsupportedVerify 表示该平台没有可用的公开校验接口。
// 调用方应当把它当成"不支持"而不是"失败"——否则会在设置页留下一条
// 永远无法消除的 error 状态（用户既配不好也没法清）。
var ErrUnsupportedVerify = errors.New("该平台没有公开校验接口")

// ---------------------------------------------------------------------------
// 洛谷：用户主页是公开的（可校验 UID），提交记录需要 __client_id Cookie。
// 另外洛谷对所有请求有 C3VK 反爬 cookie 挑战：无 cookie 时返回 302 回到同一
// URL 并下发 Set-Cookie，必须带上它重发一次，否则永远拿不到内容。
// ---------------------------------------------------------------------------

const luoguOrigin = "https://www.luogu.com.cn"

// luoguMaxPages 同步提交时单轮最多翻多少页（上限，防止一次拉爆；695 条约需 35 页）。
// 设为包级变量是为了让临时实时测试能临时调小以控制请求数，正常同步不受影响。
var luoguMaxPages = 40

// cookiePairPattern 匹配 "名字=值" 形态的 cookie 片段（用于识别用户是不是整段粘贴的）。
var cookiePairPattern = regexp.MustCompile(`^\s*(?:__client_id|_uid|C3VK|__cf_bm)\s*=`)

// luoguCookieHeader 把用户填进"Cookie"框的内容规范成可直接放进请求头的字符串。
// 支持三种粘贴形态：
//
//	eyJhbGci...                        → __client_id=eyJhbGci...
//	__client_id=eyJhbGci...            → 原样（不能再套一层前缀，否则发出去是
//	                                     "__client_id=__client_id=..."，洛谷会当成未登录）
//	__client_id=eyJ...; _uid=12345     → 原样，一次带齐
func luoguCookieHeader(input string) string {
	raw := strings.ReplaceAll(input, "\r", "")
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "\"'"))
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "__client_id=") || cookiePairPattern.MatchString(raw) {
		return raw
	}
	return "__client_id=" + raw
}

// logLuoguAuthHint 把"我们到底发了什么"打到终端，便于一眼看出是格式问题还是凭据问题。
// 只打印 Cookie 的"名字(值长度)"摘要与总长度，绝不泄露凭据内容。
func logLuoguAuthHint(endpoint, cookieHeader, upstreamMessage string) {
	fmt.Printf("[luogu] %s 认证失败：%s（已发送 Cookie 摘要 %s，总长度 %d）\n", endpoint, upstreamMessage, luoguCookieSummary(cookieHeader), len(cookieHeader))
}

// luoguContext 从洛谷响应体里解析内嵌数据。洛谷 2026-09 改版为 columba/lentille
// 架构后，数据内嵌在 <script id="lentille-context" type="application/json"> 中
// （SPA 骨架 HTML 本身 HTTP 200，但旧版 ?_contentOnly=1 的纯 JSON 接口已废弃）。
// 本函数优先提取该 script；若响应本身就是一个 JSON 对象（老版本/接口兼容），
// 也直接解析；两者都失败再区分"疑似反爬拦截"还是"页面已改版"给出清晰报错。
//
// 返回的是完整"信封"（含 template / data / errorCode 等顶层字段），调用方再按需
// 用 luoguDataOf 取出 data 业务对象。
func luoguContext(raw []byte) (map[string]any, error) {
	s := string(raw)
	const begin = `<script id="lentille-context" type="application/json">`
	if start := strings.Index(s, begin); start >= 0 {
		start += len(begin)
		rest := s[start:]
		if end := strings.Index(rest, `</script>`); end >= 0 {
			var env map[string]any
			if err := json.Unmarshal([]byte(rest[:end]), &env); err == nil {
				return env, nil
			}
		}
	}
	// 整个响应就是 JSON（老版本行为）：直接解析。
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err == nil {
		return env, nil
	}
	// 两者都失败：区分"疑似反爬拦截（有 HTML 骨架但没数据）"和"页面已改版"。
	if strings.Contains(s, "<!DOCTYPE") || strings.Contains(s, "<html") {
		return nil, fmt.Errorf("洛谷页面未内嵌数据（疑似反爬拦截，需带上挑战 Cookie 重发）")
	}
	return nil, fmt.Errorf("洛谷响应既不是内嵌 JSON 也不是合法 JSON（页面可能已改版）")
}

// luoguDataOf 从信封里取出 data 业务对象；缺失返回 (nil, false)。
func luoguDataOf(env map[string]any) (map[string]any, bool) {
	d, ok := env["data"].(map[string]any)
	return d, ok && d != nil
}

// luoguUserOf 取出页面级 user 对象：新版放在 data.user，老版本可能直接放信封顶层，
// 两处都找，避免因为嵌套位置变化导致误判"用户不存在"。
func luoguUserOf(env map[string]any) (map[string]any, bool) {
	if d, ok := luoguDataOf(env); ok {
		if u, ok := d["user"].(map[string]any); ok {
			return u, true
		}
	}
	if u, ok := env["user"].(map[string]any); ok {
		return u, true
	}
	return nil, false
}

// luoguEnvelopeOK 统一判断洛谷信封是否成功：
//   - 老版本（columba）带显式 errorCode：非 0 视为业务错误；
//   - 新版本（lentille）已无 errorCode，只要 data 存在且非空即视为成功。
func luoguEnvelopeOK(env map[string]any) error {
	if code, ok := luoguErrorCode(env); ok {
		if code != 0 {
			return fmt.Errorf("洛谷：%s", luoguString(env["errorMessage"]))
		}
		return nil
	}
	if _, ok := luoguDataOf(env); ok {
		return nil
	}
	return fmt.Errorf("洛谷：响应缺少 data（可能为反爬拦截或页面已改版）")
}

// luoguData 取洛谷页面并解析内嵌的 columba/lentille 数据信封（envelope）。
// 信封包含 template / data / errorCode(老版本) 等字段；data 才是真实业务数据。
// 注意洛谷对"找不到用户"这类业务错误可能用 404 返回同样的信封，
// 所以不能只看状态码，要交给调用方按 luoguEnvelopeOK 判断。
func (c *Client) luoguData(ctx context.Context, path, luoguCookie string) (map[string]any, error) {
	return c.luoguDataWithHeader(ctx, path, luoguCookieHeader(luoguCookie))
}

func (c *Client) luoguDataWithHeader(ctx context.Context, path, cookieHeader string) (map[string]any, error) {
	body, status, err := c.luoguGetText(ctx, luoguOrigin+path, cookieHeader)
	if err != nil {
		return nil, err
	}
	env, err := luoguContext([]byte(body))
	if err != nil {
		if status != http.StatusOK {
			return nil, fmt.Errorf("%w（HTTP %d）", err, status)
		}
		return nil, err
	}
	return env, nil
}

// mergeCookieHeader 把挑战下发的 cookie 片段合并进已有 Cookie 头，同名 cookie 取最新值，
// 避免 C3VK 挑战多次下发后累积成 "C3VK=a; C3VK=b"。
func mergeCookieHeader(existing string, challenge ...string) string {
	if len(challenge) == 0 {
		return existing
	}
	if existing == "" {
		return strings.Join(challenge, "; ")
	}
	values := map[string]string{}
	order := []string{}
	for _, seg := range strings.Split(existing, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, val, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = strings.TrimSpace(val)
	}
	for _, seg := range challenge {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, val, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = strings.TrimSpace(val)
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

// luoguGetText 处理 302 反爬挑战后返回页面正文与状态码。cookieHeader 是已规范化的 Cookie 头。
//
// 关键点：Client 上配置了 CheckRedirect，遇到同 host 的 302（C3VK 挑战）会停止自动跟随并
// 把 302 响应交回这里，于是下面“读到新下发的 Set-Cookie、合并后重发”的逻辑才能真正生效。
func (c *Client) luoguGetText(ctx context.Context, endpoint, cookieHeader string) (string, int, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.waitForSlot(ctx, endpoint); err != nil {
			return "", 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("Accept", "text/html,application/json")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		req.Header.Set("User-Agent", "CodeActivityHub/1.0")
		if cookieHeader != "" {
			req.Header.Set("Cookie", cookieHeader)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return "", 0, err
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()

		if res.StatusCode == http.StatusFound || res.StatusCode == http.StatusMovedPermanently || res.StatusCode == http.StatusTemporaryRedirect {
			challenge := []string{}
			for _, ck := range res.Cookies() {
				challenge = append(challenge, ck.Name+"="+ck.Value)
			}
			if len(challenge) == 0 {
				return "", res.StatusCode, fmt.Errorf("洛谷返回 %d 重定向但没有下发校验 cookie", res.StatusCode)
			}
			// 同名 cookie 取挑战最新值，避免重复累积
			cookieHeader = mergeCookieHeader(cookieHeader, challenge...)
			continue
		}
		return string(body), res.StatusCode, nil
	}
	return "", 0, fmt.Errorf("洛谷反爬校验未通过，请稍后重试")
}

func luoguString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	}
	return ""
}

func luoguInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

// luoguErrorCode 读取洛谷信封顶层的 errorCode（老版本 columba 字段）。
// 新版本（lentille）已无此字段，调用方改用 luoguEnvelopeOK 统一判断成功与否。
func luoguErrorCode(env map[string]any) (int, bool) {
	v, ok := env["errorCode"]
	if !ok {
		return 0, false
	}
	return luoguInt(v), true
}

// luoguVerdict 洛谷记录状态码 → 看板判定。
// 对照洛谷帮助中心与社区维护的 RecordStatus 定义（vscode-luogu-developer）：
//
//	0 Waiting / 1 Judging / 2 CE / 3 OLE / 4 MLE / 5 TLE / 6 WA / 7 RE
//	11 UKE(Unknown Error) / 12 AC / 14 Unaccepted（未通过，含部分分）
//	21/22/23 Hack 相关、-1 隐藏结果 —— 这些不是真实提交，返回空串由调用方跳过。
//
// 返回空串表示"认不出来"，调用方必须跳过而不是当成未通过，避免脏数据进错题本。
func luoguVerdict(status int) string {
	switch status {
	case 0, 1: // Waiting / Judging
		return "PENDING"
	case 2:
		return "CE"
	case 3:
		return "OLE"
	case 4:
		return "MLE"
	case 5:
		return "TLE"
	case 6:
		return "WA"
	case 7:
		return "RE"
	case 11:
		return "UKE"
	case 12:
		return "AC"
	case 14:
		return "UNACCEPTED"
	}
	return ""
}

var luoguDifficulties = map[int]string{
	0: "暂无评定", 1: "入门", 2: "普及−", 3: "普及/提高−",
	4: "普及+/提高", 5: "提高+/省选−", 6: "省选/NOI−", 7: "NOI/NOI+/CTSC",
}

// luoguCookieSummary 只输出 cookie 的"名字(值长度)"，用于诊断到底带上了什么，
// 不会泄露凭据内容。
func luoguCookieSummary(header string) string {
	if strings.TrimSpace(header) == "" {
		return "（空）"
	}
	parts := make([]string, 0, 4)
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, value, found := strings.Cut(seg, "=")
		if !found {
			parts = append(parts, seg)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%d)", strings.TrimSpace(name), len(value)))
	}
	return strings.Join(parts, ", ")
}

// luoguAuthedCookie 找出"能真正读到记录"的 Cookie 头，并给出一句可直接展示的说明。
//
// 洛谷的鉴权要求 __client_id 与 _uid 成对出现（VJudge 绑定、pyluog、pyLuogu
// 等第三方实现都是两个一起用），所以只要用户没带 _uid，我们就用账号里的 UID 补上，
// 并且优先试这一组。失败时把每次尝试的结果如实报出来，而不是笼统说一句"Cookie 失效"。
func (c *Client) luoguAuthedCookie(ctx context.Context, uid, cookie string) (string, string) {
	base := luoguCookieHeader(cookie)
	if base == "" {
		return "", "未配置 Cookie，无法读取提交记录（历史同步不可用）"
	}
	uid = strings.TrimSpace(uid)
	type attempt struct{ label, header string }
	attempts := make([]attempt, 0, 2)
	if uid != "" && !strings.Contains(base, "_uid=") {
		attempts = append(attempts, attempt{"__client_id + _uid", base + "; _uid=" + uid})
	}
	attempts = append(attempts, attempt{"仅 __client_id", base})

	path := "/record/list?user=" + url.QueryEscape(uid) + "&page=1"
	reasons := make([]string, 0, len(attempts))
	for _, at := range attempts {
		env, err := c.luoguDataWithHeader(ctx, path, at.header)
		if err != nil {
			reasons = append(reasons, at.label+"："+err.Error())
			continue
		}
		if err := luoguEnvelopeOK(env); err != nil {
			logLuoguAuthHint("/record/list "+at.label, at.header, err.Error())
			reasons = append(reasons, at.label+"："+err.Error())
			continue
		}
		// 新版本以能否拿到 user 为成功标志：记录列表页的 user 即 ?user=uid 对应用户，
		// 能取到说明页面正常返回、未命中反爬/未授权拦截。不再看已废弃的 errorCode。
		if _, ok := luoguUserOf(env); !ok {
			reasons = append(reasons, at.label+"：响应缺少 user（可能为反爬拦截或凭证无效）")
			continue
		}
		return at.header, "凭证有效，历史同步可用（" + at.label + "）"
	}
	return "", fmt.Sprintf("洛谷未认可该凭证（已发送 %s）—— %s。请在 F12 → Network → 任意 www.luogu.com.cn 请求 → Request Headers 里复制完整的 cookie 值",
		luoguCookieSummary(base), strings.Join(reasons, "；"))
}

// luoguProfile 只用公开用户主页取昵称/题数/排名（1 次请求）。
func (c *Client) luoguProfile(ctx context.Context, uid string) (Profile, error) {
	env, err := c.luoguData(ctx, "/user/"+url.PathEscape(uid), "")
	if err != nil {
		return Profile{}, err
	}
	if err := luoguEnvelopeOK(env); err != nil {
		return Profile{}, err
	}
	user, ok := luoguUserOf(env)
	if !ok {
		return Profile{}, fmt.Errorf("洛谷用户 %s 不存在或主页不可见", uid)
	}
	profile := Profile{Platform: "luogu", Handle: luoguString(user["name"]), Solved: luoguInt(user["passedProblemCount"])}
	if profile.Handle == "" {
		profile.Handle = uid
	}
	if rank := luoguInt(user["ranking"]); rank > 0 {
		profile.Rating = fmt.Sprintf("排名 %d", rank)
	}
	return profile, nil
}

// verifyLuogu 用公开用户主页校验 UID；若配置了 Cookie，再确认它能不能真正读到记录。
func (c *Client) verifyLuogu(ctx context.Context, cfg Config) (Profile, error) {
	uid := strings.TrimSpace(cfg.LuoguUID)
	if uid == "" {
		return Profile{}, fmt.Errorf("洛谷 UID 不能为空")
	}
	profile, err := c.luoguProfile(ctx, uid)
	if err != nil {
		return Profile{}, err
	}
	if strings.TrimSpace(cfg.LuoguCookie) == "" {
		profile.Note = "已校验 UID；未配置 Cookie，无法读取提交记录（历史同步不可用）"
		return profile, nil
	}
	_, note := c.luoguAuthedCookie(ctx, uid, cfg.LuoguCookie)
	profile.Note = note
	return profile, nil
}

// luoguRecords 从 /record/list 的 data 里取出记录数组，兼容几种可能的嵌套。
func luoguRecords(data map[string]any) []map[string]any {
	candidates := []any{data["records"], data["result"], data["list"]}
	for _, candidate := range candidates {
		if holder, ok := candidate.(map[string]any); ok {
			candidate = holder["result"]
		}
		if list, ok := candidate.([]any); ok {
			out := make([]map[string]any, 0, len(list))
			for _, item := range list {
				if row, ok := item.(map[string]any); ok {
					out = append(out, row)
				}
			}
			return out
		}
	}
	return nil
}

// syncLuogu 用配置里的 __client_id 拉取该用户的提交记录（洛谷记录接口需要登录态）。
func (c *Client) syncLuogu(ctx context.Context, cfg Config) (SyncResult, error) {
	uid := strings.TrimSpace(cfg.LuoguUID)
	if uid == "" {
		return SyncResult{}, fmt.Errorf("洛谷 UID 不能为空")
	}
	if strings.TrimSpace(cfg.LuoguCookie) == "" {
		return SyncResult{}, fmt.Errorf("洛谷历史同步需要 __client_id Cookie；未配置时请使用浏览器脚本实时接入")
	}
	profile, err := c.luoguProfile(ctx, uid)
	if err != nil {
		return SyncResult{}, err
	}
	// 用"能真正读到记录"的那组 Cookie；洛谷对凭据的要求各版本不同，
	// 让 luoguAuthedCookie 决定要不要补 _uid。
	// 注意这里不要再调 verifyLuogu——那会把同样的探测请求重复发一遍。
	cookieHeader, note := c.luoguAuthedCookie(ctx, uid, cfg.LuoguCookie)
	if cookieHeader == "" {
		return SyncResult{}, fmt.Errorf("%s", note)
	}
	// 洛谷记录每页 20 条（不是固定 50），perPage 要从响应里读，
	// 否则"本页不足一页"的判断永远成立，只会拉到第一页。
	const fallbackPerPage = 20
	perPage := fallbackPerPage
	total := 0
	rows := make([]Submission, 0, fallbackPerPage)
	skipped := 0
	// 记录遇到但认不出来的状态码：洛谷的状态枚举随版本变化，
	// 把出现过的码回报出去，才能据此补映射（而不是默默丢掉记录）。
	unknown := map[int]int{}
	for page := 1; page <= luoguMaxPages; page++ {
		env, err := c.luoguDataWithHeader(ctx, fmt.Sprintf("/record/list?user=%s&page=%d", url.QueryEscape(uid), page), cookieHeader)
		if err != nil {
			return SyncResult{}, err
		}
		if err := luoguEnvelopeOK(env); err != nil {
			// 缺 data / errorCode 非 0：当成失败而不是静默当成成功（否则会拿到
			// 空列表后直接 break，静默返回 0 条记录）。网络类错误已在 luoguGetText
			// 层按项目约定只记 warning，这里只负责业务层判定。
			logLuoguAuthHint("/record/list page="+strconv.Itoa(page), cookieHeader, err.Error())
			return SyncResult{}, err
		}
		data, _ := luoguDataOf(env)
		if holder, ok := data["records"].(map[string]any); ok {
			if v := luoguInt(holder["perPage"]); v > 0 {
				perPage = v
			}
			if v := luoguInt(holder["count"]); v > 0 {
				total = v
			}
		}
		list := luoguRecords(data)
		if len(list) == 0 {
			break
		}
		for _, item := range list {
			submission, ok := luoguRecordToSubmission(item, uid)
			if !ok {
				code := luoguInt(item["status"])
				// 每个未识别状态最多打 3 条样本，便于按真实数据补映射
				if unknown[code] < 3 {
					if encoded, err := json.Marshal(item); err == nil {
						preview := string(encoded)
						if len(preview) > 400 {
							preview = preview[:400]
						}
						fmt.Printf("[luogu] 未识别状态 status=%d 样本: %s\n", code, preview)
					}
				}
				skipped++
				unknown[code]++
				continue
			}
			rows = append(rows, submission)
		}
		if len(list) < perPage {
			break
		}
		if total > 0 && len(rows)+skipped >= total {
			break
		}
		// 页间间隔由 Client.waitForSlot 统一保证（默认 800ms），这里不再额外 sleep。
	}
	message := fmt.Sprintf("从洛谷获取 %d 条提交记录", len(rows))
	if total > len(rows)+skipped {
		message += fmt.Sprintf("（共 %d 条，已拉取最近 %d 页）", total, (len(rows)+skipped+perPage-1)/perPage)
	}
	if skipped > 0 {
		codes := make([]string, 0, len(unknown))
		for code, n := range unknown {
			codes = append(codes, fmt.Sprintf("%d×%d", code, n))
		}
		sort.Strings(codes)
		message += fmt.Sprintf("（跳过 %d 条未识别状态：%s）", skipped, strings.Join(codes, " "))
	}
	return SyncResult{Platform: "luogu", Profile: profile, Submissions: rows, Message: message}, nil
}

func luoguRecordToSubmission(item map[string]any, uid string) (Submission, bool) {
	rawID := luoguString(item["id"])
	if rawID == "" {
		rawID = luoguString(item["rid"])
	}
	if rawID == "" {
		return Submission{}, false
	}
	verdict := luoguVerdict(luoguInt(item["status"]))
	if verdict == "" {
		return Submission{}, false
	}
	problem, _ := item["problem"].(map[string]any)
	problemID := luoguString(item["pid"])
	title := luoguString(item["title"])
	difficulty := luoguDifficulties[luoguInt(item["difficulty"])]
	if problem != nil {
		if v := luoguString(problem["pid"]); v != "" {
			problemID = v
		}
		// 洛谷记录里的题名字段是 name（不是 title）
		if v := luoguString(problem["name"]); v != "" {
			title = v
		}
		if name, ok := luoguDifficulties[luoguInt(problem["difficulty"])]; ok {
			difficulty = name
		}
	}
	if title == "" {
		title = problemID
	}
	// 提交时间是 Unix 秒；字段名在不同版本里可能是 submissionTime / submitTime。
	submitSeconds := luoguInt(item["submissionTime"])
	if submitSeconds == 0 {
		submitSeconds = luoguInt(item["submitTime"])
	}
	if submitSeconds == 0 {
		// 提交时间缺失，无法可靠排序/去重，丢弃该条（避免产生 1970 年脏记录）。
		fmt.Printf("[luogu] 记录 %s 缺少提交时间，已跳过\n", rawID)
		return Submission{}, false
	}
	submittedAt := time.Unix(int64(submitSeconds), 0)
	extra := map[string]any{
		"score": luoguInt(item["score"]), "uid": uid,
		"time_ms": luoguInt(item["time"]), "memory_kb": luoguInt(item["memory"]),
	}
	// 洛谷记录里 language 是数字枚举，含义随站点版本变化，先原样留存不做猜测映射。
	if langID := luoguInt(item["language"]); langID > 0 {
		extra["language_id"] = langID
	}
	return Submission{
		RawID: rawID, ProblemID: problemID, ProblemTitle: title, Verdict: verdict,
		Difficulty: difficulty, SubmittedAt: submittedAt,
		URL:   "https://www.luogu.com.cn/record/" + rawID,
		Extra: extra,
	}, true
}

// luoguProblemMeta 取洛谷题库分页元数据（perPage/total）。洛谷各页 perPage 一致，
// 缓存 10 分钟，避免 AllProblems 逐页拉取时每页都重复请求第一页。
func (c *Client) luoguProblemMeta(ctx context.Context) (perPage, total int, err error) {
	c.luoguMetaMu.Lock()
	if c.luoguPerPage > 0 && time.Since(c.luoguMetaAt) < 10*time.Minute {
		pp, t := c.luoguPerPage, c.luoguTotal
		c.luoguMetaMu.Unlock()
		return pp, t, nil
	}
	c.luoguMetaMu.Unlock()
	env, err := c.luoguData(ctx, "/problem/list?page=1", "")
	if err != nil {
		return 0, 0, err
	}
	data, _ := luoguDataOf(env)
	if data == nil {
		return 0, 0, fmt.Errorf("洛谷题库响应中没有找到公开数据")
	}
	block, _ := data["problems"].(map[string]any)
	if block == nil {
		return 0, 0, fmt.Errorf("洛谷题库响应中没有找到公开数据")
	}
	// 题库页 perPage/count 在服务端是字符串（"50"/"17502"），luoguInt 已兼容。
	perPage = luoguInt(block["perPage"])
	total = luoguInt(block["count"])
	if perPage <= 0 {
		perPage = 50
	}
	c.luoguMetaMu.Lock()
	c.luoguPerPage, c.luoguTotal, c.luoguMetaAt = perPage, total, time.Now()
	c.luoguMetaMu.Unlock()
	return perPage, total, nil
}

func (c *Client) luoguProblems(ctx context.Context, page, limit int) ([]Problem, int, error) {
	// 洛谷题库页面将公开数据放在 lentille-context JSON script 中，读取页面
	// 已提供的数据，不模拟提交，也不依赖登录态。
	// 洛谷每页条数（perPage）由响应给出，必须用真实 perPage 计算页码与页内偏移，
	// 不能用猜测值，否则两者不一致会错位漏题。
	if limit < 1 {
		limit = 30
	}
	perPage, total, err := c.luoguProblemMeta(ctx)
	if err != nil {
		return nil, 0, err
	}
	globalOffset := 0
	if page > 1 {
		globalOffset = (page - 1) * limit
	}
	luoguPage := globalOffset/perPage + 1
	env, err := c.luoguData(ctx, fmt.Sprintf("/problem/list?page=%d", luoguPage), "")
	if err != nil {
		return nil, 0, err
	}
	data, _ := luoguDataOf(env)
	if data == nil {
		return nil, 0, fmt.Errorf("洛谷题库响应中没有找到公开数据")
	}
	problemBlock, _ := data["problems"].(map[string]any)
	if problemBlock == nil {
		return nil, 0, fmt.Errorf("洛谷题库响应中没有找到公开数据")
	}
	rawList, _ := problemBlock["result"].([]any)
	all := make([]struct {
		PID        string `json:"pid"`
		Name       string `json:"name"`
		Difficulty int    `json:"difficulty"`
		Tags       []int  `json:"tags"`
	}, 0, len(rawList))
	for _, item := range rawList {
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var row struct {
			PID        string `json:"pid"`
			Name       string `json:"name"`
			Difficulty int    `json:"difficulty"`
			Tags       []int  `json:"tags"`
		}
		if json.Unmarshal(encoded, &row) == nil {
			all = append(all, row)
		}
	}
	from := globalOffset % perPage
	if from > len(all) {
		from = len(all)
	}
	to := from + limit
	if to > len(all) {
		to = len(all)
	}
	rows := make([]Problem, 0, to-from)
	for _, item := range all[from:to] {
		tags := make([]string, 0, len(item.Tags))
		for _, tag := range item.Tags {
			tags = append(tags, strconv.Itoa(tag))
		}
		difficulty := luoguDifficulties[item.Difficulty]
		rows = append(rows, Problem{Platform: "luogu", ID: item.PID, Title: item.Name, Difficulty: difficulty, Tags: tags, URL: "https://www.luogu.com.cn/problem/" + item.PID})
	}
	if total == 0 {
		total = len(rows)
	}
	return rows, total, nil
}
func (c *Client) verifyAcWing(ctx context.Context, uid string) (Profile, error) {
	if strings.TrimSpace(uid) == "" {
		return Profile{}, fmt.Errorf("AcWing 用户 ID 不能为空")
	}
	// AcWing 的接口形态尚未核实（登录态要求、是否有可读的用户页），
	// 先如实标成"不支持在线校验"，等拿到真实响应再按洛谷的方式适配。
	return Profile{Platform: "acwing", Handle: uid}, fmt.Errorf("%w：暂未适配 AcWing 的用户接口，提交记录请使用浏览器脚本接入", ErrUnsupportedVerify)
}
func (c *Client) syncAcWing(ctx context.Context, uid string) (SyncResult, error) {
	_, err := c.verifyAcWing(ctx, uid)
	return SyncResult{}, err
}

// Keep compile-time coverage for URL query building in adapters.
var _ = url.Values{}
