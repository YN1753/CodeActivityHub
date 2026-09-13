package middleware

import (
	"net/http"
	"strings"
	"time"

	"codeactivityhub/backend/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const UserIDKey = "user_id"
const SessionTokenKey = "session_token"

// ingestTokenPaths 是长效脚本 token 唯一允许访问的接口白名单。
var ingestTokenPaths = map[string]bool{
	"/api/ingest/submission":  true,
	"/api/ingest/submissions": true,
}

func RequireAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 统一按 "Bearer <token>" 大小写不敏感解析，避免部分客户端发送
		// 小写 "bearer " 时被判成未授权。
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		token := ""
		if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
			token = strings.TrimSpace(header[7:])
		} else if header != "" && !strings.Contains(header, " ") {
			token = header
		}
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "detail": "未授权，请先登录"})
			c.Abort()
			return
		}

		// 长效脚本 token：独立生命周期，但只能用于"提交记录接入"这两个入口。
		// 这里用精确白名单而不是 /api/ingest/ 前缀——否则一个泄露的脚本 token
		// 还能调用令牌管理接口（列出/新建/轮换/吊销其他人的令牌）。
		if strings.HasPrefix(token, models.IngestTokenPrefix) {
			var ingest models.IngestToken
			if err := db.Where("token_hash = ?", models.HashIngestToken(token)).First(&ingest).Error; err != nil || ingest.RevokedAt != "" {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "detail": "脚本 Token 无效或已被吊销"})
				c.Abort()
				return
			}
			if !ingestTokenPaths[c.Request.URL.Path] {
				c.JSON(http.StatusForbidden, gin.H{"success": false, "detail": "脚本 Token 只能用于提交记录接入"})
				c.Abort()
				return
			}
			now := time.Now().UTC().Format(time.RFC3339)
			db.Model(&ingest).Update("last_used_at", now)
			c.Set(UserIDKey, ingest.UserID)
			c.Set(SessionTokenKey, token)
			c.Next()
			return
		}

		var session models.Session
		if err := db.Where("token = ?", token).First(&session).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "detail": "登录会话已过期，请重新登录"})
			c.Abort()
			return
		}
		expires, err := time.Parse(time.RFC3339, session.ExpiresAt)
		if err != nil || time.Now().UTC().After(expires) {
			db.Delete(&session)
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "detail": "登录会话已过期，请重新登录"})
			c.Abort()
			return
		}
		c.Set(UserIDKey, session.UserID)
		c.Set(SessionTokenKey, token)
		c.Next()
	}
}
