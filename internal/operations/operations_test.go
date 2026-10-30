package operations

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE balances(id INTEGER PRIMARY KEY, points INTEGER); INSERT INTO balances VALUES(1,100)"); err != nil {
		t.Fatal(err)
	}
	if err := StampCompatibility(db); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestEncryptedWALBackupAndRestore(t *testing.T) {
	path, db := createDB(t)
	key := strings.Repeat("k", 64)
	archive := filepath.Join(t.TempDir(), "snapshot.enc")
	// Keep a live WAL connection so a naive copy would miss committed writes.
	if _, err := db.Exec("UPDATE balances SET points=456 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := Backup(path, archive, key); err != nil {
		t.Fatal(err)
	}
	if err := Backup(path, archive, key); err == nil {
		t.Fatal("overwrote backup")
	}
	db.Close()
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := Restore(archive, restored, key); err != nil {
		t.Fatal(err)
	}
	check, err := openReadOnly(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var points int
	if err := check.QueryRow("SELECT points FROM balances WHERE id=1").Scan(&points); err != nil || points != 456 {
		t.Fatalf("lost WAL writes: %d %v", points, err)
	}
	if err := Restore(archive, restored, "wrong key"); err == nil {
		t.Fatal("wrong key accepted")
	}
	data, _ := os.ReadFile(archive)
	data[len(data)-1] ^= 1
	corrupt := filepath.Join(t.TempDir(), "bad.enc")
	os.WriteFile(corrupt, data, 0600)
	if err := Restore(corrupt, restored, key); err == nil {
		t.Fatal("corrupted backup accepted")
	}
	if err := CheckDatabase(restored); err != nil {
		t.Fatal("failed restore damaged existing database", err)
	}
}

func TestCompatibilityRefusesFutureDatabase(t *testing.T) {
	path, db := createDB(t)
	if _, err := db.Exec("UPDATE deployment_metadata SET value='999'"); err != nil {
		t.Fatal(err)
	}
	if err := CheckCompatibility(db); err == nil {
		t.Fatal("future schema accepted")
	}
	if err := CheckDatabase(path); err == nil {
		t.Fatal("incompatible restore accepted")
	}
}

func TestHealthRequiresFreshPollLiveProcessAndDatabase(t *testing.T) {
	path, _ := createDB(t)
	t.Setenv("BOT_HEALTH_FILE", filepath.Join(t.TempDir(), "health.json"))
	now := time.Now()
	for _, when := range []time.Time{{}, now.Add(-3 * time.Minute), now.Add(5 * time.Minute)} {
		if err := WriteHealth(when); err != nil {
			t.Fatal(err)
		}
		if err := Health(path, now); err == nil {
			t.Fatal("invalid heartbeat accepted")
		}
	}
	if err := WriteHealth(now); err != nil {
		t.Fatal(err)
	}
	if err := Health(path, now); err != nil {
		t.Fatal(err)
	}
	if err := Health(filepath.Join(t.TempDir(), "missing.db"), now); err == nil {
		t.Fatal("missing DB healthy")
	}
}

func TestDatabaseWithoutMarkerMustBeEmpty(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "unmarked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := CheckCompatibility(db); err != nil {
		t.Fatalf("fresh database rejected: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE users(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := CheckCompatibility(db); err == nil {
		t.Fatal("existing unmarked database accepted")
	}
}
