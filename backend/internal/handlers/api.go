package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codeactivityhub/backend/internal/database"
	"codeactivityhub/backend/internal/middleware"
	"codeactivityhub/backend/internal/models"
	"codeactivityhub/backend/internal/platforms"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------------------
// 进程内轻量锁与限流：避免依赖外部组件，只用于本文件内的并发保护。
// ---------------------------------------------------------------------------

// manualSyncMu 保护 manualSyncInProgress，按 (user_id, platform) 互斥，
// 防止用户连点并发跑多个最长 150s 的同步（重复打接口、重复写库）。
var (
	manualSyncMu         sync.Mutex
	manualSyncInProgress = map[string]bool{}
)

// loginLimitMu 保护 loginAttempts，按 IP+用户名 记失败次数做登录限流，
// 避免用户名不存在时仍被拿去跑 PBKDF2 消耗 CPU（DoS）。
var (
	loginLimitMu    sync.Mutex
	loginAttempts   = map[string]*loginAttempt{}
	loginMaxFails   = 5                // 窗口内允许的失败次数
	loginFailWindow = 60 * time.Second // 失败计数滑动窗口
)

type loginAttempt struct {
	count     int
	firstSeen time.Time
}

// displayLocOnce 缓存展示时区，避免批量入库时反复 time.LoadLocation 触发上千次磁盘 IO。
var (
	displayLocOnce sync.Once
	displayLoc     *time.Location
)

type API struct {
	DB        *gorm.DB
	Platforms *platforms.Client

	// ProblemSync 题库全量同步的内存进度；main.go 字面量构造时为零值，
	// 由 problems.go 的 syncMgr() 惰性初始化。
	ProblemSync *problemSyncManager

	problemSyncOnce sync.Once
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type passwordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}
type syncRequest struct {
	Platform string `json:"platform"`
}
type verifyRequest struct {
	Platform       string `json:"platform"`
	CFHandle       string `json:"cf_handle"`
	LuoguUID       string `json:"luogu_uid"`
	LeetCode       string `json:"leetcode_username"`
	AtCoder        string `json:"atcoder_handle"`
	AcWingID       string `json:"acwing_user_id"`
	LuoguCookie    string `json:"luogu_cookie"`
	AcWingCookie   string `json:"acwing_cookie"`
	LeetCodeCookie string `json:"leetcode_cookie"`
}
type ingestRequest struct {
	Platform        string         `json:"platform"`
	RawID           string         `json:"raw_id"`
	ProblemID       string         `json:"problem_id"`
	ProblemTitle    string         `json:"problem_title"`
	Verdict         string         `json:"verdict"`
	Tags            []string       `json:"tags"`
	Difficulty      string         `json:"difficulty"`
	DifficultyScore int            `json:"difficulty_score"`
	SubmittedAt     string         `json:"submitted_at"`
	SubmissionURL   string         `json:"submission_url"`
	CodeLanguage    string         `json:"code_language"`
	ExtraData       map[string]any `json:"extra_data"`
	RequestMeta     map[string]any `json:"request_meta"`
	Source          string         `json:"source"`
}
type ingestBatchRequest struct {
	Submissions []ingestRequest `json:"submissions"`
}

type platformCount struct {
	Platform string `gorm:"column:platform"`
	Count    int    `gorm:"column:count"`
}
type dateCount struct {
	Date  string `gorm:"column:date"`
	Count int    `gorm:"column:count"`
}

func (a *API) RegisterRoutes(r *gin.Engine) {
	r.POST("/api/auth/register", a.Register)
	r.POST("/api/auth/login", a.Login)
	auth := r.Group("/api", middleware.RequireAuth(a.DB))
	auth.GET("/auth/me", a.Me)
	auth.POST("/auth/logout", a.Logout)
	auth.POST("/auth/password", a.ChangePassword)
	auth.GET("/stats/overview", a.Overview)
	auth.GET("/stats/heatmap", a.Heatmap)
	auth.GET("/stats/tags", a.Tags)
	auth.GET("/stats/mistakes", a.Mistakes)
	auth.GET("/stats/submissions", a.Submissions)
	auth.GET("/settings", a.GetSettings)
	auth.POST("/settings", a.UpdateSettings)
	auth.POST("/sync", a.ManualSync)
	auth.GET("/ingest/token", a.IngestToken)
	auth.GET("/ingest/tokens", a.ListIngestTokens)
	auth.POST("/ingest/tokens", a.CreateIngestToken)
	auth.POST("/ingest/tokens/:id/rotate", a.RotateIngestToken)
	auth.DELETE("/ingest/tokens/:id", a.RevokeIngestToken)
	auth.POST("/ingest/submission", a.IngestSubmission)
	auth.POST("/ingest/submissions", a.IngestBatch)
	auth.GET("/ingest/events", a.IngestEvents)
	auth.GET("/contests", a.Contests)
	auth.GET("/problems", a.Problems)
	auth.POST("/problems/sync", a.ProblemsSync)
	auth.POST("/contests/sync", a.ContestsSync)
	auth.POST("/verify", a.Verify)
	auth.GET("/accounts", a.ListAccounts)
	auth.POST("/accounts", a.CreateAccount)
	auth.POST("/accounts/:id/verify", a.VerifyAccount)
	auth.POST("/accounts/:id/select", a.SelectAccount)
	auth.DELETE("/accounts/:id", a.DeleteAccount)
	auth.PUT("/accounts/:id", a.UpdateAccount)
}

func userID(c *gin.Context) uint {
	value, _ := c.Get(middleware.UserIDKey)
	id, _ := value.(uint)
	return id
}
func sessionToken(c *gin.Context) string {
	value, _ := c.Get(middleware.SessionTokenKey)
	token, _ := value.(string)
	return token
}
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
func jsonError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"success": false, "detail": message, "message": message})
}

func (a *API) Register(c *gin.Context) {
	var req authRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	// 用字符数而不是字节数校验，否则中文用户名（3 字节/字）会被后端拒绝、
	// 前端却放行，用户只看到一句对不上的报错。
	if utf8.RuneCountInString(req.Username) < 3 || utf8.RuneCountInString(req.Username) > 32 || len(req.Password) < 6 {
		jsonError(c, 400, "用户名需为 3-32 个字符，密码不少于 6 位")
		return
	}
	if !validUsername(req.Username) {
		jsonError(c, 400, "用户名只能包含中英文、数字、下划线和连字符")
		return
	}
	hash, salt, err := database.HashPassword(req.Password)
	if err != nil {
		jsonError(c, 500, "密码处理失败")
		return
	}
	user := models.User{Username: req.Username, PasswordHash: hash, Salt: salt}
	if err := a.DB.Create(&user).Error; err != nil {
		// 不能把所有写入失败都当成重名，磁盘/约束异常会被误报成"用户名已存在"。
		if isUniqueViolation(err) {
			jsonError(c, 400, "用户名已存在")
			return
		}
		jsonError(c, 500, "注册失败，请稍后重试")
		return
	}
	token, err := a.createSession(user.ID)
	if err != nil {
		jsonError(c, 500, "会话创建失败")
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "注册成功", "token": token, "user": userResponse(user)})
}

func (a *API) Login(c *gin.Context) {
	var req authRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	username := strings.TrimSpace(req.Username)
	// 登录限流：同一 IP+用户名 在窗口内失败过多直接拒绝，避免 PBKDF2 被拿消耗 CPU。
	if !loginRateAllow(c.ClientIP(), username) {
		jsonError(c, 429, "登录尝试过于频繁，请稍后再试")
		return
	}
	var user models.User
	if err := a.DB.Where("username = ?", username).First(&user).Error; err != nil {
		// 用户不存在：仍跑一次哈希，使耗时与"密码错误"趋于一致，
		// 避免时序差异泄露账号是否存在；随后按失败计一次。
		_, _, _ = database.HashPassword(req.Password)
		loginRateFail(c.ClientIP(), username)
		jsonError(c, 400, "用户名或密码错误")
		return
	}
	if !database.VerifyPassword(req.Password, user.Salt, user.PasswordHash) {
		loginRateFail(c.ClientIP(), username)
		jsonError(c, 400, "用户名或密码错误")
		return
	}
	loginRateReset(c.ClientIP(), username)
	token, err := a.createSession(user.ID)
	if err != nil {
		jsonError(c, 500, "会话创建失败")
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "登录成功", "token": token, "user": userResponse(user)})
}

