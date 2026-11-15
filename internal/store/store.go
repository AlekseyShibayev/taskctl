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
	"time"

	_ "modernc.org/sqlite"
)

// Pagination bounds for List.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	content    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'create',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_status_id ON tasks (status, id);
`

// Store owns the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (and creates when missing) the database at path.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
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
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One transaction for the whole schema: a failure must not leave the table
	// without its index.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	if _, err := tx.Exec(schema); err != nil {
		tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Add creates a task in status create.
func (s *Store) Add(ctx context.Context, name string, content *string) (Task, error) {
	if strings.TrimSpace(name) == "" {
		return Task{}, invalidArgumentf("name must not be empty")
	}
	body := ""
	if content != nil {
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

// Get returns one task by id.
func (s *Store) Get(ctx context.Context, id int64) (Task, error) {
	if id <= 0 {
		return Task{}, invalidArgumentf("id must be a positive integer")
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
		return Task{}, notFoundf("task %d not found", id)
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
	HasMore bool   `json:"has_more"`
}

// List returns tasks ordered by id with pagination.
func (s *Store) List(ctx context.Context, filter ListFilter) (Page, error) {
	limit := filter.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return Page{}, invalidArgumentf("limit must be between 1 and %d", MaxLimit)
	}
	if filter.Offset < 0 {
		return Page{}, invalidArgumentf("offset must not be negative")
	}
	if filter.Status != nil && !filter.Status.Valid() {
		return Page{}, invalidArgumentf("unknown status %q", *filter.Status)
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
	defer rows.Close()

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

	hasMore := len(tasks) > limit
	if hasMore {
		tasks = tasks[:limit]
	}
	return Page{Tasks: tasks, Limit: limit, Offset: filter.Offset, HasMore: hasMore}, nil
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
		return Task{}, invalidArgumentf("id must be a positive integer")
	}
	if patch.Name == nil && patch.Content == nil && patch.Status == nil {
		return Task{}, invalidArgumentf("at least one of name, content or status is required")
	}
	if patch.Name != nil && strings.TrimSpace(*patch.Name) == "" {
		return Task{}, invalidArgumentf("name must not be empty")
	}
	if patch.Status != nil && !patch.Status.Valid() {
		return Task{}, invalidArgumentf("unknown status %q", *patch.Status)
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
				return invalidTransitionf("cannot change status from %s to %s", current.Status, *patch.Status)
			}
			sets = append(sets, "status = ?")
			args = append(args, string(*patch.Status))
		}
		// Only changed columns are written; the status guard pins the write to
		// the version that was read. Inside this transaction it cannot fire — it
		// is the safety net for a writer that bypasses withWriteTx.
		args = append(args, id, string(current.Status))
		result, err := conn.ExecContext(ctx,
			`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ? AND status = ?`, args...)
		if err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		if affected == 0 {
			return notFoundf("task %d not found", id)
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
	defer conn.Close()

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

// Stats is a status histogram used by diagnostics.
type Stats struct {
	Total    int            `json:"total"`
	ByStatus map[Status]int `json:"by_status"`
}

// Stats counts tasks by status.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return Stats{}, fmt.Errorf("count tasks: %w", err)
	}
	defer rows.Close()

	stats := Stats{ByStatus: make(map[Status]int, len(Statuses))}
	for _, status := range Statuses {
		stats.ByStatus[status] = 0
	}
	for rows.Next() {
		var status Status
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return Stats{}, fmt.Errorf("count tasks: %w", err)
		}
		stats.ByStatus[status] = count
		stats.Total += count
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("count tasks: %w", err)
	}
	return stats, nil
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
