// Package operations provides offline maintenance without Telegram, schedulers,
// configuration validation for the Bot, or automatic business migrations.
package operations

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"

	"ccoff-abs-bot/internal/backup"
	"ccoff-abs-bot/internal/buildinfo"
	"ccoff-abs-bot/internal/config"
)

type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Schema  string `json:"schemaCompatibility"`
}

func Version() VersionInfo {
	return VersionInfo{buildinfo.Version, buildinfo.Commit, buildinfo.SchemaCompatibility}
}

func openReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open("sqlite3", u.String()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func CheckCompatibility(db *sql.DB) error {
	var table string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='deployment_metadata'").Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var schema string
	err = db.QueryRow("SELECT value FROM deployment_metadata WHERE key='schema_compatibility'").Scan(&schema)
	if err != nil {
		return fmt.Errorf("invalid deployment metadata: %w", err)
	}
	if schema != buildinfo.SchemaCompatibility {
		return fmt.Errorf("database compatibility %s differs from program %s; do not automatically downgrade or migrate", schema, buildinfo.SchemaCompatibility)
	}
	return nil
}

func StampCompatibility(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS deployment_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		return err
	}
	_, err := db.Exec("INSERT INTO deployment_metadata(key,value) VALUES('schema_compatibility',?) ON CONFLICT(key) DO NOTHING", buildinfo.SchemaCompatibility)
	return err
}

func CheckDatabase(path string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check failed: %s", result)
	}
	return CheckCompatibility(db)
}

// Backup is called with the Bot stopped by the deployment script. VACUUM INTO
// reads WAL correctly rather than copying only the main database file.
func Backup(database, output, key string) error {
	if len(key) < 32 {
		return fmt.Errorf("BACKUP_ENCRYPT_KEY requires at least 32 bytes")
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("backup output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".snapshot-*.db")
	if err != nil {
		return err
	}
	plainPath := tmp.Name()
	tmp.Close()
	os.Remove(plainPath)
	defer os.Remove(plainPath)
	db, err := openReadOnly(database)
	if err != nil {
		return err
	}
	_, err = db.Exec("VACUUM INTO ?", plainPath)
	db.Close()
	if err != nil {
		return err
	}
	if err = CheckDatabase(plainPath); err != nil {
		return err
	}
	plain, err := os.ReadFile(plainPath)
	if err != nil {
		return err
	}
	compressed, err := backup.GzipCompressBytes(plain)
	if err != nil {
		return err
	}
	encrypted, err := backup.EncryptBackupData(compressed, key)
	if err != nil {
		return err
	}
	verify, err := backup.DecryptBackupData(encrypted, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(verify, plain) {
		return fmt.Errorf("backup encryption verification failed")
	}
	return writeNewFile(output, encrypted)
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func Restore(input, database, key string) error {
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	plain, err := backup.DecryptBackupData(data, key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(database), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(database), ".restore-*.db")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(plain)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = CheckDatabase(path); err != nil {
		return err
	}
	// Stage the original DB and sidecars together. If any rename fails, retain
	// all original committed WAL data rather than deleting it before the swap.
	previous, err := os.MkdirTemp(filepath.Dir(database), ".restore-previous-")
	if err != nil {
		return err
	}
	moved := []string{}
	rollback := func(cause error) error {
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(filepath.Join(previous, filepath.Base(moved[i])), moved[i]); err != nil {
				return fmt.Errorf("restore failed (%v); original data retained in %s: %w", cause, previous, err)
			}
		}
		_ = os.Remove(previous)
		return cause
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		original := database + suffix
		info, err := os.Lstat(original)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return rollback(err)
		}
		if !info.Mode().IsRegular() {
			return rollback(fmt.Errorf("restore target must be a regular SQLite file"))
		}
		if err := os.Rename(original, filepath.Join(previous, filepath.Base(original))); err != nil {
			return rollback(err)
		}
		moved = append(moved, original)
	}
	if err := os.Rename(path, database); err != nil {
		return rollback(err)
	}
	return os.RemoveAll(previous)
}

type HealthState struct {
	PID      int       `json:"pid"`
	LastPoll time.Time `json:"lastPoll"`
	Version  string    `json:"version"`
}

func HealthFile() string {
	if p := os.Getenv("BOT_HEALTH_FILE"); p != "" {
		return p
	}
	return "/tmp/ccoff-bot-health.json"
}

func WriteHealth(lastPoll time.Time) error {
	data, err := json.Marshal(HealthState{os.Getpid(), lastPoll, buildinfo.Version})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(HealthFile()), ".bot-health-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), HealthFile())
}

func Health(database string, now time.Time) error {
	data, err := os.ReadFile(HealthFile())
	if err != nil {
		return err
	}
	var state HealthState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.PID <= 0 {
		return fmt.Errorf("invalid Bot PID")
	}
	if err := syscall.Kill(state.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("Bot process is not alive: %w", err)
	}
	if state.LastPoll.IsZero() || state.LastPoll.After(now.Add(time.Minute)) || now.Sub(state.LastPoll) > 120*time.Second {
		return fmt.Errorf("Telegram poll heartbeat stale or missing")
	}
	db, err := openReadOnly(database)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM deployment_metadata WHERE key='schema_compatibility' AND value=?", buildinfo.SchemaCompatibility).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("database compatibility marker missing")
	}
	return nil
}

// RunCLI returns handled=false for the normal Bot entry point.
func RunCLI(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] == "version" {
		if len(args) == 2 && args[1] == "--schema" {
			fmt.Println(buildinfo.SchemaCompatibility)
			return true, nil
		}
		return true, json.NewEncoder(os.Stdout).Encode(Version())
	}
	if args[0] != "maintenance" {
		return true, fmt.Errorf("unknown command")
	}
	_ = godotenv.Load()
	database := os.Getenv("DATABASE_URL")
	if database == "" {
		database = "data/bot_data.db"
	}
	if strings.Contains(database, "?") || strings.HasPrefix(database, "file:") {
		return true, fmt.Errorf("maintenance requires a plain DATABASE_URL file path")
	}
	if len(args) < 2 {
		return true, fmt.Errorf("maintenance health|check-db|backup|restore")
	}
	switch args[1] {
	case "preflight":
		config.LoadConfig()
		if config.AppConfig.IsTest() || config.AppConfig.DbURL != "data/bot_data.db" {
			return true, fmt.Errorf("deployment requires production and DATABASE_URL=data/bot_data.db")
		}
		return true, nil
	case "health":
		return true, Health(database, time.Now())
	case "check-db":
		return true, CheckDatabase(database)
	case "backup":
		if len(args) != 3 {
			return true, fmt.Errorf("maintenance backup OUTPUT.enc")
		}
		return true, Backup(database, args[2], os.Getenv("BACKUP_ENCRYPT_KEY"))
	case "restore":
		if len(args) != 4 || args[3] != "--confirm-data-loss" {
			return true, fmt.Errorf("maintenance restore INPUT.enc --confirm-data-loss (Bot must be stopped)")
		}
		return true, Restore(args[2], database, os.Getenv("BACKUP_ENCRYPT_KEY"))
	default:
		return true, fmt.Errorf("unknown maintenance command")
	}
}
