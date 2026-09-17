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
