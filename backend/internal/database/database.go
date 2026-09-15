package database

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"

	"codeactivityhub/backend/internal/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func Open(path string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// _busy_timeout 防止并发写入直接报 SQLITE_BUSY。
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	// 数据库文件里存着用户各平台的登录 Cookie，收紧文件权限。失败只记日志。
	if err := os.Chmod(path, 0600); err != nil {
		log.Printf("警告：无法设置数据库文件权限(%s): %v", path, err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Session{}, &models.UserConfig{},
		&models.PlatformStatus{}, &models.Submission{}, &models.IngestEvent{},
		&models.PlatformAccount{}, &models.IngestToken{},
		&models.Problem{}, &models.ProblemTag{},
	); err != nil {
		return nil, err
	}
	var count int64
	if err := db.Model(&models.User{}).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		// 空库时创建默认管理员：不再硬编码弱密码，随机生成强密码并打印到
		// 启动日志（一次性），用户需尽快登录并修改。
		pw, err := randomPassword(16)
		if err != nil {
			return nil, err
		}
		hash, salt, err := HashPassword(pw)
		if err != nil {
			return nil, err
		}
		if err := db.Create(&models.User{ID: 1, Username: "admin", PasswordHash: hash, Salt: salt, IsAdmin: true}).Error; err != nil {
			return nil, err
		}
		log.Printf("已创建默认管理员账号 admin，初始随机密码：%s （请尽快登录修改）", pw)
	}
	return db, nil
}

// randomPassword 生成指定字节长度的随机十六进制强密码。
func randomPassword(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
