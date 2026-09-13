package models

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// IngestTokenPrefix 用来把长效脚本 token 和登录会话 token 区分开：
// 会话 token 是纯 64 位十六进制，脚本 token 带这个前缀。
const IngestTokenPrefix = "cah_"

// IngestToken 是给 Tampermonkey 脚本用的长效凭证：
// 独立于登录会话（不会 7 天过期），只能调用 /api/ingest/*，可在设置页单独吊销或轮换。
// 库里只存哈希，明文仅在创建/轮换时返回一次。
type IngestToken struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"index;not null" json:"-"`
	Name       string    `gorm:"size:64" json:"name"`
	TokenHash  string    `gorm:"uniqueIndex;size:64;not null" json:"-"`
	TokenHint  string    `gorm:"size:16" json:"token_hint"`
	LastUsedAt string    `json:"last_used_at"`
	RevokedAt  string    `json:"revoked_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func HashIngestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewIngestTokenValue 生成明文 token 及其哈希。
func NewIngestTokenValue() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := IngestTokenPrefix + hex.EncodeToString(buf)
	return token, HashIngestToken(token), nil
}

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
	UserID          uint      `gorm:"uniqueIndex:idx_submission_user_platform_raw;not null" json:"-"`
	Platform        string    `gorm:"uniqueIndex:idx_submission_user_platform_raw;size:32;not null" json:"platform"`
	RawID           string    `gorm:"uniqueIndex:idx_submission_user_platform_raw;size:255;not null" json:"raw_id"`
	ProblemID       string    `json:"problem_id"`
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
	UserID       uint   `gorm:"uniqueIndex:idx_event_user_platform_raw;not null" json:"-"`
	Platform     string `gorm:"uniqueIndex:idx_event_user_platform_raw;size:32;not null" json:"platform"`
	RawID        string `gorm:"uniqueIndex:idx_event_user_platform_raw;size:255;not null" json:"raw_id"`
	RequestMeta  string `json:"request_meta"`
	ResponseData string `json:"response_data"`
	Source       string `json:"source"`
	ReceivedAt   string `json:"received_at"`
}
