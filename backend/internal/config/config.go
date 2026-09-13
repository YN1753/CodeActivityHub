package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	HTTPAddr     string
	DatabasePath string
	FrontendDist string
}

func Load() Config {
	root := projectRoot()
	return Config{
		HTTPAddr:     env("CODEACTIVITYHUB_ADDR", ":2053"),
		DatabasePath: env("CODEACTIVITYHUB_DB", filepath.Join(root, "data", "codeactivityhub.db")),
		FrontendDist: env("CODEACTIVITYHUB_FRONTEND_DIST", filepath.Join(root, "frontend", "dist")),
	}
}

// projectRoot makes `go run main.go` work from backend/cmd/server as well as
// starting the compiled binary from the repository root. Explicit environment
// variables always take precedence over this detection.
func projectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "go.mod")) && dirHasFrontend(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return cwd
}

func dirHasFrontend(root string) bool {
	_, err := os.Stat(filepath.Join(root, "frontend"))
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
