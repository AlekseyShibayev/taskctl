package store_test

import (
	"context"
	"database/sql"
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
	t.Cleanup(func() { st.Close() })
	return st
}

func mustAdd(t *testing.T, st *store.Store, name string) store.Task {
	t.Helper()
	task, err := st.Add(context.Background(), name, nil)
	if err != nil {
		t.Fatalf("add %q: %v", name, err)
	}
	return task
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestAddDefaultsAndIDs(t *testing.T) {
	st := newStore(t)
	content := "details"
	now := time.Now().UTC()

	first, err := st.Add(context.Background(), "first", &content)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if first.ID != 1 {
		t.Errorf("first id = %d, want 1", first.ID)
	}
	if first.Status != store.StatusCreate {
		t.Errorf("status = %q, want %q", first.Status, store.StatusCreate)
	}
	if first.Content != content {
		t.Errorf("content = %q, want %q", first.Content, content)
	}
	if first.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at location = %s, want UTC", first.CreatedAt.Location())
	}
	if first.CreatedAt.Nanosecond() != 0 {
		t.Errorf("created_at must be truncated to seconds, got %s", first.CreatedAt)
	}
	if delta := now.Sub(first.CreatedAt); delta < -time.Second || delta > 5*time.Second {
		t.Errorf("created_at = %s, now = %s", first.CreatedAt, now)
	}

	second, err := st.Add(context.Background(), "second", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if second.ID != 2 {
		t.Errorf("second id = %d, want 2", second.ID)
	}
	if second.Content != "" {
		t.Errorf("content = %q, want empty", second.Content)
	}
}

func TestAddValidation(t *testing.T) {
	st := newStore(t)
	for _, name := range []string{"", "   ", "\t\n"} {
		_, err := st.Add(context.Background(), name, nil)
		requireError(t, err)
	}

	page, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("invalid adds persisted %d tasks", len(page.Tasks))
	}
}

