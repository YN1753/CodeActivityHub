package database

import (
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
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL&_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Session{}, &models.UserConfig{},
		&models.PlatformStatus{}, &models.Submission{}, &models.IngestEvent{},
		&models.PlatformAccount{}, &models.IngestToken{}, &models.IngestToken{},
		&models.Problem{}, &models.ProblemTag{},
	); err != nil {
		return nil, err
	}
	var count int64
	if err := db.Model(&models.User{}).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		hash, salt, err := HashPassword("admin123")
		if err != nil {
			return nil, err
		}
		if err := db.Create(&models.User{ID: 1, Username: "admin", PasswordHash: hash, Salt: salt, IsAdmin: true}).Error; err != nil {
			return nil, err
		}
	}
	return db, nil
}