// loginRateAllow 在窗口内失败次数未达上限时返回 true；超限返回 false（调用方应拒绝）。
func loginRateAllow(ip, username string) bool {
	loginLimitMu.Lock()
	defer loginLimitMu.Unlock()
	key := ip + "\x00" + username
	now := time.Now()
	at, ok := loginAttempts[key]
	if !ok || now.Sub(at.firstSeen) > loginFailWindow {
		loginAttempts[key] = &loginAttempt{count: 0, firstSeen: now}
		return true
	}
	return at.count < loginMaxFails
}

// loginRateFail 记一次失败；超过窗口则重置计数起点。
func loginRateFail(ip, username string) {
	loginLimitMu.Lock()
	defer loginLimitMu.Unlock()
	key := ip + "\x00" + username
	now := time.Now()
	at, ok := loginAttempts[key]
	if !ok || now.Sub(at.firstSeen) > loginFailWindow {
		loginAttempts[key] = &loginAttempt{count: 1, firstSeen: now}
		return
	}
	at.count++
}

// loginRateReset 登录成功后清除失败计数。
func loginRateReset(ip, username string) {
	loginLimitMu.Lock()
	defer loginLimitMu.Unlock()
	delete(loginAttempts, ip+"\x00"+username)
}

func validUsername(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		case r >= 0x4e00 && r <= 0x9fff: // 中日韩统一表意文字
		default:
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") || strings.Contains(message, "constraint failed: unique")
}

func userResponse(user models.User) gin.H {
	return gin.H{"id": user.ID, "username": user.Username, "is_admin": user.IsAdmin}
}
func (a *API) createSession(uid uint) (string, error) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	if err := a.DB.Create(&models.Session{Token: token, UserID: uid, ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)}).Error; err != nil {
		return "", err
	}
	return token, nil
}
func (a *API) Me(c *gin.Context) {
	var user models.User
	if a.DB.First(&user, userID(c)).Error != nil {
		jsonError(c, 401, "用户不存在")
		return
	}
	c.JSON(200, gin.H{"user": userResponse(user)})
}
func (a *API) Logout(c *gin.Context) {
	a.DB.Delete(&models.Session{}, "token = ?", sessionToken(c))
	c.JSON(200, gin.H{"success": true, "message": "已成功注销登录"})
}
func (a *API) ChangePassword(c *gin.Context) {
	var req passwordRequest
	if c.ShouldBindJSON(&req) != nil || len(req.NewPassword) < 6 {
		jsonError(c, 400, "新密码长度不能少于 6 位")
		return
	}
	var user models.User
	if a.DB.First(&user, userID(c)).Error != nil || !database.VerifyPassword(req.OldPassword, user.Salt, user.PasswordHash) {
		jsonError(c, 400, "原密码错误")
		return
	}
	hash, salt, err := database.HashPassword(req.NewPassword)
	if err != nil {
		jsonError(c, 500, "密码处理失败")
		return
	}
	a.DB.Model(&user).Updates(map[string]any{"password_hash": hash, "salt": salt})
	a.DB.Delete(&models.Session{}, "user_id = ?", user.ID)
	// 改密码应该让所有脚本 token 一起失效，否则"改了密码脚本照样能提交"。
	a.revokeAllIngestTokens(user.ID)
	c.JSON(200, gin.H{"success": true, "message": "密码修改成功，请重新登录；浏览器脚本 Token 已全部吊销，需重新生成"})
}

func (a *API) Overview(c *gin.Context) {
	uid := userID(c)
	today := effectiveDate(time.Now())
	var total, totalAC, todaySubs, todayAC int64
	a.DB.Model(&models.Submission{}).Where("user_id = ?", uid).Count(&total)
	a.DB.Model(&models.Submission{}).Where("user_id = ? AND verdict = ?", uid, "AC").Select("COUNT(DISTINCT platform || ':' || problem_id)").Scan(&totalAC)
	a.DB.Model(&models.Submission{}).Where("user_id = ? AND date = ?", uid, today).Count(&todaySubs)
	a.DB.Model(&models.Submission{}).Where("user_id = ? AND date = ? AND verdict = ?", uid, today, "AC").Select("COUNT(DISTINCT platform || ':' || problem_id)").Scan(&todayAC)
	var rows []platformCount
	a.DB.Model(&models.Submission{}).Select("platform, COUNT(DISTINCT CASE WHEN verdict = 'AC' THEN problem_id END) AS count").Where("user_id = ?", uid).Group("platform").Scan(&rows)
	platforms := map[string]gin.H{}
	for _, row := range rows {
		platforms[row.Platform] = gin.H{"ac": row.Count}
	}
	var statuses []models.PlatformStatus
	a.DB.Where("user_id = ?", uid).Order("platform").Find(&statuses)
	// 未配置账号的平台即便库里有历史 error，也不该让顶部横幅一直告警。
	statuses = normalizeStatuses(statuses, a.configuredPlatforms(uid))
	var last models.IngestEvent
	lastTime := ""
	if a.DB.Where("user_id = ?", uid).Order("received_at DESC").First(&last).Error == nil {
		lastTime = last.ReceivedAt
	}
	c.JSON(200, gin.H{"stats": gin.H{"total_ac": totalAC, "total_subs": total, "today_ac": todayAC, "today_subs": todaySubs, "streak": a.streak(uid), "platforms": platforms}, "platforms_status": statuses, "last_sync_time": lastTime})
}

func (a *API) Heatmap(c *gin.Context) {
	uid := userID(c)
	year, _ := strconv.Atoi(c.DefaultQuery("year", strconv.Itoa(time.Now().In(displayLocation()).Year())))
	if year == 0 {
		year = time.Now().In(displayLocation()).Year()
	}
	query := a.DB.Model(&models.Submission{}).Select("date, COUNT(*) AS count").Where("user_id = ? AND date LIKE ?", uid, fmt.Sprintf("%04d-%%", year))
	if p := c.Query("platform"); p != "" && p != "all" {
		query = query.Where("platform = ?", p)
	}
	var counts []dateCount
	query.Group("date").Scan(&counts)
	values := map[string]int{}
	for _, row := range counts {
		values[row.Date] = row.Count
	}
	result := make([]gin.H, 0, 366)
	start := time.Date(year, 1, 1, 0, 0, 0, 0, displayLocation())
	for d := start; d.Year() == year; d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		result = append(result, gin.H{"date": ds, "count": values[ds]})
	}
	// 年份切换器需要真实有数据的年份，否则永远只能看当前年。
	var yearRows []struct {
		Yr string
	}
	a.DB.Raw(`SELECT DISTINCT substr(date, 1, 4) AS yr FROM submissions WHERE user_id = ? AND length(date) >= 4`, uid).Scan(&yearRows)
	yearSet := map[int]bool{}
	years := make([]int, 0, len(yearRows)+1)
	for _, row := range yearRows {
		if y, err := strconv.Atoi(row.Yr); err == nil && !yearSet[y] {
			yearSet[y] = true
			years = append(years, y)
		}
	}
	if !yearSet[time.Now().In(displayLocation()).Year()] {
		years = append(years, time.Now().In(displayLocation()).Year())
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	c.JSON(200, gin.H{"year": year, "heatmap": result, "available_years": years})
}

func (a *API) Tags(c *gin.Context) {
	// 页面展示为"按通过题数"：只统计 AC 提交，且每道题（平台+题号去重）只计一次。
	// 不能用 GROUP BY platform, problem_id 直接取 tags：SQLite 会返回分组内任意
	// 一行的裸列值，同一道题的多次 AC 提交里只要挑到没带标签的那条，标签就丢了。
	// 这里取回全部 AC 记录，在 Go 里按题归组并对标签求并集。
	var rows []models.Submission
	a.DB.Where("user_id = ? AND verdict = 'AC'", userID(c)).
		Select("platform, problem_id, tags").Find(&rows)
	type problemKey struct{ platform, problemID string }
	tagSets := make(map[problemKey]map[string]bool)
	for _, row := range rows {
		var tags []string
		_ = json.Unmarshal([]byte(row.Tags), &tags)
		key := problemKey{row.Platform, row.ProblemID}
		set := tagSets[key]
		if set == nil {
			set = map[string]bool{}
			tagSets[key] = set
		}
		for _, tag := range tags {
			if strings.TrimSpace(tag) != "" {
				set[strings.TrimSpace(tag)] = true
			}
		}
	}
	counts := map[string]int{}
	for _, set := range tagSets {
		for tag := range set {
			counts[tag]++
		}
	}
	result := make([]gin.H, 0, len(counts))
	for name, value := range counts {
		result = append(result, gin.H{"name": name, "value": value})
	}
	// 让前端拿到稳定的顺序，避免每次刷新标签条形图乱跳。
	sort.Slice(result, func(i, j int) bool {
		if result[i]["value"].(int) != result[j]["value"].(int) {
			return result[i]["value"].(int) > result[j]["value"].(int)
		}
		return result[i]["name"].(string) < result[j]["name"].(string)
	})
	c.JSON(200, gin.H{"tags": result})
}

