// listening-repair previews a migrated historical day's legacy-policy deficit.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	path := flag.String("db", "", "existing bot database path (backup before applying)")
	user := flag.Int64("user", 0, "Telegram user ID")
	day := flag.String("day", "", "Beijing date YYYY-MM-DD")
	apply := flag.Bool("apply", false, "apply reviewed repair; default is read-only preview")
	delta := flag.Float64("expected-delta", 0, "exact BaseDeltaHours from preview")
	actor := flag.Int64("actor", 0, "operator Telegram ID")
	reason := flag.String("reason", "", "audit reason")
	flag.Parse()
	if *path == "" || *user == 0 || *day == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*path); err != nil {
		fail(err)
	}
	mode := "ro"
	if *apply {
		mode = "rw"
	}
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=%s&_busy_timeout=5000", *path, mode)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		fail(err)
	}
	pool, err := db.DB()
	if err != nil {
		fail(err)
	}
	defer pool.Close()
	storage.DB = db
	result, err := game.RepairOfficialListeningDay(*user, *day, *apply, *delta, *actor, *reason)
	if err != nil {
		fail(err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(data))
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
