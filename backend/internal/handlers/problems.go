package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeactivityhub/backend/internal/models"
	"codeactivityhub/backend/internal/platforms"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------- 题库本地缓存 ----------
//
// 题库数据量在几万行级别且变化很慢：全量同步进 SQLite 后，检索/筛选/翻页
// 全部落在本地，浏览不再实时打平台公开接口。同步沿用项目的"用户主动触发、
// 不后台轮询"哲学，按平台一次性拉全，边拉边落库。

// problemSyncStatus 单个平台同步的实时进度（内存态，进程重启即清零）。
type problemSyncStatus struct {
	Running    bool   `json:"running"`
	Platform   string `json:"platform"`
	Items      int    `json:"items"`
	Total      int    `json:"total"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type problemSyncManager struct {
	mu      sync.Mutex
	running map[string]bool
	status  map[string]*problemSyncStatus
}

func newProblemSyncManager() *problemSyncManager {
	return &problemSyncManager{running: map[string]bool{}, status: map[string]*problemSyncStatus{}}
}

func (m *problemSyncManager) start(platform string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[platform] {
		return false
	}
	m.running[platform] = true
	m.status[platform] = &problemSyncStatus{Running: true, Platform: platform, StartedAt: nowUTC()}
	return true
}

func (m *problemSyncManager) progress(platform string, items, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.status[platform]; s != nil {
		s.Items, s.Total = items, total
	}
}

func (m *problemSyncManager) finish(platform string, syncErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running[platform] = false
	if s := m.status[platform]; s != nil {
		s.Running = false
		s.FinishedAt = nowUTC()
		if syncErr != nil {
			s.Error = syncErr.Error()
		}
	}
}

func (m *problemSyncManager) snapshot(platform string) problemSyncStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.status[platform]; s != nil {
		return *s
	}
	return problemSyncStatus{Platform: platform}
}

// syncMgr 惰性初始化：API 由 main.go 字面量构造，这里不动它的构造方式。
func (a *API) syncMgr() *problemSyncManager {
	a.problemSyncOnce.Do(func() {
		if a.ProblemSync == nil {
			a.ProblemSync = newProblemSyncManager()
		}
	})
	return a.ProblemSync
}

// Problems 题库检索：读本地 problems 表，关键词/难度/标签/解决状态在 SQL 里
// 过滤，分页在本地完成。facets 返回当前平台的难度/标签分布等筛选维度。
func (a *API) Problems(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.DefaultQuery("platform", "codeforces")))
	keyword := strings.TrimSpace(c.Query("keyword"))
	difficulty := strings.TrimSpace(c.Query("difficulty"))
	tag := strings.TrimSpace(c.Query("tag"))
	solved := strings.TrimSpace(c.DefaultQuery("solved", "all"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 30
	}

	// 结束器（Count/Find）执行后不能复用同一个查询链，
	// 这里用 Session 克隆出两个互不污染的分支。
	q := a.problemsQuery(platform, keyword, difficulty, tag, solved, userID(c))
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		jsonError(c, 500, "题库查询失败")
		return
	}
	var rows []models.Problem
	// LENGTH + 字典序实现自然排序："1A" < "999X" < "1000A" < "2263B"。
	if err := q.Session(&gorm.Session{}).
		Order("LENGTH(problem_id) ASC, problem_id ASC").
		Offset((page - 1) * limit).Limit(limit).Find(&rows).Error; err != nil {
		jsonError(c, 500, "题库查询失败")
		return
	}

	// 当页题目的"已解决"标记：一次查出当前用户在这些题上的 AC 记录
	acSet := map[string]bool{}
	if len(rows) > 0 {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ProblemID)
		}
		var acIDs []string
		a.DB.Model(&models.Submission{}).Distinct().
			Where("user_id = ? AND platform = ? AND verdict = 'AC' AND problem_id IN ?", userID(c), platform, ids).
			Pluck("problem_id", &acIDs)
		for _, id := range acIDs {
			acSet[id] = true
		}
	}

	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		var tags []string
		_ = json.Unmarshal([]byte(r.Tags), &tags)
		if tags == nil {
			tags = []string{}
		}
		out = append(out, gin.H{
			"platform": r.Platform, "id": r.ProblemID, "title": r.Title, "url": r.URL,
			"difficulty": r.Difficulty, "rating": r.Rating, "tags": tags,
			"solved": acSet[r.ProblemID], "updated_at": r.UpdatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"platform": platform, "page": page, "limit": limit, "total": total,
		"problems": out,
		"facets":   a.problemFacets(platform, userID(c)),
		"sync":     a.syncMgr().snapshot(platform),
	})
}

// problemsQuery 组装题库检索条件；Count 与 Find 各自 Session 克隆使用。
func (a *API) problemsQuery(platform, keyword, difficulty, tag, solved string, uid uint) *gorm.DB {
	q := a.DB.Model(&models.Problem{}).Where("platform = ?", platform)
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("(title LIKE ? OR problem_id LIKE ?)", like, like)
	}
	if difficulty != "" {
		q = q.Where("difficulty = ?", difficulty)
	}
	if tag != "" {
		q = q.Where("EXISTS (SELECT 1 FROM problem_tags pt WHERE pt.platform = problems.platform AND pt.problem_id = problems.problem_id AND pt.tag = ?)", tag)
	}
	const solvedCond = "EXISTS (SELECT 1 FROM submissions s WHERE s.user_id = ? AND s.platform = problems.platform AND s.problem_id = problems.problem_id AND s.verdict = 'AC')"
	switch solved {
	case "solved":
		q = q.Where(solvedCond, uid)
	case "unsolved":
		q = q.Where("NOT "+solvedCond, uid)
	}
	return q
}

// problemFacets 返回当前平台的筛选维度：难度分布、高频标签、已解决数、最近同步时间。
// 口径是"该平台全量"，不随其它筛选联动，保证前端下拉选项稳定。
func (a *API) problemFacets(platform string, uid uint) gin.H {
	difficultyRows := []struct {
		Difficulty string
		N          int
	}{}
	a.DB.Model(&models.Problem{}).Select("difficulty, COUNT(*) AS n").
		Where("platform = ?", platform).Group("difficulty").Order("n DESC").Scan(&difficultyRows)
	difficulties := make([]gin.H, 0, len(difficultyRows))
	for _, r := range difficultyRows {
		if strings.TrimSpace(r.Difficulty) != "" {
			difficulties = append(difficulties, gin.H{"value": r.Difficulty, "count": r.N})
		}
	}

	tagRows := []struct {
		Tag string
		N   int
	}{}
	a.DB.Model(&models.ProblemTag{}).Select("tag, COUNT(*) AS n").
		Where("platform = ?", platform).Group("tag").Order("n DESC").Limit(200).Scan(&tagRows)
	tags := make([]gin.H, 0, len(tagRows))
	for _, r := range tagRows {
		tags = append(tags, gin.H{"value": r.Tag, "count": r.N})
	}

	var total, solved int64
	a.DB.Model(&models.Problem{}).Where("platform = ?", platform).Count(&total)
	a.DB.Model(&models.Problem{}).Where("platform = ?", platform).
		Where("EXISTS (SELECT 1 FROM submissions s WHERE s.user_id = ? AND s.platform = problems.platform AND s.problem_id = problems.problem_id AND s.verdict = 'AC')", uid).
		Count(&solved)

	var lastSync string
	a.DB.Raw("SELECT COALESCE(MAX(updated_at), '') FROM problems WHERE platform = ?", platform).Scan(&lastSync)

	return gin.H{"difficulties": difficulties, "tags": tags, "solved": solved, "total": total, "updated_at": lastSync}
}

// ProblemsSync 触发单个平台题库全量同步：立即返回，后台协程拉取 + 落库，
// 前端轮询 /api/problems 响应里的 sync 字段看进度。
func (a *API) ProblemsSync(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.DefaultQuery("platform", "codeforces")))
	switch platform {
	case "codeforces", "leetcode", "atcoder", "luogu":
	default:
		jsonError(c, 400, "暂不支持同步的平台: "+platform)
		return
	}
	if !a.syncMgr().start(platform) {
		jsonError(c, 409, "该平台题库正在同步中，请稍候")
		return
	}
	go a.runProblemSync(platform)
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "题库同步已开始", "sync": a.syncMgr().snapshot(platform)})
}

func (a *API) runProblemSync(platform string) {
	var syncErr error
	defer func() {
		// 兜底：回调或 AllProblems 里若发生 panic，记成同步失败，
		// 避免 running 标记永久卡死、拖垮整个进程。
		if r := recover(); r != nil {
			syncErr = fmt.Errorf("题库同步协程 panic: %v", r)
		}
		a.syncMgr().finish(platform, syncErr)
	}()

	// 每拉到一页立刻 upsert：长同步中途失败/重启时，已拉取的部分仍然可检索。
	// 带 30 分钟超时，避免平台接口卡死导致 running 标记永久卡住。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var callbackErr error
	retErr := a.Platforms.AllProblems(ctx, platform, func(rows []platforms.Problem, done, total int) {
		a.syncMgr().progress(platform, done, total)
		if err := a.saveProblems(platform, rows); err != nil {
			callbackErr = err
		}
	})
	// 回调里的写库错误与 AllProblems 本身的返回值分开保存：
	// 只要回调中出过错就保留，避免被成功时的 nil 覆盖而静默吞掉。
	if callbackErr != nil {
		syncErr = callbackErr
	} else {
		syncErr = retErr
	}
}

// saveProblems 分批 upsert：边拉边写，中途失败时已完成的部分不回滚。
// 标签行以本次拉取为准，先清这批题的旧标签再插入。
func (a *API) saveProblems(platform string, rows []platforms.Problem) error {
	const batch = 200
	now := nowUTC()
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		problems := make([]models.Problem, 0, end-start)
		tags := make([]models.ProblemTag, 0, (end-start)*4)
		for _, p := range rows[start:end] {
			pid := truncate(strings.TrimSpace(p.ID), 128)
			if pid == "" {
				continue
			}
			encoded, _ := json.Marshal(p.Tags)
			problems = append(problems, models.Problem{
				Platform: platform, ProblemID: pid,
				Title:      truncate(strings.TrimSpace(p.Title), 300),
				URL:        truncate(strings.TrimSpace(p.URL), 500),
				Difficulty: truncate(strings.TrimSpace(p.Difficulty), 64),
				Rating:     p.Rating,
				Tags:       string(encoded),
				UpdatedAt:  now,
			})
			seen := map[string]bool{}
			for _, t := range p.Tags {
				tag := truncate(strings.TrimSpace(t), 64)
				if tag == "" || seen[tag] {
					continue
				}
				seen[tag] = true
				tags = append(tags, models.ProblemTag{Platform: platform, ProblemID: pid, Tag: tag})
			}
		}
		if len(problems) == 0 {
			continue
		}
		ids := make([]string, 0, len(problems))
		for _, p := range problems {
			ids = append(ids, p.ProblemID)
		}
		err := a.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&problems).Error; err != nil {
				return err
			}
			if err := tx.Where("platform = ? AND problem_id IN ?", platform, ids).Delete(&models.ProblemTag{}).Error; err != nil {
				return err
			}
			if len(tags) > 0 {
				return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&tags).Error
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