func (a *API) Mistakes(c *gin.Context) {
	limit := queryLimit(c, 50)
	var rows []struct {
		Platform      string
		ProblemID     string
		ProblemTitle  string
		SubmittedAt   string
		FailTimes     int
		Verdict       string
		Difficulty    string
		SubmissionURL string
	}
	// 模板的错题表会展示最近一次失败提交的判定、难度和跳转链接，一并查出。
	a.DB.Raw(`SELECT s.platform, s.problem_id, s.problem_title, MAX(s.submitted_at) AS submitted_at, COUNT(*) AS fail_times,
		(SELECT s2.verdict FROM submissions s2 WHERE s2.user_id=s.user_id AND s2.platform=s.platform AND s2.problem_id=s.problem_id AND s2.verdict NOT IN ('AC','PENDING','JUDGING','UNKNOWN') ORDER BY s2.submitted_at DESC LIMIT 1) AS verdict,
		(SELECT s2.difficulty FROM submissions s2 WHERE s2.user_id=s.user_id AND s2.platform=s.platform AND s2.problem_id=s.problem_id AND s2.verdict NOT IN ('AC','PENDING','JUDGING','UNKNOWN') ORDER BY s2.submitted_at DESC LIMIT 1) AS difficulty,
		(SELECT s2.submission_url FROM submissions s2 WHERE s2.user_id=s.user_id AND s2.platform=s.platform AND s2.problem_id=s.problem_id AND s2.verdict NOT IN ('AC','PENDING','JUDGING','UNKNOWN') ORDER BY s2.submitted_at DESC LIMIT 1) AS submission_url
		FROM submissions s WHERE s.user_id = ? AND s.verdict NOT IN ('AC','PENDING','JUDGING','UNKNOWN') AND NOT EXISTS (SELECT 1 FROM submissions ac WHERE ac.user_id=s.user_id AND ac.platform=s.platform AND ac.problem_id=s.problem_id AND ac.verdict='AC') GROUP BY s.platform,s.problem_id ORDER BY fail_times DESC, submitted_at DESC LIMIT ?`, userID(c), limit).Scan(&rows)
	result := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		result = append(result, gin.H{"platform": row.Platform, "problem_id": row.ProblemID, "problem_title": row.ProblemTitle,
			"submitted_at": row.SubmittedAt, "fail_times": row.FailTimes, "verdict": row.Verdict,
			"difficulty": row.Difficulty, "submission_url": row.SubmissionURL, "tags": []string{}})
	}
	c.JSON(200, gin.H{"mistakes": result})
}

func (a *API) Submissions(c *gin.Context) {
	limit := queryLimit(c, 2000)
	query := a.DB.Where("user_id = ?", userID(c))
	if p := c.Query("platform"); p != "" && p != "all" {
		query = query.Where("platform = ?", p)
	}
	var rows []models.Submission
	query.Order("submitted_at DESC, id DESC").Limit(limit).Find(&rows)
	// tags 在库里是 JSON 字符串，前端按数组使用，这里解析后返回。
	result := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		var tags []string
		_ = json.Unmarshal([]byte(row.Tags), &tags)
		if tags == nil {
			tags = []string{}
		}
		result = append(result, gin.H{"id": row.ID, "platform": row.Platform, "raw_id": row.RawID, "problem_id": row.ProblemID,
			"problem_title": row.ProblemTitle, "verdict": row.Verdict, "tags": tags, "difficulty": row.Difficulty,
			"difficulty_score": row.DifficultyScore, "submitted_at": row.SubmittedAt, "date": row.Date,
			"submission_url": row.SubmissionURL, "code_language": row.CodeLanguage})
	}
	c.JSON(200, gin.H{"submissions": result})
}

func queryLimit(c *gin.Context, fallback int) int {
	n, _ := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(fallback)))
	if n <= 0 {
		n = fallback
	}
	if n > 10000 {
		n = 10000
	}
	return n
}

// configuredPlatforms 一次读出该用户各平台是否已配置账号。
func (a *API) configuredPlatforms(uid uint) map[string]bool {
	var rows []models.UserConfig
	a.DB.Where("user_id = ?", uid).Find(&rows)
	values := map[string]string{}
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	cfg := platforms.Config{
		CFHandle: values["cf_handle"], LuoguUID: values["luogu_uid"],
		LeetCode: values["leetcode_username"], AtCoder: values["atcoder_handle"],
		AcWingID: values["acwing_user_id"],
	}
	out := map[string]bool{}
	for _, name := range []string{"codeforces", "leetcode", "atcoder", "luogu", "acwing"} {
		out[name] = platformConfigured(cfg, name)
	}
	return out
}

// normalizeStatuses 把"平台根本没配账号、却残留着 error 状态"的历史记录降级成未配置。
// 这类 error 大多来自早期版本把"该平台没有公开校验接口"也记成了失败，
// 用户既没有任何可操作的入口，也没法把它清掉，只会让顶部横幅一直告警。
func normalizeStatuses(statuses []models.PlatformStatus, configured map[string]bool) []models.PlatformStatus {
	for i := range statuses {
		if (statuses[i].Status == "error" || statuses[i].Status == "warning") && !configured[statuses[i].Platform] {
			statuses[i].Status = "unconfigured"
			continue
		}
		// 历史遗留自愈：在"网络错误→warning"的修复上线前，超时曾被写成 error 并持久化。
		// 读时降级为 warning（红条只认 error），否则一次网络波动会在横幅上永久驻留。
		if statuses[i].Status == "error" && isNetworkErrText(statuses[i].Message) {
			statuses[i].Status = "warning"
			statuses[i].Message = syncTimeoutHint
		}
	}
	return statuses
}

func (a *API) GetSettings(c *gin.Context) {
	var configs []models.UserConfig
	a.DB.Where("user_id = ?", userID(c)).Find(&configs)
	values := map[string]string{}
	for _, row := range configs {
		values[row.Key] = row.Value
	}
	var statuses []models.PlatformStatus
	a.DB.Where("user_id = ?", userID(c)).Find(&statuses)
	statuses = normalizeStatuses(statuses, a.configuredPlatforms(userID(c)))
	statusMap := map[string]models.PlatformStatus{}
	for _, row := range statuses {
		statusMap[row.Platform] = row
	}
	c.JSON(200, gin.H{"configs": values, "status": statusMap})
}

// allowedSettingKeys 配置白名单：只允许写入这些已知的平台配置键，
// 避免任意 key 被写进 user_configs（例如覆盖其他模块使用的键、注入脏数据）。
var allowedSettingKeys = map[string]bool{
	"cf_handle": true, "luogu_uid": true, "leetcode_username": true,
	"atcoder_handle": true, "acwing_user_id": true,
	"luogu_cookie": true, "acwing_cookie": true, "leetcode_cookie": true,
}

func (a *API) UpdateSettings(c *gin.Context) {
	var values map[string]any
	if c.ShouldBindJSON(&values) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	uid := userID(c)
	// 单事务写入：要么全部成功，要么失败时明确报错，避免"部分写入却报成功"。
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		for key, value := range values {
			if value == nil {
				continue
			}
			if !allowedSettingKeys[key] {
				// 非法键不写入，但也不阻断其余合法键——只跳过。
				continue
			}
			row := models.UserConfig{UserID: uid, Key: key, Value: fmt.Sprint(value)}
			if err := tx.Where("user_id = ? AND key = ?", uid, key).
				Assign(models.UserConfig{Value: row.Value, UpdatedAt: time.Now()}).
				FirstOrCreate(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("更新设置失败 uid=%d: %v", uid, err)
		jsonError(c, 500, "配置保存失败")
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "配置更新成功"})
}
func (a *API) platformClient() *platforms.Client {
	if a.Platforms == nil {
		a.Platforms = platforms.NewClient()
	}
	return a.Platforms
}

func (a *API) loadPlatformConfig(uid uint, platform string) platforms.Config {
	values := map[string]string{}
	var rows []models.UserConfig
	a.DB.Where("user_id = ?", uid).Find(&rows)
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	return platforms.Config{
		Platform: platform, CFHandle: values["cf_handle"], LuoguUID: values["luogu_uid"],
		LeetCode: values["leetcode_username"], AtCoder: values["atcoder_handle"],
		AcWingID: values["acwing_user_id"], LuoguCookie: values["luogu_cookie"],
		AcWingCookie: values["acwing_cookie"], LeetCodeCookie: values["leetcode_cookie"],
	}
}

