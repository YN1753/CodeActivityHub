package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"codeactivityhub/backend/internal/config"
	"codeactivityhub/backend/internal/database"
	"codeactivityhub/backend/internal/handlers"
	"codeactivityhub/backend/internal/platforms"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.Load()
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		panic(err)
	}

	if os.Getenv("GIN_MODE") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// 前端与后端通常同源部署，此时不需要 CORS；只有在 Vite dev server
	// 或独立域名部署时才需要放行，默认保留常用本地来源，额外来源用
	// CODEACTIVITYHUB_CORS_ORIGINS（逗号分隔）配置。
	r.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins(),
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "Cache-Control"},
		AllowCredentials: true,
	}))

	api := &handlers.API{DB: db, Platforms: platforms.NewClient()}
	// 清洗历史提交记录的时间格式，否则旧数据仍会按错误的字符串顺序返回。
	if err := handlers.NormalizeSubmissionTimes(db); err != nil {
		log.Printf("警告：提交记录时间格式清洗失败: %v", err)
	}
	// 把旧版单账号配置迁移成多账号记录（幂等，可重复执行）。
	handlers.SeedPlatformAccounts(db)
	api.RegisterRoutes(r)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "CodeActivityHub"}) })
	registerFrontend(r, cfg.FrontendDist)

	if err := r.Run(cfg.HTTPAddr); err != nil {
		panic(err)
	}
}

func allowedOrigins() []string {
	origins := []string{"http://127.0.0.1:5173", "http://localhost:5173", "http://127.0.0.1:2053", "http://localhost:2053"}
	for _, item := range strings.Split(os.Getenv("CODEACTIVITYHUB_CORS_ORIGINS"), ",") {
		if item = strings.TrimSpace(item); item != "" {
			origins = append(origins, item)
		}
	}
	return origins
}

func registerFrontend(r *gin.Engine, dist string) {
	index := filepath.Join(dist, "index.html")
	if _, err := os.Stat(index); err != nil {
		// Never return a confusing Gin 404 when the frontend has not been built.
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/healthz" {
				c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
				return
			}
			c.String(http.StatusServiceUnavailable, "CodeActivityHub 前端尚未构建，请先执行：cd frontend && npm install && npm run build")
		})
		return
	}

	// index.html 引用带哈希的 /assets 文件，重新构建后旧哈希会被删除；
	// 必须禁用 index.html 缓存，否则浏览器会因引用失效的旧资源而白屏/点击无效。
	noCache := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	r.GET("/", func(c *gin.Context) {
		noCache(c)
		c.File(index)
	})
	r.StaticFile("/favicon.svg", filepath.Join(dist, "favicon.svg"))
	r.Static("/assets", filepath.Join(dist, "assets"))
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/healthz" {
			c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
			return
		}
		// Vite emits hashed assets under /assets. Unknown browser routes fall back to SPA index.
		noCache(c)
		c.File(index)
	})
}
