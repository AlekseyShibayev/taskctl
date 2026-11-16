package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"maragu.dev/goqite"
	_ "modernc.org/sqlite" // register the pure-Go sqlite driver
)

// ErrEmpty reports that every queued task has already been taken.
var ErrEmpty = errors.New("queue is empty: every task has been taken")

// queueName is the goqite queue inside the shared SQLite file.
const queueName = "tasks"

// schema is schema_sqlite.sql from maragu.dev/goqite v0.4.0: the library reads
// and writes the table but never creates it.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS goqite (
		id       text primary key default ('m_' || lower(hex(randomblob(16)))),
		created  text not null default (strftime('%Y-%m-%dT%H:%M:%fZ')),
		updated  text not null default (strftime('%Y-%m-%dT%H:%M:%fZ')),
		queue    text not null,
		body     blob not null,
		timeout  text not null default (strftime('%Y-%m-%dT%H:%M:%fZ')),
		received integer not null default 0,
		priority integer not null default 0
	) strict`,
	`CREATE TRIGGER IF NOT EXISTS goqite_updated_timestamp
	AFTER UPDATE ON goqite
	BEGIN
		UPDATE goqite SET updated = strftime('%Y-%m-%dT%H:%M:%fZ') WHERE id = old.id;
	END`,
	`CREATE INDEX IF NOT EXISTS goqite_queue_priority_created_idx
	ON goqite (queue, priority DESC, created)`,
}

// Store owns the SQLite database and the goqite queue on top of it.
type Store struct {
	db    *sql.DB
	queue *goqite.Queue
}

// Open opens (and creates when missing) the database at path.
func Open(path string) (*Store, error) {
	// The SQLite DSN is a file: URI, which needs an absolute path.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection serialises writes; this tool is single-user.
	db.SetMaxOpenConns(1)

	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialise schema: %w", err)
		}
	}
	// Task text must not be readable by other local users.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	return &Store{
		db:    db,
		queue: goqite.New(goqite.NewOpts{DB: db, Name: queueName}),
	}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Create sends text as one goqite message. It returns no value: the MCP
// method is void.
func (s *Store) Create(ctx context.Context, text string) error {
	if err := s.queue.Send(ctx, goqite.Message{Body: []byte(text)}); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// Get receives the next message and deletes it in the same transaction, so a
// successful call never puts it back into the queue.
func (s *Store) Get(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	message, err := s.queue.ReceiveTx(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	if message == nil {
		return "", ErrEmpty
	}
	if err := s.queue.DeleteTx(ctx, tx, message.ID); err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	return string(message.Body), nil
}
