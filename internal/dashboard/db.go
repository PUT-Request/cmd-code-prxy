package dashboard

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required
)

// retention is how long usage events are kept in the database.
const retention = 30 * 24 * time.Hour

// openUsageDB opens (creating if needed) the SQLite database at path
// and migrates it. The schema stores fingerprints only, never raw
// keys, so a leaked database cannot be used to authenticate.
func openUsageDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage_events (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		at     INTEGER NOT NULL,
		key_fp TEXT    NOT NULL,
		model  TEXT    NOT NULL,
		input  INTEGER NOT NULL,
		output INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_at ON usage_events(at)`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// pruneUsage deletes events older than the retention window.
func pruneUsage(db *sql.DB, cutoff time.Time) error {
	_, err := db.Exec(`DELETE FROM usage_events WHERE at < ?`, cutoff.Unix())
	return err
}

// loadUsage reads events newer than cutoff in chronological order.
func loadUsage(db *sql.DB, cutoff time.Time) ([]Event, error) {
	rows, err := db.Query(`SELECT at, key_fp, model, input, output
		FROM usage_events WHERE at >= ? ORDER BY at ASC`, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Key, &e.Model, &e.Input, &e.Output); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ensureDir creates a directory (and parents) if it does not exist.
func ensureDir(dir string) error {
	const mode = 0o755
	info, err := os.Stat(dir)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("usage db path %q is not a directory", dir)
	}
	return os.MkdirAll(dir, mode)
}

// openDB is a hook so tests can substitute a failing open.
var openDB = openUsageDB

// initUsageDB opens the database, prunes old rows and returns the
// retained events. It returns a nil db (with the error logged) when
// storage is unavailable: usage tracking must never stop the proxy.
func initUsageDB(path string) (*sql.DB, []Event) {
	if path == "" {
		return nil, nil
	}
	db, err := openDB(path)
	if err != nil {
		log.Printf("[WARN] Usage database unavailable (%v); usage stats stay in memory", err)
		return nil, nil
	}
	if err := pruneUsage(db, time.Now().Add(-retention)); err != nil {
		log.Printf("[WARN] Could not prune old usage rows: %v", err)
	}
	events, err := loadUsage(db, time.Now().Add(-retention))
	if err != nil {
		log.Printf("[WARN] Could not load usage history: %v", err)
		events = nil
	}
	return db, events
}
