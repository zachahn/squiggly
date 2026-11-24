package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600

	busyTimeoutMillis = 5000
)

var migrations = []string{
	`CREATE TABLE sessions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id     TEXT NOT NULL UNIQUE,
  system_prompt TEXT NOT NULL,
  version       INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  writable   INTEGER NOT NULL CHECK (writable IN (0, 1)),
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, name)
);

CREATE TABLE requests (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
  tool        TEXT NOT NULL,
  arguments   TEXT NOT NULL,
  status      TEXT NOT NULL,
  error       TEXT,
  platform    TEXT NOT NULL,
  model       TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  deadline_at INTEGER NOT NULL,
  finished_at INTEGER
);

CREATE TABLE file_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  request_id INTEGER REFERENCES requests(id) ON DELETE CASCADE,
  content    TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  role              TEXT NOT NULL,
  content           TEXT NOT NULL,
  reasoning         TEXT NOT NULL,
  tool_calls        TEXT,
  tool_call_id      TEXT,
  tool_name         TEXT,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  total_tokens      INTEGER,
  created_at        INTEGER NOT NULL
);

CREATE INDEX requests_session_id ON requests(session_id);
CREATE INDEX requests_started_at ON requests(started_at);
CREATE INDEX file_versions_file_id ON file_versions(file_id);
CREATE INDEX file_versions_request_id ON file_versions(request_id);
CREATE INDEX messages_request_id ON messages(request_id);`,
}

func DBPath() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "squiggly", "sessions.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "squiggly", "sessions.db"), nil
}

func DBExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check database: %w", err)
	}
	return true, nil
}

func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := ensureFile(path); err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	query.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: query.Encode()}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func ensureDir(dir string) error {
	_, err := os.Stat(dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check state directory: %w", err)
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("set state directory mode: %w", err)
	}
	return nil
}

func ensureFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, FileMode)
	if err != nil {
		return fmt.Errorf("create database file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(FileMode); err != nil {
		return fmt.Errorf("set database file mode: %w", err)
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this program supports (%d)", current, len(migrations))
	}
	for version := current + 1; version <= len(migrations); version++ {
		if err := applyMigration(ctx, db, version, migrations[version-1]); err != nil {
			return fmt.Errorf("migrate database to version %d: %w", version, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, version int, statements string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, statements); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}
