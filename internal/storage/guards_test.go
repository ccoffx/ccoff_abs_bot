package storage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "github.com/patrickmn/go-cache"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
)

func TestRateLimitAdmitsOneConcurrentRequest(t *testing.T) {
	previous := GoCacheInstance
	GoCacheInstance = cache.New(time.Minute, 0)
	t.Cleanup(func() { GoCacheInstance = previous })
	start := make(chan struct{})
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			<-start
			if CheckRateLimit(1, "refresh", time.Minute) {
				admitted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent requests admitted=%d, want 1", admitted.Load())
	}
	GoCacheInstance.Delete("1_refresh")
	if !CheckRateLimit(1, "refresh", time.Minute) {
		t.Fatal("expired cooldown did not allow the next request")
	}
}

func TestAuthenticationRequiresPersistentLockState(t *testing.T) {
	previousDB, previousConfig := DB, config.AppConfig
	t.Cleanup(func() { DB, config.AppConfig = previousDB, previousConfig })
	config.AppConfig = &config.Config{SecurityPepper: "persistent-lock-regression-test"}
	stored := security.HashSensitiveToken("correct")
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, unavailable := range []*gorm.DB{nil, db} {
		DB = unavailable
		if ok, _ := VerifyUserSecurityCodeWithCooldown(1, "correct", stored); ok {
			t.Fatal("credential passed without a readable lock table")
		}
		if locked, _ := AbsPasswordLockedMessage(1); !locked {
			t.Fatal("ABS password verification allowed without readable lock state")
		}
	}
	if err := db.AutoMigrate(&model.SecurityAttemptLock{}); err != nil {
		t.Fatal(err)
	}
	DB = db
	until := time.Now().Add(time.Minute)
	if err := db.Create(&model.SecurityAttemptLock{UserID: 1, Purpose: securityCodeAttemptPurpose, FailCount: 5, LockedUntil: &until}).Error; err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyUserSecurityCodeWithCooldown(1, "correct", stored); ok {
		t.Fatal("correct credential bypassed active lock")
	}
	if err := db.Model(&model.SecurityAttemptLock{}).Where("user_id = ?", 1).Update("locked_until", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if ok, message := VerifyUserSecurityCodeWithCooldown(1, "correct", stored); !ok {
		t.Fatalf("credential remained blocked after lock expiry: %s", message)
	}
}