func (a *API) configFromVerify(req verifyRequest) platforms.Config {
	return platforms.Config{Platform: req.Platform, CFHandle: req.CFHandle, LuoguUID: req.LuoguUID,
		LeetCode: req.LeetCode, AtCoder: req.AtCoder, AcWingID: req.AcWingID,
		LuoguCookie: req.LuoguCookie, AcWingCookie: req.AcWingCookie, LeetCodeCookie: req.LeetCodeCookie}
}

// verifySupported 标出哪些平台能通过公开接口或已配置的 Cookie 完成在线校验。
// 洛谷用公开用户主页校验 UID（提交记录才需要 Cookie），AcWing 尚未适配。
func verifySupported(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "codeforces", "leetcode", "atcoder", "luogu":
		return true
	}
	return false
}

// historySyncSupported 标出哪些平台能拉取历史提交。
// 洛谷的提交记录接口需要 __client_id Cookie，缺少时由 platformReadyForHistorySync 判定为跳过。
func historySyncSupported(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "codeforces", "leetcode", "atcoder", "luogu":
		return true
	}
	return false
}

// platformReadyForHistorySync 判断某平台当前配置能不能真的拉历史，
// 不能则给出一条说明（用于跳过而不是报错，避免设置页挂上无法消除的告警）。
func platformReadyForHistorySync(platform string, cfg platforms.Config) (bool, string) {
	if !historySyncSupported(platform) {
		return false, "该平台无公开历史提交接口，提交记录请使用浏览器脚本实时接入"
	}
	if platform == "luogu" && strings.TrimSpace(cfg.LuoguCookie) == "" {
		return false, "洛谷历史同步需要 __client_id Cookie；未配置时请使用浏览器脚本实时接入"
	}
	return true, ""
}

func platformConfigured(cfg platforms.Config, platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "codeforces":
		return strings.TrimSpace(cfg.CFHandle) != ""
	case "leetcode":
		return strings.TrimSpace(cfg.LeetCode) != ""
	case "atcoder":
		return strings.TrimSpace(cfg.AtCoder) != ""
	case "luogu":
		return strings.TrimSpace(cfg.LuoguUID) != ""
	case "acwing":
		return strings.TrimSpace(cfg.AcWingID) != ""
	}
	return false
}

func (a *API) savePlatformStatus(uid uint, profile platforms.Profile, status, message string, count int) {
	now := nowUTC()
	row := models.PlatformStatus{UserID: uid, Platform: profile.Platform, Status: status, Message: message, ItemCount: count, Rating: profile.Rating, LastCheckedAt: now}
	a.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "platform"}}, DoUpdates: clause.AssignmentColumns([]string{"status", "message", "item_count", "rating", "last_checked_at"})}).Create(&row)
}

func submissionToIngest(platform string, s platforms.Submission) ingestRequest {
	tags := append([]string(nil), s.Tags...)
	return ingestRequest{Platform: platform, RawID: s.RawID, ProblemID: s.ProblemID, ProblemTitle: s.ProblemTitle,
		Verdict: s.Verdict, Tags: tags, Difficulty: s.Difficulty, DifficultyScore: s.Score,
		SubmittedAt: s.SubmittedAt.Format(time.RFC3339), SubmissionURL: s.URL, CodeLanguage: s.Language,
		ExtraData: s.Extra, Source: "manual_api"}
}

// networkErrMarkers 网络类错误的特征串：这类错误是瞬时的（网络波动/站点限流/代理断连），
// 不该把平台持久化成"连接异常"——否则一次网络波动会让红条一直挂着，
// 和"账号验证成功"自相矛盾。
var networkErrMarkers = []string{
	"context deadline exceeded", "Client.Timeout", "i/o timeout",
	"no such host", "connection refused", "connection reset",
	"EOF", "proxyconnect",
}

