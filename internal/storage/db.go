package storage

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cache "github.com/patrickmn/go-cache"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/textfmt"
)

const RedPacketClaimScopeWorldBossParticipant = "world_boss_participant"

var DB *gorm.DB

var GoCacheInstance *cache.Cache

func EnsureSQLiteDatabaseDir(dsn string) {
	dir := SqliteDatabaseDir(dsn)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		log.Fatalf("❌ 创建数据库目录失败: dir=%s err=%s", textfmt.FormatPlainValue(dir), textfmt.FormatPlainError(err))
	}
}

func SqliteDatabaseDir(dsn string) string {
	dbPath := strings.TrimSpace(dsn)
	if dbPath == "" || dbPath == ":memory:" {
		return ""
	}

	if idx := strings.Index(dbPath, "?"); idx >= 0 {
		dbPath = dbPath[:idx]
	}
	dbPath = strings.TrimPrefix(dbPath, "file:")
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "mode=memory") {
		return ""
	}

	dir := filepath.Dir(dbPath)
	if dir == "." || dir == "" {
		return ""
	}
	return dir
}

func CheckRateLimit(userID int64, action string, cooldown time.Duration) bool {
	key := strconv.FormatInt(userID, 10) + "_" + action

	if lastReqVal, found := GoCacheInstance.Get(key); found {
		if time.Since(lastReqVal.(time.Time)) < cooldown {
			return false
		}
	}

	GoCacheInstance.Set(key, time.Now(), cooldown)
	return true
}