func TestGet(t *testing.T) {
	st := newStore(t)
	added := mustAdd(t, st, "task")

	got, err := st.Get(context.Background(), added.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != added.ID || got.Name != added.Name || got.Status != added.Status ||
		got.Content != added.Content || !got.CreatedAt.Equal(added.CreatedAt) {
		t.Errorf("get = %+v, want %+v", got, added)
	}

	_, err = st.Get(context.Background(), 99)
	requireError(t, err)

	for _, id := range []int64{0, -1} {
		_, err = st.Get(context.Background(), id)
		requireError(t, err)
	}
}

func TestListOrderAndPagination(t *testing.T) {
	st := newStore(t)
	for i := 1; i <= 5; i++ {
		mustAdd(t, st, "task")
	}

	page, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Limit != store.DefaultLimit || page.Offset != 0 || page.HasNext {
		t.Errorf("default page = %+v", page)
	}
	if len(page.Tasks) != 5 {
		t.Fatalf("tasks = %d, want 5", len(page.Tasks))
	}
	for i, task := range page.Tasks {
		if task.ID != int64(i+1) {
			t.Errorf("tasks[%d].id = %d, want %d", i, task.ID, i+1)
		}
	}

	first, err := st.List(context.Background(), store.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first.Tasks) != 2 || !first.HasNext || first.Tasks[0].ID != 1 || first.Tasks[1].ID != 2 {
		t.Errorf("first page = %+v", first)
	}

	second, err := st.List(context.Background(), store.ListFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(second.Tasks) != 2 || !second.HasNext || second.Tasks[0].ID != 3 {
		t.Errorf("second page = %+v", second)
	}

	last, err := st.List(context.Background(), store.ListFilter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(last.Tasks) != 1 || last.HasNext || last.Tasks[0].ID != 5 {
		t.Errorf("last page = %+v", last)
	}

	empty, err := st.List(context.Background(), store.ListFilter{Offset: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if empty.Tasks == nil || len(empty.Tasks) != 0 || empty.HasNext {
		t.Errorf("empty page = %+v", empty)
	}
}

func TestListFilterAndBounds(t *testing.T) {
	st := newStore(t)
	first := mustAdd(t, st, "first")
	mustAdd(t, st, "second")
	status := store.StatusInProgress
	if _, err := st.Update(context.Background(), first.ID, store.Patch{Status: &status}); err != nil {
		t.Fatalf("update: %v", err)
	}

	page, err := st.List(context.Background(), store.ListFilter{Status: &status})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != first.ID {
		t.Fatalf("filtered page = %+v", page)
	}

	valid := store.Status("invalid")
	bad := []store.ListFilter{
		{Limit: -1},
		{Limit: store.MaxLimit + 1},
		{Offset: -1},
		{Status: &valid},
	}
	for _, filter := range bad {
		_, err := st.List(context.Background(), filter)
		requireError(t, err)
	}

	zero, err := st.List(context.Background(), store.ListFilter{Limit: 0})
	if err != nil {
		t.Fatalf("limit 0 must mean default: %v", err)
	}
	if zero.Limit != store.DefaultLimit {
		t.Errorf("limit = %d, want %d", zero.Limit, store.DefaultLimit)
	}
}

// TestListErrorMessages pins the wording clients see: the allowed range plus
// the offending value for limit, and the full enum for an unknown status.
func TestListErrorMessages(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if _, err := st.List(ctx, store.ListFilter{Limit: store.MaxLimit}); err != nil {
		t.Fatalf("limit at the maximum must be accepted: %v", err)
	}
	_, err := st.List(ctx, store.ListFilter{Limit: store.MaxLimit + 1})
	if want := "limit must be between 1 and 50, got 51"; err == nil || err.Error() != want {
		t.Fatalf("limit 51 error = %v, want %q", err, want)
	}

	invalid := store.Status("nope")
	_, err = st.List(ctx, store.ListFilter{Status: &invalid})
	if want := `unknown status "nope": expected one of [create in_progress done]`; err == nil || err.Error() != want {
		t.Fatalf("unknown status error = %v, want %q", err, want)
	}
}

func TestUpdateFields(t *testing.T) {
	st := newStore(t)
	task := mustAdd(t, st, "old")

	name := "new"
	updated, err := st.Update(context.Background(), task.ID, store.Patch{Name: &name})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != name || updated.Content != task.Content || updated.Status != task.Status {
		t.Errorf("updated = %+v", updated)
	}
	if !updated.CreatedAt.Equal(task.CreatedAt) {
		t.Errorf("created_at changed: %s -> %s", task.CreatedAt, updated.CreatedAt)
	}

	content := "body"
	updated, err = st.Update(context.Background(), task.ID, store.Patch{Content: &content})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Content != content || updated.Name != name {
		t.Errorf("updated = %+v", updated)
	}

	empty := ""
	_, err = st.Update(context.Background(), task.ID, store.Patch{Content: &empty})
	if err != nil {
		t.Fatalf("update content to empty: %v", err)
	}

	blank := "  "
	_, err = st.Update(context.Background(), task.ID, store.Patch{Name: &blank})
	requireError(t, err)

	_, err = st.Update(context.Background(), task.ID, store.Patch{})
	requireError(t, err)

	unknown := int64(99)
	_, err = st.Update(context.Background(), unknown, store.Patch{Name: &name})
	requireError(t, err)

	invalid := store.Status("cancelled")
	_, err = st.Update(context.Background(), task.ID, store.Patch{Status: &invalid})
	requireError(t, err)
}

func TestUpdateTransitions(t *testing.T) {
	all := store.Statuses()

	// moveTo drives a fresh task into the requested status.
	moveTo := func(t *testing.T, st *store.Store, target store.Status) store.Task {
		t.Helper()
		task := mustAdd(t, st, "task")
		path := map[store.Status][]store.Status{
			store.StatusCreate:     nil,
			store.StatusInProgress: {store.StatusInProgress},
			store.StatusDone:       {store.StatusInProgress, store.StatusDone},
		}[target]
		for _, next := range path {
			status := next
			updated, err := st.Update(context.Background(), task.ID, store.Patch{Status: &status})
			if err != nil {
				t.Fatalf("move %d to %s: %v", task.ID, next, err)
			}
			task = updated
		}
		return task
	}

	allowed := map[store.Status][]store.Status{
		store.StatusCreate:     {store.StatusCreate, store.StatusInProgress},
		store.StatusInProgress: {store.StatusInProgress, store.StatusCreate, store.StatusDone},
		store.StatusDone:       {store.StatusDone},
	}

	for _, from := range all {
		for _, to := range all {
			from, to := from, to
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				st := newStore(t)
				task := moveTo(t, st, from)
				if task.Status != from {
					t.Fatalf("driver produced %s, want %s", task.Status, from)
				}
				status := to
				updated, err := st.Update(context.Background(), task.ID, store.Patch{Status: &status})
				if contains(allowed[from], to) {
					if err != nil {
						t.Fatalf("transition %s -> %s: %v", from, to, err)
					}
					if updated.Status != to {
						t.Errorf("status = %s, want %s", updated.Status, to)
					}
					return
				}
				requireError(t, err)
			})
		}
	}
}

// Concurrent updates of different fields must not overwrite each other: the
// read-modify-write lives inside one write transaction, so both changes survive.
func TestUpdateConcurrentFieldsAreNotLost(t *testing.T) {
	st := newStore(t)
	const rounds = 50

	for i := 0; i < rounds; i++ {
		task := mustAdd(t, st, "task")
		inProgress := store.StatusInProgress
		name := fmt.Sprintf("renamed-%d", i)

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := st.Update(context.Background(), task.ID, store.Patch{Status: &inProgress})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := st.Update(context.Background(), task.ID, store.Patch{Name: &name})
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: update: %v", i, err)
			}
		}

		got, err := st.Get(context.Background(), task.ID)
		if err != nil {
			t.Fatalf("round %d: get: %v", i, err)
		}
		if got.Name != name || got.Status != store.StatusInProgress {
			t.Fatalf("round %d: lost update: name = %q, status = %s", i, got.Name, got.Status)
		}
	}
}

// Update must never wait for a connection it is holding itself: the store keeps
// a single connection, so every query inside the write transaction has to use it.
func TestUpdateDoesNotDeadlockOnSingleConnection(t *testing.T) {
	st := newStore(t)
	task := mustAdd(t, st, "task")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name := "renamed"
	updated, err := st.Update(ctx, task.ID, store.Patch{Name: &name})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != name {
		t.Errorf("name = %q, want %q", updated.Name, name)
	}
}

func contains(values []store.Status, want store.Status) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDatabasePermissionsAndDirectory(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "nested", "tasks.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

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
	defer db.Close()

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

func TestNameAndContentLimits(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	exactName := strings.Repeat("n", store.MaxNameBytes)
	if _, err := st.Add(ctx, exactName, nil); err != nil {
		t.Errorf("name at the limit: %v", err)
	}
	tooLong := strings.Repeat("n", store.MaxNameBytes+1)
	_, err := st.Add(ctx, tooLong, nil)
	requireError(t, err)

	exactContent := strings.Repeat("c", store.MaxContentBytes)
	if _, err := st.Add(ctx, "big", &exactContent); err != nil {
		t.Errorf("content at the limit: %v", err)
	}
	tooBig := strings.Repeat("c", store.MaxContentBytes+1)
	_, err = st.Add(ctx, "bigger", &tooBig)
	requireError(t, err)
}

func TestUnicodeAndTrimming(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	task, err := st.Add(ctx, "  задача 🚀  ", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if task.Name != "задача 🚀" {
		t.Errorf("name = %q, want trimmed unicode name", task.Name)
	}

	emoji := strings.Repeat("🚀", 300) // 1200 bytes > 1 KiB
	_, err = st.Add(ctx, emoji, nil)
	requireError(t, err)

	invalid := string([]byte{0xff, 0xfe})
	_, err = st.Add(ctx, invalid, nil)
	requireError(t, err)

	content := invalid
	_, err = st.Add(ctx, "valid", &content)
	requireError(t, err)

	renamed := " updated "
	updated, err := st.Update(ctx, task.ID, store.Patch{Name: &renamed})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "updated" {
		t.Errorf("updated name = %q, want trimmed", updated.Name)
	}
}

func TestUpdateAcrossTwoStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.Close()
	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.Close()

	task := mustAdd(t, first, "task")
	inProgress := store.StatusInProgress
	name := "renamed"

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := first.Update(context.Background(), task.ID, store.Patch{Status: &inProgress})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := second.Update(context.Background(), task.ID, store.Patch{Name: &name})
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("update: %v", err)
		}
	}

	got, err := first.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != name || got.Status != store.StatusInProgress {
		t.Fatalf("lost update across stores: name = %q, status = %s", got.Name, got.Status)
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
	defer st.Close()

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

// A done task is a closed record: no field may change, and only the no-op
// repetition of its status is accepted.
func TestUpdateDoneIsImmutable(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	task := mustAdd(t, st, "closed")
	for _, next := range []store.Status{store.StatusInProgress, store.StatusDone} {
		status := next
		updated, err := st.Update(ctx, task.ID, store.Patch{Status: &status})
		if err != nil {
			t.Fatalf("move to %s: %v", next, err)
		}
		task = updated
	}

	name := "renamed"
	content := "rewritten"
	create := store.StatusCreate
	progress := store.StatusInProgress
	done := store.StatusDone

	cases := []struct {
		name  string
		patch store.Patch
	}{
		{"name", store.Patch{Name: &name}},
		{"content", store.Patch{Content: &content}},
		{"name_content", store.Patch{Name: &name, Content: &content}},
		{"status_create", store.Patch{Status: &create}},
		{"status_in_progress", store.Patch{Status: &progress}},
		{"name_status_create", store.Patch{Name: &name, Status: &create}},
		{"name_status_in_progress", store.Patch{Name: &name, Status: &progress}},
		{"name_status_done", store.Patch{Name: &name, Status: &done}},
		{"content_status_create", store.Patch{Content: &content, Status: &create}},
		{"content_status_in_progress", store.Patch{Content: &content, Status: &progress}},
		{"content_status_done", store.Patch{Content: &content, Status: &done}},
		{"name_content_status_create", store.Patch{Name: &name, Content: &content, Status: &create}},
		{"name_content_status_in_progress", store.Patch{Name: &name, Content: &content, Status: &progress}},
		{"name_content_status_done", store.Patch{Name: &name, Content: &content, Status: &done}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.Update(ctx, task.ID, tc.patch); err == nil {
				t.Fatalf("update combined with %s succeeded on a done task", tc.name)
			}
			current, err := st.Get(ctx, task.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if current.Name != task.Name || current.Content != task.Content || current.Status != done {
				t.Fatalf("done task changed: %+v, want %+v", current, task)
			}
		})
	}

	// Repeating the terminal status is a no-op, not a change.
	updated, err := st.Update(ctx, task.ID, store.Patch{Status: &done})
	if err != nil {
		t.Fatalf("repeat done status: %v", err)
	}
	if updated.Name != task.Name || updated.Content != task.Content || updated.Status != done {
		t.Fatalf("no-op update changed the task: %+v", updated)
	}
}