// isNetworkErrText 按错误文本判断是否网络类错误。
// 除了新产生的 error，还被 normalizeStatuses 用来在读取时自愈历史遗留数据。
func isNetworkErrText(msg string) bool {
	for _, marker := range networkErrMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isTimeoutErr(err error) bool {
	return err != nil && isNetworkErrText(err.Error())
}

const syncTimeoutHint = "上次同步超时（网络波动或站点限流），账号配置不受影响，可点「刷新看板」重试"

func (a *API) syncOne(ctx context.Context, uid uint, platform string, cfg platforms.Config) (int, string, error) {
	result, err := a.platformClient().SyncSubmissions(ctx, cfg)
	if err != nil {
		if isTimeoutErr(err) {
			a.savePlatformStatus(uid, platforms.Profile{Platform: platform}, "warning", syncTimeoutHint, 0)
			// 返回友好提示而不是裸 Go 错误，避免前端 toast 弹出吓人的英文堆栈。
			return 0, syncTimeoutHint, err
		}
		a.savePlatformStatus(uid, platforms.Profile{Platform: platform}, "error", err.Error(), 0)
		return 0, "", err
	}
	inserted := 0
	for _, item := range result.Submissions {
		n, saveErr := a.saveSubmission(uid, submissionToIngest(result.Platform, item))
		if saveErr != nil {
			return inserted, "", saveErr
		}
		inserted += n
	}
	a.savePlatformStatus(uid, result.Profile, "ok", result.Message, len(result.Submissions))
	return inserted, result.Message, nil
}

func (a *API) ManualSync(c *gin.Context) {
	var req syncRequest
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		jsonError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	requested := strings.ToLower(strings.TrimSpace(req.Platform))
	if requested == "" {
		requested = "all"
	}
	valid := map[string]bool{"codeforces": true, "leetcode": true, "atcoder": true, "luogu": true, "acwing": true}
	if requested != "all" && !valid[requested] {
		jsonError(c, 400, "不支持的平台: "+requested)
		return
	}
	if requested != "all" && !historySyncSupported(requested) {
		jsonError(c, 400, requested+" 无公开历史提交接口，提交记录请使用浏览器脚本实时接入")
		return
	}
	uid := userID(c)
	platformList := []string{requested}
	if requested == "all" {
		platformList = []string{"codeforces", "leetcode", "atcoder", "luogu", "acwing"}
	}
	// 并发保护：同一用户的同一平台只允许一个同步在跑，连点直接返回 409，
	// 避免多个最长 150s 的同步并发打接口、并发写库。
	keys := make([]string, 0, len(platformList))
	for _, name := range platformList {
		keys = append(keys, fmt.Sprintf("%d:%s", uid, name))
	}
	manualSyncMu.Lock()
	busy := false
	for _, k := range keys {
		if manualSyncInProgress[k] {
			busy = true
			break
		}
	}
	if !busy {
		for _, k := range keys {
			manualSyncInProgress[k] = true
		}
	}
	manualSyncMu.Unlock()
	if busy {
		jsonError(c, http.StatusConflict, "同步进行中，请稍候再试")
		return
	}
	defer func() {
		manualSyncMu.Lock()
		for _, k := range keys {
			delete(manualSyncInProgress, k)
		}
		manualSyncMu.Unlock()
	}()
	results := make([]gin.H, 0, len(platformList))
	total := 0
	failed := 0
	attempted := 0
	for _, name := range platformList {
		cfg := a.loadPlatformConfig(uid, name)
		// 不能同步的情况（平台无接口、洛谷缺 Cookie）先判掉，记成 skipped，
		// 不要让它变成 failed 再被顶部横幅当成"连接异常"。
		if ready, reason := platformReadyForHistorySync(name, cfg); !ready {
			if requested != "all" {
				jsonError(c, 400, reason)
				return
			}
			results = append(results, gin.H{"platform": name, "success": true, "skipped": true, "synced": 0, "message": reason})
			continue
		}
		if !platformConfigured(cfg, name) {
			if requested != "all" {
				jsonError(c, 400, name+" 尚未配置账号")
				return
			}
			continue
		}
		// 每个平台各自独立的超时预算：共享同一个 context 会让前一个慢平台
		// 耗尽后剩余平台必然 deadline exceeded，却被判成"网络波动"写假告警。
		ctx, cancel := context.WithTimeout(c.Request.Context(), 150*time.Second)
		inserted, message, err := a.syncOne(ctx, uid, name, cfg)
		cancel()
		if err != nil {
			failed++
			if message == "" {
				message = err.Error()
			}
			results = append(results, gin.H{"platform": name, "success": false, "message": message})
			continue
		}
		attempted++
		total += inserted
		results = append(results, gin.H{"platform": name, "success": true, "synced": inserted, "message": message})
	}
	if len(results) == 0 {
		jsonError(c, 400, "请先在设置中配置至少一个平台账号")
		return
	}
	// 只配置了 AcWing 时不能报"同步完成"：它没有公开的历史接口，
	// 用户会以为真的同步过了。（洛谷已支持：走页面内嵌的 lentille 数据）
	if failed == 0 && attempted == 0 {
		jsonError(c, 400, "尚未配置支持历史同步的平台（洛谷 / Codeforces / LeetCode / AtCoder）；AcWing 请使用浏览器脚本实时接入")
		return
	}
	c.JSON(200, gin.H{"success": failed == 0, "data": gin.H{"mode": "manual", "platform": requested, "synced": total, "results": results, "synced_at": nowUTC(), "message": fmt.Sprintf("手动同步完成，写入 %d 条提交记录", total)}})
}

func (a *API) Verify(c *gin.Context) {
	var req verifyRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		jsonError(c, 400, "platform 必填")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	profile, err := a.platformClient().Verify(ctx, a.configFromVerify(req))
	if err != nil {
		// 洛谷/AcWing 没有公开校验接口：这是"不支持"，不是"连接失败"。
		// 若记成 error，设置页会挂一条用户永远清不掉的告警。
		if errors.Is(err, platforms.ErrUnsupportedVerify) {
			a.savePlatformStatus(userID(c), platforms.Profile{Platform: platform}, "unsupported", err.Error(), 0)
			c.JSON(200, gin.H{"valid": false, "supported": false, "success": false, "message": err.Error()})
			return
		}
		// 超时类错误是瞬时的，记成 warning 而不是 error，别让红条常驻。
		if isTimeoutErr(err) {
			a.savePlatformStatus(userID(c), platforms.Profile{Platform: platform}, "warning", syncTimeoutHint, 0)
			c.JSON(200, gin.H{"valid": false, "supported": true, "success": false, "timeout": true, "message": "验证请求超时，请稍后重试；账号配置不受影响"})
			return
		}
		a.savePlatformStatus(userID(c), platforms.Profile{Platform: platform}, "error", err.Error(), 0)
		c.JSON(400, gin.H{"valid": false, "supported": true, "success": false, "message": err.Error()})
		return
	}
	message := "账号验证成功"
	if profile.Note != "" {
		message = profile.Note
	}
	a.savePlatformStatus(userID(c), profile, "ok", message, profile.Solved)
	c.JSON(200, gin.H{"valid": true, "success": true, "message": message, "profile": profile})
}

// ---------------------------------------------------------------------------
// 平台多账号管理：同一平台可保存多个候选账号，同一时间只启用一个。
// 被启用的账号会把 handle/cookie 同步进 user_configs，历史同步、统计沿用原逻辑。
// ---------------------------------------------------------------------------

type accountRequest struct {
	Platform string `json:"platform"`
	Name     string `json:"name"`
	Handle   string `json:"handle"`
	Cookie   string `json:"cookie"`
	Verify   bool   `json:"verify"`
	Select   *bool  `json:"select"`
}

var platformMeta = map[string]struct {
	HandleKey string
	CookieKey string
	Label     string
}{
	"codeforces": {"cf_handle", "", "Codeforces"},
	"leetcode":   {"leetcode_username", "leetcode_cookie", "LeetCode"},
	"atcoder":    {"atcoder_handle", "", "AtCoder"},
	"luogu":      {"luogu_uid", "luogu_cookie", "洛谷"},
	"acwing":     {"acwing_user_id", "acwing_cookie", "AcWing"},
}

// UpdateAccount 编辑已保存账号的备注名 / 用户名 / Cookie。
// 改完把账号标回"未验证"（凭证可能已不同）；如果它正是启用中的账号，
// 还要回写 user_configs —— 历史同步读的是启用账号那一条，不回写等于没改。
// Cookie 留空表示"保持原值"：明文不回显，前端没法预填，只能重填或不动。
func (a *API) UpdateAccount(c *gin.Context) {
	acc, ok := a.loadAccount(c)
	if !ok {
		return
	}
	var req accountRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	handle := strings.TrimSpace(req.Handle)
	if handle == "" {
		jsonError(c, 400, "账号不能为空")
		return
	}
	if len(req.Cookie) > 1024 {
		jsonError(c, 400, "Cookie 过长")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = handle
	}
	updates := map[string]any{
		"name":     truncate(name, 64),
		"handle":   truncate(handle, 255),
		"status":   "unverified",
		"message":  "尚未验证",
		"verified": false,
	}
	if cookie := strings.TrimSpace(req.Cookie); cookie != "" {
		updates["cookie"] = cookie
	}
	if err := a.DB.Model(acc).Updates(updates).Error; err != nil {
		jsonError(c, 500, "账号更新失败")
		return
	}
	if err := a.DB.Where("id = ?", acc.ID).First(acc).Error; err != nil {
		jsonError(c, 500, "账号更新失败")
		return
	}
	if req.Select != nil && *req.Select && !acc.Selected {
		a.DB.Model(&models.PlatformAccount{}).Where("user_id = ? AND platform = ?", acc.UserID, acc.Platform).Update("selected", false)
		a.DB.Model(acc).Update("selected", true)
		acc.Selected = true
	}
	if req.Verify {
		a.verifyAccountRow(acc)
	}
	if acc.Selected {
		a.applyAccountToConfig(acc.UserID, acc.Platform)
	}
	c.JSON(200, gin.H{"success": true, "message": "账号已更新", "account": accountResponse(*acc)})
}

func accountResponse(acc models.PlatformAccount) gin.H {
	cookieHint := ""
	if acc.Cookie != "" {
		runes := []rune(acc.Cookie)
		prefix := 4
		if len(runes) < prefix {
			prefix = len(runes)
		}
		// 带上长度：从 DevTools 的 cookie 表格里复制时很容易只复制到截断的值，
		// 显示长度能让人一眼看出两个账号哪个是完整的那份。
		cookieHint = fmt.Sprintf("%s****（%d 字符）", string(runes[:prefix]), len(runes))
	}
	status := acc.Status
	// 平台没有在线校验能力时，历史数据里残留的 error 其实是"不支持校验"，
	// 不该在账号列表里一直飘红（这类平台只能靠浏览器脚本接入）。
	if status == "error" && !verifySupported(acc.Platform) {
		status = "unsupported"
	}
	return gin.H{"id": acc.ID, "platform": acc.Platform, "name": acc.Name, "handle": acc.Handle,
		"has_cookie": acc.Cookie != "", "cookie_hint": cookieHint, "verified": acc.Verified,
		"status": status, "message": acc.Message, "rating": acc.Rating, "solved": acc.Solved,
		"selected": acc.Selected, "last_checked_at": acc.LastCheckedAt}
}

func (a *API) setConfigValue(uid uint, key, value string) {
	row := models.UserConfig{UserID: uid, Key: key, Value: value}
	a.DB.Where("user_id = ? AND key = ?", uid, key).Assign(models.UserConfig{Value: value, UpdatedAt: time.Now()}).FirstOrCreate(&row)
}

func (a *API) clearConfigKeys(uid uint, keys []string) {
	if len(keys) == 0 {
		return
	}
	a.DB.Where("user_id = ? AND key IN ?", uid, keys).Delete(&models.UserConfig{})
}

// applyAccountToConfig 把某平台当前启用的账号写进 user_configs；没有启用账号时清空对应配置。
func (a *API) applyAccountToConfig(uid uint, platform string) {
	meta, ok := platformMeta[platform]
	if !ok {
		return
	}
	var acc models.PlatformAccount
	if err := a.DB.Where("user_id = ? AND platform = ? AND selected = ?", uid, platform, true).First(&acc).Error; err != nil {
		keys := []string{meta.HandleKey}
		if meta.CookieKey != "" {
			keys = append(keys, meta.CookieKey)
		}
		a.clearConfigKeys(uid, keys)
		return
	}
	a.setConfigValue(uid, meta.HandleKey, acc.Handle)
	if meta.CookieKey != "" {
		if acc.Cookie == "" {
			a.clearConfigKeys(uid, []string{meta.CookieKey})
		} else {
			a.setConfigValue(uid, meta.CookieKey, acc.Cookie)
		}
	}
}

func (a *API) ListAccounts(c *gin.Context) {
	var rows []models.PlatformAccount
	a.DB.Where("user_id = ?", userID(c)).Order("platform, selected DESC, id").Find(&rows)
	result := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		result = append(result, accountResponse(row))
	}
	c.JSON(200, gin.H{"accounts": result})
}

