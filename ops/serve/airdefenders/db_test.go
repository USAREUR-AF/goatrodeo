package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpenDBMigratesLegacySchema reproduces the nova-a crash-loop fixed in this
// change. A database created by a earlier build has only the original
// five columns (no team/members/mode). openDB must migrate it in place without
// error. Before the fix, the bootstrap schema's `CREATE INDEX ... ON
// scores(mode)` ran before migrateScores added the column and failed with
// "no such column: mode", so the server crash-looped on startup against any
// pre-mode database.
func TestOpenDBMigratesLegacySchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scores.db")

	// Seed a legacy (pre-mode) database exactly as an older build left it.
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(`CREATE TABLE scores (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL,
		score      INTEGER NOT NULL,
		wave       INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO scores (name, score, wave, created_at) VALUES ('LEGACY', 1234, 5, 1)`,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	seed.Close()

	// The production openDB must migrate the legacy DB in place without crashing.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB on legacy schema: %v", err)
	}
	defer db.Close()

	// The mode column now exists and the legacy row defaulted to 'sp'.
	var mode string
	if err := db.QueryRow(`SELECT mode FROM scores WHERE name = 'LEGACY'`).Scan(&mode); err != nil {
		t.Fatalf("select mode after migrate: %v", err)
	}
	if mode != "sp" {
		t.Errorf("legacy row mode = %q, want \"sp\"", mode)
	}

	// idx_scores_mode exists exactly once (created by migrateScores, not the
	// bootstrap DDL, and not duplicated).
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_scores_mode'`,
	).Scan(&n); err != nil {
		t.Fatalf("count idx_scores_mode: %v", err)
	}
	if n != 1 {
		t.Errorf("idx_scores_mode count = %d, want 1", n)
	}

	// Re-opening an already-migrated DB must be a no-op (idempotent).
	db.Close()
	db2, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB second time (idempotent re-run): %v", err)
	}
	db2.Close()
}

// TestOpenDBFreshSchema guards the other direction: a brand-new database (no
// pre-existing scores table) opens clean, with the mode column and its index
// present after the bootstrap + migrate sequence.
func TestOpenDBFreshSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB fresh: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(
		`INSERT INTO scores (name, score, wave, created_at, mode) VALUES ('FRESH', 10, 1, 1, 'mp')`,
	); err != nil {
		t.Fatalf("insert with mode column: %v", err)
	}
	var got string
	if err := db.QueryRow(`SELECT mode FROM scores WHERE name = 'FRESH'`).Scan(&got); err != nil {
		t.Fatalf("select mode: %v", err)
	}
	if got != "mp" {
		t.Errorf("mode = %q, want \"mp\"", got)
	}
}
