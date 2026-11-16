package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustAdd(t *testing.T, st *store.Store, text string) store.Task {
	t.Helper()
	task, err := st.Add(context.Background(), text)
	if err != nil {
		t.Fatalf("add %q: %v", text, err)
	}
	return task
}

func mustTake(t *testing.T, st *store.Store) store.Task {
	t.Helper()
	task, err := st.Take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	return task
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestAddFields(t *testing.T) {
	st := newStore(t)
	now := time.Now().UTC()

	first := mustAdd(t, st, "first task")
	if first.ID != 1 {
		t.Errorf("first id = %d, want 1", first.ID)
	}
	if first.GetAt != nil {
		t.Errorf("get_at = %v, want null", first.GetAt)
	}
	if first.CreateAt.Location() != time.UTC {
		t.Errorf("create_at location = %s, want UTC", first.CreateAt.Location())
	}
	if first.CreateAt.Nanosecond() != 0 {
		t.Errorf("create_at must be truncated to seconds, got %s", first.CreateAt)
	}
	if delta := now.Sub(first.CreateAt); delta < -time.Second || delta > 5*time.Second {
		t.Errorf("create_at = %s, now = %s", first.CreateAt, now)
	}

	second := mustAdd(t, st, "second task")
	if second.ID != 2 {
		t.Errorf("second id = %d, want 2", second.ID)
	}
	if second.Text != "second task" {
		t.Errorf("text = %q, want it as sent", second.Text)
	}
}

func TestAddKeepsTextAsSent(t *testing.T) {
	st := newStore(t)
	text := "  задача 🚀  "
	task := mustAdd(t, st, text)
	if task.Text != text {
		t.Errorf("text = %q, want %q", task.Text, text)
	}
}

func TestAddValidation(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, text := range []string{"", "   ", "\t\n", string([]byte{0xff, 0xfe})} {
		if _, err := st.Add(ctx, text); err == nil {
			t.Errorf("add %q succeeded", text)
		}
	}
	tooLong := strings.Repeat("x", store.MaxTextChars+1)
	_, err := st.Add(ctx, tooLong)
	requireError(t, err)

	if _, err := st.Take(ctx); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("invalid adds persisted a task: err = %v", err)
	}

	exact := strings.Repeat("x", store.MaxTextChars)
	if _, err := st.Add(ctx, exact); err != nil {
		t.Errorf("text at the limit: %v", err)
	}
}

// The limit counts characters, not bytes: 2000 emoji are 2000 characters and
// 8000 bytes, and must still fit.
func TestTextLimitCountsCharacters(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	emoji := strings.Repeat("🚀", store.MaxTextChars)
	if _, err := st.Add(ctx, emoji); err != nil {
		t.Fatalf("2000 emoji: %v", err)
	}

	_, err := st.Add(ctx, emoji+"🚀")
	if want := "text must be at most 2000 characters"; err == nil || err.Error() != want {
		t.Fatalf("2001 emoji: err = %v, want %q", err, want)
	}
}

func TestTakeOldestUnclaimed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	added := make([]store.Task, 0, 3)
	for i := 1; i <= 3; i++ {
		added = append(added, mustAdd(t, st, fmt.Sprintf("task %d", i)))
	}

	for i, want := range added {
		got := mustTake(t, st)
		if got.ID != want.ID || got.Text != want.Text {
			t.Fatalf("take %d = %+v, want %+v", i, got, want)
		}
		if !got.CreateAt.Equal(want.CreateAt) {
			t.Errorf("take %d: create_at = %s, want %s", i, got.CreateAt, want.CreateAt)
		}
		if got.GetAt == nil {
			t.Fatalf("take %d: get_at is null after taking", i)
		}
		if got.GetAt.Location() != time.UTC || got.GetAt.Nanosecond() != 0 {
			t.Errorf("take %d: get_at = %s, want UTC truncated to seconds", i, got.GetAt)
		}
	}

	if _, err := st.Take(ctx); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("take on an empty queue: err = %v, want ErrEmpty", err)
	}
}

// A taken task must never come back, even when newer tasks keep arriving.
func TestTakeSkipsClaimed(t *testing.T) {
	st := newStore(t)
	first := mustAdd(t, st, "first")
	if got := mustTake(t, st); got.ID != first.ID {
		t.Fatalf("take = %+v, want id %d", got, first.ID)
	}

	second := mustAdd(t, st, "second")
	third := mustAdd(t, st, "third")

	for _, want := range []store.Task{second, third} {
		if got := mustTake(t, st); got.ID != want.ID {
			t.Fatalf("take = %+v, want id %d", got, want.ID)
		}
	}
}

// Two stores on the same file claim concurrently: every task is taken exactly
// once and nothing is lost.
func TestTakeIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer func() { _ = second.Close() }()

	const total = 20
	for i := 0; i < total; i++ {
		mustAdd(t, first, fmt.Sprintf("task %d", i))
	}

	var wg sync.WaitGroup
	results := make(chan store.Task, total*2)
	errs := make(chan error, total*2)
	for i := 0; i < total*2; i++ {
		st := first
		if i%2 == 1 {
			st = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := st.Take(context.Background())
			if errors.Is(err, store.ErrEmpty) {
				return
			}
			if err != nil {
				errs <- err
				return
			}
			results <- task
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("take: %v", err)
	}

	seen := make(map[int64]bool, total)
	for task := range results {
		if seen[task.ID] {
			t.Fatalf("task %d taken twice", task.ID)
		}
		seen[task.ID] = true
	}
	if len(seen) != total {
		t.Fatalf("taken %d tasks, want %d", len(seen), total)
	}
	if _, err := first.Take(context.Background()); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("queue not empty after concurrent takes: %v", err)
	}
}

func TestDatabasePermissionsAndDirectory(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "nested", "tasks.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", mode)
	}
	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode = %04o, want 0600", mode)
	}
}

func TestWALMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal mode = %q, want wal", mode)
	}
	var result string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil || result != "ok" {
		t.Errorf("quick check = %q, %v", result, err)
	}
}

func TestOpenRelativePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Chdir(dir)

	st, err := store.Open("tasks.db")
	if err != nil {
		t.Fatalf("open relative path: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := os.Stat(filepath.Join(dir, "tasks.db")); err != nil {
		t.Errorf("database file: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("current directory mode = %04o, want 0755 (unchanged)", mode)
	}
}

// A database created by the previous taskctl (name/content/status) must be
// reported, not silently read as a queue.
func TestIncompatibleSchemaRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tasks (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL,
		content    TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'create',
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err = store.Open(path)
	if err == nil || !strings.Contains(err.Error(), "incompatible schema") {
		t.Fatalf("open old database: %v", err)
	}
}
