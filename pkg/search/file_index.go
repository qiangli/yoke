package search

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// FileIndexStatus describes one root's last completed filename scan.
type FileIndexStatus struct {
	Root      string    `json:"root"`
	Path      string    `json:"path"`
	Files     int64     `json:"files"`
	Skipped   int64     `json:"skipped"`
	Bytes     int64     `json:"bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

func fileIndexPath(root string) (string, string, error) {
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		home = filepath.Join(userHome, ".bashy")
	}
	key := sha256.Sum256([]byte(root))
	return root, filepath.Join(home, "search", "files", fmt.Sprintf("%x.db", key[:12])), nil
}

func openFileIndex(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// BuildFileIndex rebuilds a root's filename index atomically. It never reads file contents.
func BuildFileIndex(dir string) (FileIndexStatus, error) {
	root, path, err := fileIndexPath(dir)
	if err != nil {
		return FileIndexStatus{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return FileIndexStatus{}, err
	}
	if !info.IsDir() {
		return FileIndexStatus{}, fmt.Errorf("search: %s is not a directory", root)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return FileIndexStatus{}, err
	}
	db, err := openFileIndex(path)
	if err != nil {
		return FileIndexStatus{}, err
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS files (path TEXT PRIMARY KEY, filename TEXT NOT NULL)",
		"CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(filename, content='files', content_rowid='rowid', tokenize='trigram')",
		"CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN INSERT INTO files_fts(rowid, filename) VALUES (new.rowid, new.filename); END",
		"CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN INSERT INTO files_fts(files_fts, rowid, filename) VALUES ('delete', old.rowid, old.filename); END",
		"CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			return FileIndexStatus{}, err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return FileIndexStatus{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM files"); err != nil {
		return FileIndexStatus{}, err
	}
	insert, err := tx.Prepare("INSERT INTO files(path, filename) VALUES (?, ?)")
	if err != nil {
		return FileIndexStatus{}, err
	}
	defer insert.Close()
	var count, skipped int64
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			skipped++
			return nil
		}
		if entry.IsDir() {
			if name != root && skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if name == path || strings.HasPrefix(name, path+"-") {
			return nil
		}
		relPath, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if _, err := insert.Exec(relPath, entry.Name()); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return FileIndexStatus{}, err
	}
	updated := time.Now().UTC()
	for key, value := range map[string]string{
		"root": root, "skipped": fmt.Sprint(skipped), "updated_at": updated.Format(time.RFC3339Nano),
	} {
		if _, err := tx.Exec("INSERT INTO metadata(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return FileIndexStatus{}, err
		}
	}
	if err := insert.Close(); err != nil {
		return FileIndexStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return FileIndexStatus{}, err
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return FileIndexStatus{}, err
	}
	status, err := FileIndexInfo(root)
	if err == nil {
		status.Files = count
	}
	return status, err
}

// FileIndexInfo returns the last completed index's size and age.
func FileIndexInfo(dir string) (FileIndexStatus, error) {
	root, path, err := fileIndexPath(dir)
	if err != nil {
		return FileIndexStatus{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return FileIndexStatus{}, err
	}
	db, err := openFileIndex(path)
	if err != nil {
		return FileIndexStatus{}, err
	}
	defer db.Close()
	status := FileIndexStatus{Root: root, Path: path}
	var updated string
	if err := db.QueryRow("SELECT count(*) FROM files").Scan(&status.Files); err != nil {
		return FileIndexStatus{}, err
	}
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='skipped'").Scan(&status.Skipped); err != nil {
		return FileIndexStatus{}, err
	}
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='updated_at'").Scan(&updated); err != nil {
		return FileIndexStatus{}, err
	}
	status.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return FileIndexStatus{}, err
	}
	for _, sidecar := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + sidecar); err == nil {
			status.Bytes += info.Size()
		}
	}
	return status, nil
}

// queryFileIndex searches one existing index. found is false only when no index exists.
func queryFileIndex(dir, query string, max int) ([]LocalResult, bool, error) {
	_, path, err := fileIndexPath(dir)
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	db, err := openFileIndex(path)
	if err != nil {
		return nil, true, err
	}
	defer db.Close()
	var updated string
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='updated_at'").Scan(&updated); err != nil {
		return nil, true, fmt.Errorf("search: filename index is not ready: %w", err)
	}
	match := buildMatcher(query)
	statement := "SELECT path, filename FROM files ORDER BY path"
	var args []any
	if !hasRegexMeta(query) && len([]rune(query)) >= 3 {
		statement = "SELECT f.path, f.filename FROM files_fts JOIN files f ON f.rowid=files_fts.rowid WHERE files_fts MATCH ? ORDER BY f.path"
		args = append(args, `"`+strings.ReplaceAll(query, `"`, `""`)+`"`)
	}
	rows, err := db.Query(statement, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	var out []LocalResult
	for rows.Next() {
		var relPath, filename string
		if err := rows.Scan(&relPath, &filename); err != nil {
			return nil, true, err
		}
		if match(filename) {
			out = append(out, LocalResult{Kind: "file", Path: relPath})
			if len(out) >= max {
				break
			}
		}
	}
	return out, true, rows.Err()
}
