package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"maragu.dev/goqite"
	"modernc.org/sqlite" // registers the pure-Go sqlite driver
)

// ErrEmpty reports that every queued task has already been taken.
var ErrEmpty = errors.New("queue is empty: every task has been taken")

const (
	// queueName is the goqite queue inside the shared SQLite file.
	queueName = "tasks"

	dataDirMode  = 0o700
	dataFileMode = 0o600

	// lockFileName serialises first-time initialisation between processes.
	lockFileName = ".taskctl.lock"

	// busyRetryTimeout bounds retries while another process holds the database.
	busyRetryTimeout = 5 * time.Second
)

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
	dir := filepath.Dir(path)
	if err := prepareDir(dir); err != nil {
		return nil, err
	}

	// Several processes may start at once on a fresh data directory. The
	// lock makes schema creation and the WAL switch a single-writer step, so
	// a second process never sees SQLITE_BUSY from the initialisation itself.
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Create the file ourselves: SQLite would otherwise create it with the
	// process umask (typically 0644) before the mode can be restricted.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, dataFileMode)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}

	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection serialises writes; this tool is single-user.
	db.SetMaxOpenConns(1)

	if err := initDatabase(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Task text must not be readable by other local users, and SQLite
	// creates -wal/-shm next to the database.
	if err := restrictDatabaseFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db:    db,
		queue: goqite.New(goqite.NewOpts{DB: db, Name: queueName}),
	}, nil
}

// prepareDir creates the data directory and tightens an existing one.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// MkdirAll leaves an existing directory untouched; the spec promises 0700.
	if err := os.Chmod(dir, dataDirMode); err != nil {
		return fmt.Errorf("restrict data directory: %w", err)
	}
	return nil
}

// lockDir takes an exclusive advisory lock on dir until the returned function
// is called. The lock file stays behind: removing it would race with the next
// opener.
func lockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, dataFileMode)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// initDatabase creates the schema and switches the file to WAL. Both steps
// retry while another connection holds a lock, because busy_timeout on a
// connection does not cover a concurrent journal-mode change.
func initDatabase(db *sql.DB) error {
	return retryBusy(func() error {
		for _, statement := range schema {
			if _, err := db.Exec(statement); err != nil {
				return fmt.Errorf("initialise schema: %w", err)
			}
		}
		// WAL is a persistent property of the file: switching it once is
		// enough, and doing it on every connection open would race with a
		// process that is creating the database for the first time.
		var mode string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			return fmt.Errorf("read journal mode: %w", err)
		}
		if strings.EqualFold(mode, "wal") {
			return nil
		}
		if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
			return fmt.Errorf("enable WAL: %w", err)
		}
		return nil
	})
}

// retryBusy retries fn while SQLite reports a transient lock.
func retryBusy(fn func() error) error {
	deadline := time.Now().Add(busyRetryTimeout)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if !isBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy reports SQLITE_BUSY (5) and SQLITE_LOCKED (6).
func isBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code() == 5 || sqliteErr.Code() == 6)
}

// restrictDatabaseFiles chmods the database and its WAL companions. Missing
// companions are fine: SQLite creates them on demand.
func restrictDatabaseFiles(path string) error {
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(name, dataFileMode); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("restrict database file %s: %w", filepath.Base(name), err)
		}
	}
	return nil
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
