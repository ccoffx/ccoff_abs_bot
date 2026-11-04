package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func TestOnlyDuplicateKeysAreUniqueConstraintErrors(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE constraint_probe (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, points INTEGER CHECK(points >= 0));
INSERT INTO constraint_probe VALUES (1, 'reader', 1);
CREATE TRIGGER reject_probe BEFORE INSERT ON constraint_probe WHEN NEW.name = 'trigger' BEGIN SELECT RAISE(ABORT, 'unique business rule rejected'); END;`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		duplicate   bool
	}{
		{"unique key", "INSERT INTO constraint_probe VALUES (2, 'reader', 1)", true},
		{"primary key", "INSERT INTO constraint_probe VALUES (1, 'other', 1)", true},
		{"required value", "INSERT INTO constraint_probe VALUES (2, NULL, 1)", false},
		{"check constraint", "INSERT INTO constraint_probe VALUES (2, 'other', -1)", false},
		{"business trigger", "INSERT INTO constraint_probe VALUES (2, 'trigger', 1)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.query)
			if err == nil {
				t.Fatal("invalid write unexpectedly succeeded")
			}
			if got := IsUniqueConstraintError(fmt.Errorf("persist record: %w", err)); got != tc.duplicate {
				t.Fatalf("duplicate=%t, want %t: %v", got, tc.duplicate, err)
			}
		})
	}
	if IsUniqueConstraintError(nil) || IsUniqueConstraintError(errors.New("duplicate delivery refused")) {
		t.Fatal("non-database errors must not be treated as successful deduplication")
	}
	if !IsUniqueConstraintError(fmt.Errorf("persist record: %w", gorm.ErrDuplicatedKey)) {
		t.Fatal("GORM duplicate-key error was not recognized")
	}
}