func (a *API) CreateAccount(c *gin.Context) {
	var req accountRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	meta, ok := platformMeta[platform]
	if !ok {
		jsonError(c, 400, "不支持的平台: "+req.Platform)
		return
	}
	handle := strings.TrimSpace(req.Handle)
	if handle == "" {
		jsonError(c, 400, meta.Label+" 的账号不能为空")
		return
	}
	if len(req.Cookie) > 1024 {
		jsonError(c, 400, "Cookie 过长")
		return
	}
	uid := userID(c)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = handle
	}
	var count int64
	a.DB.Model(&models.PlatformAccount{}).Where("user_id = ? AND platform = ?", uid, platform).Count(&count)
	selectIt := count == 0
	if req.Select != nil {
		selectIt = *req.Select
	}
	acc := models.PlatformAccount{UserID: uid, Platform: platform, Name: truncate(name, 64), Handle: truncate(handle, 255),
		Cookie: req.Cookie, Status: "unverified", Message: "尚未验证"}
	if selectIt {
		a.DB.Model(&models.PlatformAccount{}).Where("user_id = ? AND platform = ?", uid, platform).Update("selected", false)
		acc.Selected = true
	}
	if err := a.DB.Create(&acc).Error; err != nil {
		jsonError(c, 500, "账号保存失败")
		return
	}
	if req.Verify {
		a.verifyAccountRow(&acc)
	}
	if selectIt {
		a.applyAccountToConfig(uid, platform)
	}
	c.JSON(200, gin.H{"success": true, "message": "账号已保存", "account": accountResponse(acc)})
}

// verifyAccountRow 调平台公开接口验证账号，并把结果写回账号记录。
func (a *API) verifyAccountRow(acc *models.PlatformAccount) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cfg := platforms.Config{Platform: acc.Platform, CFHandle: acc.Handle, LuoguUID: acc.Handle, LeetCode: acc.Handle, AtCoder: acc.Handle, AcWingID: acc.Handle, LuoguCookie: acc.Cookie, AcWingCookie: acc.Cookie, LeetCodeCookie: acc.Cookie}
	profile, err := a.platformClient().Verify(ctx, cfg)
	if err != nil {
		if errors.Is(err, platforms.ErrUnsupportedVerify) {
			// 平台本身不支持在线校验：账号信息照常保存，只是标记为"未校验"，
			// 不要写成 error 让列表一直飘红。
			message := truncate(err.Error(), 255)
			a.DB.Model(acc).Updates(map[string]any{"verified": false, "status": "unsupported", "message": message, "last_checked_at": nowUTC()})
			acc.Verified, acc.Status, acc.Message, acc.LastCheckedAt = false, "unsupported", message, nowUTC()
			return
		}
		message := truncate(err.Error(), 255)
		if isTimeoutErr(err) {
			// 超时是瞬时的：配置照常保存，标记为 warning，避免账号列表飘红。
			// 注意这里只写库，HTTP 响应由调用方（VerifyAccount）负责。
			hint := "验证请求超时，账号配置已保存，可稍后点「验证」重试"
			a.DB.Model(acc).Updates(map[string]any{"verified": false, "status": "warning", "message": hint, "last_checked_at": nowUTC()})
			acc.Verified, acc.Status, acc.Message, acc.LastCheckedAt = false, "warning", hint, nowUTC()
			return
		}
		a.DB.Model(acc).Updates(map[string]any{"verified": false, "status": "error", "message": message, "last_checked_at": nowUTC()})
		acc.Status, acc.Message, acc.Verified, acc.LastCheckedAt = "error", message, false, nowUTC()
		return
	}
	message := "账号验证成功"
	if profile.Note != "" {
		message = profile.Note
	}
	message = truncate(message, 255)
	if err := a.DB.Model(acc).Updates(map[string]any{"verified": true, "status": "ok", "message": message,
		"rating": profile.Rating, "solved": profile.Solved, "last_checked_at": nowUTC()}).Error; err != nil {
		return
	}
	acc.Verified, acc.Status, acc.Message, acc.Rating, acc.Solved, acc.LastCheckedAt = true, "ok", message, profile.Rating, profile.Solved, nowUTC()
}

func (a *API) loadAccount(c *gin.Context) (*models.PlatformAccount, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		jsonError(c, 400, "账号 ID 无效")
		return nil, false
	}
	var acc models.PlatformAccount
	if a.DB.Where("id = ? AND user_id = ?", id, userID(c)).First(&acc).Error != nil {
		jsonError(c, 404, "账号不存在")
		return nil, false
	}
	return &acc, true
}

func (a *API) VerifyAccount(c *gin.Context) {
	acc, ok := a.loadAccount(c)
	if !ok {
		return
	}
	a.verifyAccountRow(acc)
	switch acc.Status {
	case "ok":
		c.JSON(200, gin.H{"success": true, "valid": true, "supported": true, "message": acc.Message, "account": accountResponse(*acc)})
	case "unsupported":
		// 平台没有公开校验接口：既不是成功也不是失败，前端按提示处理。
		c.JSON(200, gin.H{"success": false, "valid": false, "supported": false, "message": acc.Message, "account": accountResponse(*acc)})
	case "warning":
		// 验证请求超时等瞬时问题：给 200 + timeout 标记，前端用提示而非报错。
		c.JSON(200, gin.H{"success": false, "valid": false, "supported": true, "timeout": true, "message": acc.Message, "account": accountResponse(*acc)})
	default:
		c.JSON(400, gin.H{"success": false, "valid": false, "supported": true, "message": acc.Message, "account": accountResponse(*acc)})
	}
}

func (a *API) SelectAccount(c *gin.Context) {
	acc, ok := a.loadAccount(c)
	if !ok {
		return
	}
	a.DB.Model(&models.PlatformAccount{}).Where("user_id = ? AND platform = ?", acc.UserID, acc.Platform).Update("selected", false)
	a.DB.Model(acc).Update("selected", true)
	a.applyAccountToConfig(acc.UserID, acc.Platform)
	acc.Selected = true
	c.JSON(200, gin.H{"success": true, "message": "已启用 " + acc.Name, "account": accountResponse(*acc)})
}

func (a *API) DeleteAccount(c *gin.Context) {
	acc, ok := a.loadAccount(c)
	if !ok {
		return
	}
	a.DB.Delete(acc)
	if acc.Selected {
		// 启用中的账号被删掉时，自动顶替同平台剩下的第一个账号，避免配置悬空。
		var next models.PlatformAccount
		if a.DB.Where("user_id = ? AND platform = ?", acc.UserID, acc.Platform).Order("id").First(&next).Error == nil {
			a.DB.Model(&next).Update("selected", true)
		}
		a.applyAccountToConfig(acc.UserID, acc.Platform)
	}
	c.JSON(200, gin.H{"success": true, "message": "账号已删除"})
}

// SeedPlatformAccounts 把旧版单账号配置（user_configs）迁移成账号记录，
// 只对"该平台还没有任何账号"的用户生效，可安全重复执行。
func SeedPlatformAccounts(db *gorm.DB) {
	var users []models.User
	if err := db.Find(&users).Error; err != nil {
		return
	}
	for _, user := range users {
		var configs []models.UserConfig
		if err := db.Where("user_id = ?", user.ID).Find(&configs).Error; err != nil {
			continue
		}
		values := map[string]string{}
		for _, row := range configs {
			values[row.Key] = row.Value
		}
		for platform, meta := range platformMeta {
			handle := strings.TrimSpace(values[meta.HandleKey])
			if handle == "" {
				continue
			}
			var count int64
			db.Model(&models.PlatformAccount{}).Where("user_id = ? AND platform = ?", user.ID, platform).Count(&count)
			if count > 0 {
				continue
			}
			var status models.PlatformStatus
			verified := false
			if db.Where("user_id = ? AND platform = ?", user.ID, platform).First(&status).Error == nil {
				verified = status.Status == "ok"
			}
			account := models.PlatformAccount{UserID: user.ID, Platform: platform, Name: "默认账号", Handle: truncate(handle, 255),
				Cookie: values[meta.CookieKey], Selected: true, Verified: verified,
				Status: map[bool]string{true: "ok", false: "unverified"}[verified], Message: "由旧版配置迁移"}
			if err := db.Create(&account).Error; err == nil {
				// 立即启用，保证 user_configs 与账号记录一致。
				db.Model(&account).Update("selected", true)
			}
		}
	}
}

