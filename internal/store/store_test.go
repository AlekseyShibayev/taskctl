package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return open(t, filepath.Join(t.TempDir(), "tasks.db"))
}

func mustCreate(t *testing.T, st *store.Store, text string) {
	t.Helper()
	if err := st.Create(context.Background(), text); err != nil {
		t.Fatalf("create %q: %v", text, err)
	}
}

func mustGet(t *testing.T, st *store.Store) string {
	t.Helper()
	text, err := st.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return text
}

// Text is stored byte for byte: no trimming, no normalisation.
func TestCreateKeepsTextAsSent(t *testing.T) {
	st := newStore(t)
	const text = "  задача 🚀  "
	mustCreate(t, st, text)
	if got := mustGet(t, st); got != text {
		t.Fatalf("text = %q, want %q", got, text)
	}
}

func TestGetEmpty(t *testing.T) {
	st := newStore(t)
	if _, err := st.Get(context.Background()); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("get on empty queue: %v", err)
	}
}

func TestGetOldestFirst(t *testing.T) {
	st := newStore(t)
	for i := 1; i <= 3; i++ {
		mustCreate(t, st, fmt.Sprintf("task %d", i))
		// goqite orders by created with millisecond precision.
		time.Sleep(5 * time.Millisecond)
	}
	for i := 1; i <= 3; i++ {
		if got, want := mustGet(t, st), fmt.Sprintf("task %d", i); got != want {
			t.Fatalf("get %d = %q, want %q", i, got, want)
		}
	}
}

func TestNoDeduplication(t *testing.T) {
	st := newStore(t)
	mustCreate(t, st, "same")
	mustCreate(t, st, "same")
	mustGet(t, st)
	mustGet(t, st)
	if _, err := st.Get(context.Background()); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("two identical messages produced more than two gets: %v", err)
	}
}

// A taken task never comes back, even after a restart.
func TestGetSkipsTaken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first := open(t, path)
	mustCreate(t, first, "taken")
	if got := mustGet(t, first); got != "taken" {
		t.Fatalf("get = %q", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, path)
	mustCreate(t, second, "new")
	if got := mustGet(t, second); got != "new" {
		t.Fatalf("get after restart = %q, want new", got)
	}
	if _, err := second.Get(context.Background()); !errors.Is(err, store.ErrEmpty) {
		t.Fatalf("taken task came back: %v", err)
	}
}

// Two stores on one file claim concurrently: every task is taken exactly once.
func TestGetIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first := open(t, path)
	second := open(t, path)

	const total = 20
	for i := 0; i < total; i++ {
		mustCreate(t, first, fmt.Sprintf("task %d", i))
	}

	var (
		wg    sync.WaitGroup
		empty atomic.Int32
	)
	results := make(chan string, total*2)
	errs := make(chan error, total*2)
	for i := 0; i < total*2; i++ {
		st := first
		if i%2 == 1 {
			st = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, err := st.Get(context.Background())
			switch {
			case errors.Is(err, store.ErrEmpty):
				empty.Add(1)
			case err != nil:
				errs <- err
			default:
				results <- text
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("get: %v", err)
	}

	seen := make(map[string]bool, total)
	for text := range results {
		if seen[text] {
			t.Fatalf("task %q taken twice", text)
		}
		seen[text] = true
	}
	if len(seen) != total {
		t.Fatalf("taken %d tasks, want %d", len(seen), total)
	}
	if got := empty.Load(); got != total {
		t.Fatalf("empty answers = %d, want %d", got, total)
	}
}

func TestRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	st := open(t, "tasks.db")
	mustCreate(t, st, "task")
	if got := mustGet(t, st); got != "task" {
		t.Fatalf("get = %q", got)
	}
}

// An existing directory and database file must be tightened on open.
func TestOpenTightensPermissions(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "taskctl")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	path := filepath.Join(dataDir, "tasks.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create database file: %v", err)
	}

	st := open(t, path)
	mustCreate(t, st, "task")

	for name, want := range map[string]os.FileMode{
		dataDir:       0o700,
		path:          0o600,
		path + "-wal": 0o600,
		path + "-shm": 0o600,
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(name), err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(name), got, want)
		}
	}
}

// Concurrent first opens must serialise on the directory lock.
func TestConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")

	const openers = 8
	var wg sync.WaitGroup
	errs := make(chan error, openers)
	stores := make([]*store.Store, openers)
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := store.Open(path)
			if err != nil {
				errs <- err
				return
			}
			stores[i] = st
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("open: %v", err)
	}
	for _, st := range stores {
		if st != nil {
			_ = st.Close()
		}
	}
}
