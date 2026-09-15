package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

	// 可信代理：默认不信任任何代理（反向代理下 X-Forwarded-For 可被伪造）。
	// 通过 CODEACTIVITYHUB_TRUSTED_PROXIES（逗号分隔）显式配置可信来源。
	if proxies := os.Getenv("CODEACTIVITYHUB_TRUSTED_PROXIES"); proxies != "" {
		parts := []string{}
		for _, p := range strings.Split(proxies, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			if err := r.SetTrustedProxies(parts); err != nil {
				log.Printf("警告：可信代理配置无效: %v", err)
			}
		}
	} else {
		if err := r.SetTrustedProxies(nil); err != nil {
			log.Printf("警告：设置默认可信代理失败: %v", err)
		}
	}

	// 前端与后端通常同源部署，此时不需要 CORS；只有在 Vite dev server
	// 或独立域名部署时才需要放行，默认保留常用本地来源，额外来源用
	// CODEACTIVITYHUB_CORS_ORIGINS（逗号分隔）配置。
	corsConfig, corsWarn := buildCORSConfig()
	if corsWarn != "" {
		log.Printf("警告：%s", corsWarn)
	}
	r.Use(cors.New(corsConfig))

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

	// 显式设置超时，避免慢连接/慢请求长期占用连接；并支持优雅退出。
	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP 服务异常退出: %v", err)
			panic(err)
		}
	}()

	// 监听 SIGINT/SIGTERM 做优雅退出，给在途请求 10 秒宽限期收尾。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("收到退出信号，正在优雅关闭服务...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭失败: %v", err)
	}
	log.Println("服务已停止")
}

func buildCORSConfig() (cors.Config, string) {
	origins, hasWildcard := sanitizeOrigins()
	cfg := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "Cache-Control"},
		AllowCredentials: true,
	}
	warn := ""
	if hasWildcard {
		// 白名单含 "*" 风险极大（配合 AllowCredentials 可被任意站点读带凭证响应）。
		// 强制关闭 AllowCredentials，"*" 仍作为通配放行，但不再携带凭证。
		cfg.AllowCredentials = false
		warn = "CORS 白名单含 \"*\"，已强制关闭 AllowCredentials 并通配放行，建议改用明确来源"
	}
	return cfg, warn
}

func sanitizeOrigins() ([]string, bool) {
	origins := []string{"http://127.0.0.1:5173", "http://localhost:5173", "http://127.0.0.1:2053", "http://localhost:2053"}
	hasWildcard := false
	for _, item := range strings.Split(os.Getenv("CODEACTIVITYHUB_CORS_ORIGINS"), ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		if item == "*" {
			hasWildcard = true
		}
		origins = append(origins, item)
	}
	return origins, hasWildcard
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
	// 平台 logo 等固定图片：frontend/public/logos 会被 Vite 原样拷到 dist/logos
	r.Static("/logos", filepath.Join(dist, "logos"))
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