func (a *API) Contests(c *gin.Context) {
	platform := c.DefaultQuery("platform", "all")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	rows, err := a.platformClient().Contests(ctx, platform)
	if err != nil {
		jsonError(c, 502, err.Error())
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartTimestamp < rows[j].StartTimestamp })
	c.JSON(200, gin.H{"contests": rows, "platform": platform, "mode": "on_demand"})
}

func (a *API) ContestsSync(c *gin.Context) {
	platform := c.DefaultQuery("platform", "all")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	a.platformClient().InvalidateContestCache()
	rows, err := a.platformClient().Contests(ctx, platform)
	if err != nil {
		jsonError(c, 502, err.Error())
		return
	}
	c.JSON(200, gin.H{"success": true, "platform": platform, "synced": len(rows), "message": fmt.Sprintf("已获取 %d 场比赛", len(rows)), "synced_at": nowUTC()})
}

// ---------------------------------------------------------------------------
// 长效脚本 Token：独立于登录会话，只能调用 /api/ingest/*，可单独吊销 / 轮换
// ---------------------------------------------------------------------------

func ingestTokenResponse(row models.IngestToken, plain string) gin.H {
	return gin.H{"id": row.ID, "name": row.Name, "token": plain, "token_hint": row.TokenHint,
		"created_at": row.CreatedAt, "last_used_at": row.LastUsedAt, "revoked_at": row.RevokedAt}
}

// IngestToken 旧版「每次 GET 都新签一条 token」的接口，前端从未使用，
// 且 GET 建资源 + 无限流会导致凭证无限堆积。已废弃，改用 POST /api/ingest/tokens
// （带名称、可在设置页逐条查看/吊销/轮换）。前端只调用复数接口，这里直接 410。
func (a *API) IngestToken(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{"success": false, "message": "该接口已废弃，请改用 POST /api/ingest/tokens 创建长效脚本 Token"})
}

// issueIngestToken 返回入库记录与明文。明文仅在本次调用返回，之后只能靠 token_hint 辨认。
func (a *API) issueIngestToken(uid uint, name string) (models.IngestToken, string, error) {
	plain, hash, err := models.NewIngestTokenValue()
	if err != nil {
		return models.IngestToken{}, "", err
	}
	if name == "" {
		name = "浏览器脚本"
	}
	row := models.IngestToken{UserID: uid, Name: truncate(name, 64), TokenHash: hash, TokenHint: plain[:12]}
	if err := a.DB.Create(&row).Error; err != nil {
		return models.IngestToken{}, "", err
	}
	row.TokenHash = ""
	return row, plain, nil
}

func (a *API) ListIngestTokens(c *gin.Context) {
	var rows []models.IngestToken
	a.DB.Where("user_id = ?", userID(c)).Order("revoked_at, id DESC").Find(&rows)
	result := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		row.TokenHash = ""
		result = append(result, gin.H{"id": row.ID, "name": row.Name, "token_hint": row.TokenHint,
			"created_at": row.CreatedAt, "last_used_at": row.LastUsedAt, "revoked_at": row.RevokedAt})
	}
	c.JSON(200, gin.H{"tokens": result})
}

func (a *API) CreateIngestToken(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "浏览器脚本"
	}
	uid := userID(c)
	row, plain, err := a.issueIngestToken(uid, name)
	if err != nil {
		jsonError(c, 500, "Token 生成失败")
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "Token 已生成，请立即复制保存（之后不再显示明文）",
		"token": ingestTokenResponse(row, plain)})
}

// createIngestTokenWithPlain 已合并进 issueIngestToken

func (a *API) RotateIngestToken(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		jsonError(c, 400, "Token ID 无效")
		return
	}
	var row models.IngestToken
	if a.DB.Where("id = ? AND user_id = ?", id, userID(c)).First(&row).Error != nil {
		jsonError(c, 404, "Token 不存在")
		return
	}
	if row.RevokedAt != "" {
		jsonError(c, 400, "该 Token 已吊销，无法轮换")
		return
	}
	plain, hash, err := models.NewIngestTokenValue()
	if err != nil {
		jsonError(c, 500, "Token 生成失败")
		return
	}
	if err := a.DB.Model(&row).Updates(map[string]any{"token_hash": hash, "token_hint": plain[:12], "last_used_at": ""}).Error; err != nil {
		jsonError(c, 500, "轮换失败")
		return
	}
	row.TokenHash = ""
	row.TokenHint = plain[:12]
	c.JSON(200, gin.H{"success": true, "message": "已轮换，旧 Token 立即失效，请更新浏览器脚本",
		"token": ingestTokenResponse(row, plain)})
}

func (a *API) RevokeIngestToken(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		jsonError(c, 400, "Token ID 无效")
		return
	}
	result := a.DB.Model(&models.IngestToken{}).Where("id = ? AND user_id = ? AND revoked_at = ''", id, userID(c)).
		Update("revoked_at", nowUTC())
	if result.Error != nil {
		jsonError(c, 500, "吊销失败")
		return
	}
	if result.RowsAffected == 0 {
		jsonError(c, 404, "Token 不存在或已吊销")
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "Token 已吊销，使用该 Token 的脚本会立即失效"})
}

// revokeAllIngestTokens 改密码时连带吊销，避免"改了密码脚本还能用"。
func (a *API) revokeAllIngestTokens(uid uint) {
	a.DB.Model(&models.IngestToken{}).Where("user_id = ? AND revoked_at = ''", uid).Update("revoked_at", nowUTC())
}
func (a *API) IngestSubmission(c *gin.Context) {
	var req ingestRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	inserted, err := a.saveSubmission(userID(c), req)
	if err != nil {
		jsonError(c, 500, "提交记录保存失败")
		log.Printf("ingest 写入失败: %v", err)
		return
	}
	c.JSON(200, gin.H{"success": true, "inserted": inserted, "message": "提交记录已接收"})
}
func (a *API) IngestBatch(c *gin.Context) {
	var req ingestBatchRequest
	if c.ShouldBindJSON(&req) != nil {
		jsonError(c, 400, "请求格式错误")
		return
	}
	inserted := 0
	for _, item := range req.Submissions {
		n, err := a.saveSubmission(userID(c), item)
		if err != nil {
			jsonError(c, 500, "提交记录保存失败")
			log.Printf("ingest 写入失败: %v", err)
			return
		}
		inserted += n
	}
	c.JSON(200, gin.H{"success": true, "inserted": inserted, "received": len(req.Submissions)})
}

// verdictAliases 把各平台五花八门的判定收敛成看板识别的几档。
// 浏览器脚本直接把 OJ 返回的原始 verdict 透传进来（例如 Codeforces 的 "OK"），
// 不做归一化的话通过记录不会被计入 AC，热力图和 AC 统计都会漏。
var verdictAliases = map[string]string{
	"OK": "AC", "ACCEPTED": "AC", "ANSWER_CORRECT": "AC", "PASSED": "AC", "SUCCESS": "AC",
	"WRONG_ANSWER": "WA", "WRONG-ANSWER": "WA", "WRONGANSWER": "WA", "答案错误": "WA", "NO": "WA",
	"TIME_LIMIT_EXCEEDED": "TLE", "TIME_LIMIT": "TLE", "TLE_EXCEEDED": "TLE", "时间限制": "TLE",
	"MEMORY_LIMIT_EXCEEDED": "MLE", "MEMORY_LIMIT": "MLE", "内存限制": "MLE",
	"COMPILATION_ERROR": "CE", "COMPILE_ERROR": "CE", "编译错误": "CE",
	"RUNTIME_ERROR": "RE", "运行错误": "RE",
	// 带空格的写法（力扣 statusDisplay、Codeforces 状态文案）也要归一，
	// 否则看板上会出现 "WRONG ANSWER" 这种既不算 AC 也不好看的判定。
	"WRONG ANSWER": "WA", "TIME LIMIT EXCEEDED": "TLE", "MEMORY LIMIT EXCEEDED": "MLE",
	"COMPILE ERROR": "CE", "RUNTIME ERROR": "RE", "OUTPUT LIMIT EXCEEDED": "OLE",
	"IDLENESS LIMIT EXCEEDED": "ILE",
	"JUDGING":                 "PENDING", "QUEUE": "PENDING", "QUEUING": "PENDING", "IN_QUEUE": "PENDING",
	"WAITING": "PENDING", "TESTING": "PENDING", "PENDING": "PENDING",
}

