package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // register the pure-Go sqlite driver
)

// MaxTextChars bounds one task: a single row must not be able to grow the
// database and every MCP response without bound. Characters, not bytes: a
// Cyrillic or emoji task must not hit the limit sooner than an ASCII one.
const MaxTextChars = 2000

// ErrEmpty reports that every stored task has already been taken.
var ErrEmpty = errors.New("queue is empty: every task has been taken")

const createTasksTable = `CREATE TABLE IF NOT EXISTS tasks (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	text      TEXT NOT NULL,
	create_at TEXT NOT NULL,
	get_at    TEXT
)`

// The index covers only unclaimed tasks, so taking the oldest one reads its
// first entry instead of scanning the table. AUTOINCREMENT keeps ids monotonic,
// so id order is creation order.
const createTasksIndex = `CREATE INDEX IF NOT EXISTS tasks_unclaimed ON tasks (id) WHERE get_at IS NULL`

// expectedColumns is the exact schema this build reads and writes.
var expectedColumns = []string{"id", "text", "create_at", "get_at"}

// Store owns the SQLite database.
type Store struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
}

// Open opens (and creates when missing) the database at path.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	// The SQLite DSN is a file: URI, which needs an absolute path; a relative
	// XDG_DATA_HOME or "./tasks.db" used to produce "invalid uri authority".
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	path = absolute
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	// WAL keeps readers from blocking the writer; NORMAL is safe for a local
	// single-user database and avoids an fsync on every commit.
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection keeps writes serialised; this tool is single-user.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	// The file is created with the process umask (usually 0644); task contents
	// must not be readable by other local users.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	if err := ensureSchema(context.Background(), db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// ensurePrivateDir creates dir (mode 0700) when missing. An existing directory
// is left as is: its permissions may belong to the user's XDG layout, and
// tightening an arbitrary parent directory is worse than reporting it.
func ensurePrivateDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.Chmod(dir, 0o700) //nolint:gosec // 0700 is deliberate for a private directory
	default:
		return err
	}
}

// ensureSchema creates the table and index on a new database and verifies the
// shape of an existing one. There is no version stamp and no migration path:
// the schema is one CREATE and will not evolve for a local tool.
func ensureSchema(ctx context.Context, db *sql.DB, path string) error {
	exists, err := tableExists(ctx, db)
	if err != nil {
		return err
	}
	if exists {
		if err := checkSchema(ctx, db, path); err != nil {
			return err
		}
	} else if _, err := db.ExecContext(ctx, createTasksTable); err != nil {
		return fmt.Errorf("initialise schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, createTasksIndex); err != nil {
		return fmt.Errorf("initialise schema: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("read schema: %w", err)
	}
	return count > 0, nil
}

// checkSchema refuses a database left behind by an older taskctl. Its rows have
// no get_at, so the queue queries cannot read it, and guessing a translation
// would silently invent which tasks were taken.
func checkSchema(ctx context.Context, db *sql.DB, path string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(tasks)`)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var (
			position     int
			name         string
			kind         string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)
		if err := rows.Scan(&position, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("read schema: %w", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if !slices.Equal(columns, expectedColumns) {
		return fmt.Errorf("database %s has an incompatible schema: columns %v, want %v; "+
			"it was created by an older taskctl and must be removed", path, columns, expectedColumns)
	}
	return nil
}

// Close checkpoints the WAL and releases the database. It is safe to call more
// than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			s.closeErr = fmt.Errorf("checkpoint database: %w", err)
		}
		if err := s.db.Close(); err != nil && s.closeErr == nil {
			s.closeErr = fmt.Errorf("close database: %w", err)
		}
	})
	return s.closeErr
}

// Add stores text as a new task with get_at null.
func (s *Store) Add(ctx context.Context, text string) (Task, error) {
	if err := validateText(text); err != nil {
		return Task{}, err
	}
	createdAt := time.Now().UTC().Truncate(time.Second)

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (text, create_at) VALUES (?, ?)`, text, formatTime(createdAt))
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	return Task{ID: id, Text: text, CreateAt: createdAt}, nil
}

// Take marks the oldest unclaimed task as taken and returns it. The read and
// the write are one statement, so two callers never take the same task.
func (s *Store) Take(ctx context.Context) (Task, error) {
	claimedAt := time.Now().UTC().Truncate(time.Second)

	row := s.db.QueryRowContext(ctx,
		`UPDATE tasks SET get_at = ?
		 WHERE id = (SELECT id FROM tasks WHERE get_at IS NULL ORDER BY id LIMIT 1)
		 RETURNING id, text, create_at, get_at`, formatTime(claimedAt))
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrEmpty
	}
	if err != nil {
		return Task{}, fmt.Errorf("take task: %w", err)
	}
	return task, nil
}

// validateText keeps text as sent, but rejects values that cannot be a task.
func validateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("text must not be empty")
	}
	if !utf8.ValidString(text) {
		return errors.New("text must be valid UTF-8")
	}
	if utf8.RuneCountInString(text) > MaxTextChars {
		return fmt.Errorf("text must be at most %d characters", MaxTextChars)
	}
	return nil
}

// rowScanner covers *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var (
		task      Task
		createdAt string
		takenAt   sql.NullString
	)
	if err := row.Scan(&task.ID, &task.Text, &createdAt, &takenAt); err != nil {
		return Task{}, err
	}
	parsed, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return Task{}, fmt.Errorf("parse create_at %q: %w", createdAt, err)
	}
	task.CreateAt = parsed.UTC()
	if takenAt.Valid {
		taken, err := time.Parse(time.RFC3339, takenAt.String)
		if err != nil {
			return Task{}, fmt.Errorf("parse get_at %q: %w", takenAt.String, err)
		}
		utc := taken.UTC()
		task.GetAt = &utc
	}
	return task, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
