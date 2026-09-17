package models

import (
	"time"
)

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:32;not null" json:"username"`
	PasswordHash string    `gorm:"not null" json:"-"`
	Salt         string    `gorm:"not null" json:"-"`
	IsAdmin      bool      `json:"is_admin"`
	CreatedAt    time.Time `json:"created_at"`
}

type Session struct {
	Token     string    `gorm:"primaryKey;size:64" json:"-"`
	UserID    uint      `gorm:"index;not null" json:"-"`
	ExpiresAt string    `gorm:"not null" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

type UserConfig struct {
	UserID    uint      `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	Key       string    `gorm:"primaryKey;size:100" json:"key"`
	Value     string    `gorm:"not null" json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PlatformStatus struct {
	UserID        uint   `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	Platform      string `gorm:"primaryKey;size:32" json:"platform"`
	Status        string `gorm:"not null;default:unconfigured" json:"status"`
	Message       string `json:"message"`
	ItemCount     int    `json:"item_count"`
	Rating        string `json:"rating"`
	LastCheckedAt string `json:"last_checked_at"`
}

// PlatformAccount 保存某个平台下的一个候选账号。同一平台可存多个，
// 但每个 (user, platform) 只有 selected=true 的那一个生效——它的
// handle/cookie 会被同步写进 user_configs，供历史同步与统计使用。
type PlatformAccount struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	UserID        uint      `gorm:"index:idx_account_user_platform;not null" json:"-"`
	Platform      string    `gorm:"index:idx_account_user_platform;size:32;not null" json:"platform"`
	Name          string    `gorm:"size:64" json:"name"`
	Handle        string    `gorm:"size:255;not null" json:"handle"`
	Cookie        string    `gorm:"size:1024" json:"-"`
	Verified      bool      `json:"verified"`
	Status        string    `gorm:"size:32;default:unverified" json:"status"`
	Message       string    `json:"message"`
	Rating        string    `json:"rating"`
	Solved        int       `json:"solved"`
	Selected      bool      `json:"selected"`
	LastCheckedAt string    `json:"last_checked_at"`
	CreatedAt     time.Time `json:"created_at"`
}

type Submission struct {
	ID              string    `gorm:"primaryKey;size:255" json:"id"`
	UserID          uint      `gorm:"uniqueIndex:idx_submission_user_platform_raw;index:idx_submission_user_platform_problem;index:idx_submission_user_date;index:idx_submission_user_submitted;not null" json:"-"`
	Platform        string    `gorm:"uniqueIndex:idx_submission_user_platform_raw;index:idx_submission_user_platform_problem;size:32;not null" json:"platform"`
	RawID           string    `gorm:"uniqueIndex:idx_submission_user_platform_raw;size:255;not null" json:"raw_id"`
	ProblemID       string    `gorm:"index:idx_submission_user_platform_problem" json:"problem_id"`
	ProblemTitle    string    `json:"problem_title"`
	Verdict         string    `json:"verdict"`
	Tags            string    `json:"tags"`
	Difficulty      string    `json:"difficulty"`
	DifficultyScore int       `json:"difficulty_score"`
	SubmittedAt     string    `gorm:"index:idx_submission_user_submitted;size:32" json:"submitted_at"`
	Date            string    `gorm:"index:idx_submission_user_date;size:16" json:"date"`
	SubmissionURL   string    `json:"submission_url"`
	CodeLanguage    string    `json:"code_language"`
	ExtraData       string    `json:"extra_data"`
	CreatedAt       time.Time `json:"created_at"`
}

type IngestEvent struct {
	ID           string `gorm:"primaryKey;size:255" json:"id"`
	UserID       uint   `gorm:"uniqueIndex:idx_event_user_platform_raw;index:idx_event_user_received;not null" json:"-"`
	Platform     string `gorm:"uniqueIndex:idx_event_user_platform_raw;size:32;not null" json:"platform"`
	RawID        string `gorm:"uniqueIndex:idx_event_user_platform_raw;size:255;not null" json:"raw_id"`
	RequestMeta  string `json:"request_meta"`
	ResponseData string `json:"response_data"`
	Source       string `json:"source"`
	ReceivedAt   string `gorm:"index:idx_event_user_received;size:40" json:"received_at"`
}

// Problem 是平台公开题库的本地缓存行，(platform, problem_id) 为自然键。
// 题库由用户手动触发全量同步写入，检索/筛选/翻页全部落在本地 SQLite 上，
// 浏览不再实时打平台公开接口。
type Problem struct {
	Platform   string `gorm:"primaryKey;size:32" json:"platform"`
	ProblemID  string `gorm:"primaryKey;size:128;column:problem_id" json:"problem_id"`
	Title      string `gorm:"size:300" json:"title"`
	URL        string `gorm:"size:500" json:"url"`
	Difficulty string `gorm:"size:64" json:"difficulty"`
	Rating     int    `json:"rating"`
	Tags       string `gorm:"size:1000" json:"tags"` // JSON 字符串数组
	UpdatedAt  string `gorm:"size:40" json:"updated_at"`
}

// ProblemTag 把标签拆成行，标签筛选与标签聚合（facet）才能走索引。
type ProblemTag struct {
	Platform  string `gorm:"primaryKey;size:32" json:"platform"`
	ProblemID string `gorm:"primaryKey;size:128;column:problem_id" json:"problem_id"`
	Tag       string `gorm:"primaryKey;size:64;index" json:"tag"`
}