func normalizeVerdict(verdict string) string {
	verdict = strings.ToUpper(strings.TrimSpace(verdict))
	if verdict == "" {
		return "UNKNOWN"
	}
	if mapped, ok := verdictAliases[verdict]; ok {
		return mapped
	}
	return verdict
}

func (a *API) saveSubmission(uid uint, req ingestRequest) (int, error) {
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		return 0, fmt.Errorf("platform 必填")
	}
	raw := strings.TrimSpace(req.RawID)
	if raw == "" {
		// 回退值必须与入库格式一致：用归一化后的时间，否则同一条提交以
		// RFC3339 和 "YYYY-MM-DD HH:mm:ss" 两种格式各上报一次会生成两个 raw_id，
		// 导致 OnConflict 去重失效、重复记录、AC 统计虚高。
		raw = fmt.Sprintf("%s:%s:%s", platform, req.ProblemID, normalizeSubmittedAt(req.SubmittedAt))
	}
	// 主键与 raw_id 列宽 255，超长会直接写入失败并让整批 ingest 回滚。
	raw = truncate(raw, 200)
	problemID := truncate(strings.TrimSpace(req.ProblemID), 200)
	if problemID == "" {
		problemID = raw
	}
	title := truncate(strings.TrimSpace(req.ProblemTitle), 300)
	if title == "" {
		title = problemID
	}
	verdict := normalizeVerdict(req.Verdict)
	submittedAt := normalizeSubmittedAt(req.SubmittedAt)
	tags, _ := json.Marshal(req.Tags)
	extra, _ := json.Marshal(req.ExtraData)
	meta, _ := json.Marshal(req.RequestMeta)
	now := nowUTC()
	id := fmt.Sprintf("u%d_%s_%s", uid, platform, raw)
	submission := models.Submission{ID: id, UserID: uid, Platform: platform, RawID: raw, ProblemID: problemID, ProblemTitle: title, Verdict: verdict, Tags: string(tags), Difficulty: req.Difficulty, DifficultyScore: req.DifficultyScore, SubmittedAt: submittedAt, Date: effectiveDate(parseTime(submittedAt)), SubmissionURL: req.SubmissionURL, CodeLanguage: req.CodeLanguage, ExtraData: string(extra)}
	var inserted int
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		// 先判断是否为新记录：Upsert 无论插入还是更新都返回 RowsAffected=1，
		// 不先查一次就会把重复上报也算成"新增 N 条"。
		var existing int64
		if err := tx.Model(&models.Submission{}).Where("user_id = ? AND platform = ? AND raw_id = ?", uid, platform, raw).Count(&existing).Error; err != nil {
			return err
		}
		inserted = 0
		if existing == 0 {
			inserted = 1
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "platform"}, {Name: "raw_id"}}, DoUpdates: clause.AssignmentColumns([]string{"problem_id", "problem_title", "verdict", "tags", "difficulty", "difficulty_score", "submitted_at", "date", "submission_url", "code_language", "extra_data"})}).Create(&submission).Error; err != nil {
			return err
		}
		event := models.IngestEvent{ID: fmt.Sprintf("%d_%s_%d", uid, raw, time.Now().UnixNano()), UserID: uid, Platform: platform, RawID: raw, RequestMeta: string(meta), ResponseData: string(extra), Source: req.Source, ReceivedAt: now}
		if event.Source == "" {
			event.Source = "browser"
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "platform"}, {Name: "raw_id"}}, DoUpdates: clause.AssignmentColumns([]string{"request_meta", "response_data", "source", "received_at"})}).Create(&event).Error; err != nil {
			return err
		}
		var count int64
		tx.Model(&models.Submission{}).Where("user_id = ? AND platform = ?", uid, platform).Count(&count)
		status := models.PlatformStatus{UserID: uid, Platform: platform, Status: "ok", Message: "浏览器脚本实时接入", ItemCount: int(count), LastCheckedAt: now}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "platform"}}, DoUpdates: clause.AssignmentColumns([]string{"status", "message", "item_count", "last_checked_at"})}).Create(&status).Error
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

func truncate(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
func (a *API) IngestEvents(c *gin.Context) {
	var rows []models.IngestEvent
	a.DB.Where("user_id = ?", userID(c)).Order("received_at DESC").Limit(queryLimit(c, 100)).Find(&rows)
	c.JSON(200, gin.H{"events": rows})
}

func (a *API) streak(uid uint) int {
	var rows []struct{ Date string }
	a.DB.Model(&models.Submission{}).Select("DISTINCT date").Where("user_id = ? AND verdict = ?", uid, "AC").Scan(&rows)
	dates := map[string]bool{}
	for _, row := range rows {
		dates[row.Date] = true
	}
	if len(dates) == 0 {
		return 0
	}
	loc := displayLocation()
	now := time.Now().In(loc)
	// 今天还没 AC 时不应直接把连续天数清零，从昨天继续往前数，
	// 否则每天早上打开看板都会看到 streak 变成 0。
	cursor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if !dates[cursor.Format("2006-01-02")] {
		cursor = cursor.AddDate(0, 0, -1)
	}
	count := 0
	for dates[cursor.Format("2006-01-02")] {
		count++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return count
}

// displayLocation 返回看板展示时区。系统缺少 tzdata 时（精简容器/部分 Linux
// 发行版）LoadLocation 会失败，此时必须回退到固定的 UTC+8，否则 t.In(nil) 会 panic。
// 结果用 sync.Once 缓存，避免批量入库时反复 LoadLocation 触发上千次磁盘 IO。
func displayLocation() *time.Location {
	displayLocOnce.Do(func() {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			displayLoc = time.FixedZone("CST", 8*3600)
			return
		}
		displayLoc = loc
	})
	return displayLoc
}

// effectiveDate 按展示时区返回自然日。此前这里额外减去 4 小时，导致凌晨的提交
// 被算进前一天，与前端热力图日历、"今天"筛选的口径不一致。
func effectiveDate(t time.Time) string {
	return t.In(displayLocation()).Format("2006-01-02")
}

// parseTime 兼容 RFC3339、"YYYY-MM-DD HH:mm:ss"、纯日期以及 Unix 秒/毫秒时间戳。
func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now()
	}
	loc := displayLocation()
	if result, err := time.Parse(time.RFC3339, value); err == nil {
		return result
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.Unix(n/1000, (n%1000)*int64(time.Millisecond))
		}
		return time.Unix(n, 0)
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
		"2006-01-02T15:04:05", "2006-01-02T15:04",
		"2006/01/02 15:04:05", "2006/01/02",
	} {
		if result, err := time.ParseInLocation(layout, value, loc); err == nil {
			return result
		}
	}
	// 脏时间兜底成当前时间，但至少留一条日志，避免热力图错日却毫无痕迹。
	log.Printf("parseTime: 无法解析提交时间 %q，回退为当前时间", value)
	return time.Now()
}

// NormalizeSubmissionTimes 修复历史数据：早期版本混用 RFC3339 与本地时间字符串，
// 同一天内排序必然错乱。启动时把所有记录统一成展示时区的
// "YYYY-MM-DD HH:mm:ss" 并重算 date，保证老数据也能正确排序和筛选。
// 改为按 id 分批（LIMIT）处理、每批单事务，循环到没有可修的行为止，
// 避免数据量大时启动极慢、对生产库长时间持锁。
func NormalizeSubmissionTimes(db *gorm.DB) error {
	const batchSize = 500
	var lastID string
	for {
		var rows []models.Submission
		if err := db.Select("id, submitted_at, date").Where("id > ?", lastID).
			Order("id").Limit(batchSize).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		lastID = rows[len(rows)-1].ID
		// 每批在单事务内更新，失败整体回滚，避免部分写入。
		if err := db.Transaction(func(tx *gorm.DB) error {
			for _, row := range rows {
				normalized := normalizeSubmittedAt(row.SubmittedAt)
				date := effectiveDate(parseTime(normalized))
				if normalized == row.SubmittedAt && date == row.Date {
					continue
				}
				if err := tx.Model(&models.Submission{}).Where("id = ?", row.ID).
					Updates(map[string]any{"submitted_at": normalized, "date": date}).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// normalizeSubmittedAt 统一入库格式为展示时区的 "YYYY-MM-DD HH:mm:ss"。
// 之前浏览器脚本写 RFC3339、手动同步写本地格式，两种格式混排会让
// ORDER BY submitted_at 的字符串比较与时间先后不一致（同一天内必然错序）。
func normalizeSubmittedAt(value string) string {
	return parseTime(value).In(displayLocation()).Format("2006-01-02 15:04:05")
}
