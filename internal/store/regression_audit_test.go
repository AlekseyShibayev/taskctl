package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// C1: a slice page is bounded by row count, not by bytes, so 500 tasks of
// 1 MiB can produce a ~524 MB response. The page must carry at most 2 MiB of
// task data and report hasNext so the client can continue.
func TestAuditC1ListPageByteBudget(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const (
		tasks  = 30
		size   = 100 << 10
		budget = 2 << 20
	)
	body := strings.Repeat("x", size)
	for i := 0; i < tasks; i++ {
		if _, err := st.Add(ctx, "task", &body); err != nil {
			t.Fatalf("add task %d: %v", i, err)
		}
	}

	seen := make(map[int64]bool, tasks)
	offset := 0
	for page := 0; page < tasks+5 && len(seen) < tasks; page++ {
		result, err := st.List(ctx, store.ListFilter{Limit: store.MaxLimit, Offset: offset})
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		payload := 0
		for _, task := range result.Tasks {
			payload += len(task.Name) + len(task.Content)
			seen[task.ID] = true
		}
		if payload > budget {
			t.Fatalf("page %d carries %d bytes of task data, budget is %d (tasks on page: %d)",
				page, payload, budget, len(result.Tasks))
		}
		if len(result.Tasks) > result.Limit {
			t.Fatalf("page %d returns %d tasks but reports limit %d", page, len(result.Tasks), result.Limit)
		}
		if !result.HasNext {
			break
		}
		if len(result.Tasks) == 0 {
			t.Fatalf("page %d is empty but hasNext is true", page)
		}
		offset += len(result.Tasks)
	}
	if len(seen) != tasks {
		t.Fatalf("pagination saw %d of %d tasks", len(seen), tasks)
	}
}
