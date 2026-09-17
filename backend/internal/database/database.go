package database

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

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
		&models.PlatformAccount{},
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
	fixSubmissionIndexes(db)
	return db, nil
}

// submissionIndexWant 描述"期望的索引列组合"。
// 早期版本把这两个索引误挂成单列（idx_submission_user_date 只在 date 上、
// idx_submission_user_submitted 只在 submitted_at 上），而热力图/近 7 天/错题本
// 的查询都是 user_id+date / user_id+submitted_at，单列索引等于全表扫描。
type submissionIndexWant struct {
	name string
	cols []string
}

// fixSubmissionIndexes 幂等修正上述两个索引：列组合正确就不动；不对就
// DROP 后按正确列重建。只动索引、不动数据，SQLite 上瞬时完成。
// 注意：GORM 的 AutoMigrate 按"索引名已存在"跳过重建，所以这里不能省。
func fixSubmissionIndexes(db *gorm.DB) {
	wants := []submissionIndexWant{
		{name: "idx_submission_user_date", cols: []string{"user_id", "date"}},
		{name: "idx_submission_user_submitted", cols: []string{"user_id", "submitted_at"}},
	}
	for _, w := range wants {
		// PRAGMA 不能参数化，索引名是代码里写死的常量，无注入风险
		rows, err := db.Raw("PRAGMA index_info(" + w.name + ")").Rows()
		if err != nil {
			log.Printf("检查索引 %s 失败: %v", w.name, err)
			continue
		}
		var cols []string
		for rows.Next() {
			var seq int
			var idxName, colName string
			if err := rows.Scan(&seq, &idxName, &colName); err == nil {
				cols = append(cols, colName)
			}
		}
		rows.Close()

		mismatch := len(cols) != len(w.cols)
		if !mismatch {
			for i, c := range w.cols {
				if cols[i] != c {
					mismatch = true
					break
				}
			}
		}
		if !mismatch {
			continue
		}
		log.Printf("修正索引 %s：当前列 %v -> %v（DROP 后重建）", w.name, cols, w.cols)
		if err := db.Exec("DROP INDEX IF EXISTS " + w.name).Error; err != nil {
			log.Printf("修正索引 %s 失败(DROP): %v", w.name, err)
			continue
		}
		quoted := make([]string, 0, len(w.cols))
		for _, c := range w.cols {
			quoted = append(quoted, "\""+c+"\"")
		}
		create := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON submissions (%s)", w.name, strings.Join(quoted, ", "))
		if err := db.Exec(create).Error; err != nil {
			log.Printf("修正索引 %s 失败(CREATE): %v", w.name, err)
		}
	}
}

// randomPassword 生成指定字节长度的随机十六进制强密码。
func randomPassword(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
