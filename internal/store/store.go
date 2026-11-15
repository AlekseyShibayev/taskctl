package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // register the pure-Go sqlite driver
)

// Pagination bounds for List.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// Field limits: one task must not be able to grow the database and every MCP
// response without bound.
const (
	MaxNameBytes    = 1 << 10 // 1 KiB
	MaxContentBytes = 1 << 20 // 1 MiB
)

const createTasksTable = `CREATE TABLE IF NOT EXISTS tasks (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	content    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'create',
	created_at TEXT NOT NULL
)`

const createTasksIndex = `CREATE INDEX IF NOT EXISTS tasks_status_id ON tasks (status, id)`

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
	// Consequence: a transaction holds the only connection, so every query made
	// while it is open must go through that connection (see withWriteTx), never
	// through s.db — otherwise the call blocks until the context expires.
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
	if err := ensureSchema(context.Background(), db); err != nil {
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

// ensureSchema creates the table and index on a new database. There is no
// version stamp and no migration path: the schema is one CREATE and will not
// evolve for a local tool.
func ensureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, createTasksTable); err != nil {
		return fmt.Errorf("initialise schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, createTasksIndex); err != nil {
		return fmt.Errorf("initialise schema: %w", err)
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

// Add creates a task in status create.
func (s *Store) Add(ctx context.Context, name string, content *string) (Task, error) {
	name, err := validateName(name)
	if err != nil {
		return Task{}, err
	}
	body := ""
	if content != nil {
		if err := validateContent(*content); err != nil {
			return Task{}, err
		}
		body = *content
	}
	createdAt := time.Now().UTC().Truncate(time.Second)

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (name, content, status, created_at) VALUES (?, ?, ?, ?)`,
		name, body, string(StatusCreate), formatTime(createdAt))
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	return Task{
		ID:        id,
		Name:      name,
		Content:   body,
		Status:    StatusCreate,
		CreatedAt: createdAt,
	}, nil
}

// validateName trims the name, rejects blank and oversized values.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("name must be valid UTF-8")
	}
	if len(name) > MaxNameBytes {
		return "", fmt.Errorf("name must be at most %d bytes", MaxNameBytes)
	}
	return name, nil
}

// validateContent keeps content as sent, but bounds its size.
func validateContent(content string) error {
	if !utf8.ValidString(content) {
		return fmt.Errorf("content must be valid UTF-8")
	}
	if len(content) > MaxContentBytes {
		return fmt.Errorf("content must be at most %d bytes", MaxContentBytes)
	}
	return nil
}

// Get returns one task by id.
func (s *Store) Get(ctx context.Context, id int64) (Task, error) {
	if id <= 0 {
		return Task{}, fmt.Errorf("id must be a positive integer")
	}
	return getTask(ctx, s.db, id, "read task")
}

// rowQuerier is satisfied by *sql.DB and *sql.Conn.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// getTask reads one task through q; op labels the failure.
func getTask(ctx context.Context, q rowQuerier, id int64, op string) (Task, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, name, content, status, created_at FROM tasks WHERE id = ?`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %d not found", id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("%s: %w", op, err)
	}
	return task, nil
}

// ListFilter narrows a List call. Zero Limit means DefaultLimit.
type ListFilter struct {
	Status *Status
	Limit  int
	Offset int
}

// Page is one page of tasks ordered by id.
type Page struct {
	Tasks   []Task `json:"tasks"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasNext bool   `json:"hasNext"`
}

// List returns tasks ordered by id with pagination.
func (s *Store) List(ctx context.Context, filter ListFilter) (Page, error) {
	limit := filter.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return Page{}, fmt.Errorf("limit must be between 1 and %d", MaxLimit)
	}
	if filter.Offset < 0 {
		return Page{}, fmt.Errorf("offset must not be negative")
	}
	if filter.Status != nil && !filter.Status.Valid() {
		return Page{}, fmt.Errorf("unknown status %q", *filter.Status)
	}

	query := `SELECT id, name, content, status, created_at FROM tasks`
	args := make([]any, 0, 2)
	if filter.Status != nil {
		query += ` WHERE status = ?`
		args = append(args, string(*filter.Status))
	}
	query += ` ORDER BY id LIMIT ? OFFSET ?`
	args = append(args, limit+1, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]Task, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return Page{}, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list tasks: %w", err)
	}

	hasNext := len(tasks) > limit
	if hasNext {
		tasks = tasks[:limit]
	}
	return Page{Tasks: tasks, Limit: limit, Offset: filter.Offset, HasNext: hasNext}, nil
}

// Patch updates the given fields; nil fields stay unchanged.
type Patch struct {
	Name    *string
	Content *string
	Status  *Status
}

// Update applies a patch and returns the stored task.
func (s *Store) Update(ctx context.Context, id int64, patch Patch) (Task, error) {
	if id <= 0 {
		return Task{}, fmt.Errorf("id must be a positive integer")
	}
	if patch.Name == nil && patch.Content == nil && patch.Status == nil {
		return Task{}, fmt.Errorf("at least one of name, content or status is required")
	}
	if patch.Name != nil {
		name, err := validateName(*patch.Name)
		if err != nil {
			return Task{}, err
		}
		patch.Name = &name
	}
	if patch.Content != nil {
		if err := validateContent(*patch.Content); err != nil {
			return Task{}, err
		}
	}
	if patch.Status != nil && !patch.Status.Valid() {
		return Task{}, fmt.Errorf("unknown status %q", *patch.Status)
	}

	var updated Task
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		current, err := getTask(ctx, conn, id, "update task")
		if err != nil {
			return err
		}

		sets := make([]string, 0, 3)
		args := make([]any, 0, 5)
		if patch.Name != nil {
			sets = append(sets, "name = ?")
			args = append(args, *patch.Name)
		}
		if patch.Content != nil {
			sets = append(sets, "content = ?")
			args = append(args, *patch.Content)
		}
		if patch.Status != nil {
			// Checked against the row read in this transaction, not a snapshot.
			if !current.Status.CanTransitionTo(*patch.Status) {
				return fmt.Errorf("cannot change status from %s to %s", current.Status, *patch.Status)
			}
			sets = append(sets, "status = ?")
			args = append(args, string(*patch.Status))
		}
		// Only changed columns are written; the status guard pins the write to
		// the version that was read. Inside this transaction it cannot fire — it
		// is the safety net for a writer that bypasses withWriteTx.
		args = append(args, id, string(current.Status))
		// Column names come from the fixed list above; values stay parameterised.
		result, err := conn.ExecContext(ctx,
			`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ? AND status = ?`, args...) //nolint:gosec // SQL is assembled from constant column names only
		if err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("task %d not found", id)
		}

		// Return the row as stored, not the values computed from the read.
		updated, err = getTask(ctx, conn, id, "update task")
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return updated, nil
}

// withWriteTx runs fn inside BEGIN IMMEDIATE: the write lock is taken before the
// first read, so a read-modify-write cannot interleave with another process and
// no update is lost. fn must reach the database through conn only — the store
// keeps a single connection, so a query through s.db would wait for the
// connection this transaction is holding, forever.
func (s *Store) withWriteTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(conn); err != nil {
		s.rollback(ctx, conn)
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		s.rollback(ctx, conn)
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// rollback undoes the open transaction. When the rollback itself fails, the
// connection still holds the transaction and must not go back to the pool.
func (s *Store) rollback(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// rowScanner covers *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var (
		task      Task
		createdAt string
	)
	if err := row.Scan(&task.ID, &task.Name, &task.Content, &task.Status, &createdAt); err != nil {
		return Task{}, err
	}
	parsed, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return Task{}, fmt.Errorf("parse created_at %q: %w", createdAt, err)
	}
	task.CreatedAt = parsed.UTC()
	return task, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
