package store

import "time"

// Task is a single row of the tasks table. GetAt stays nil while the task is
// unclaimed, so "not taken yet" is exactly get_at IS NULL.
type Task struct {
	ID       int64      `json:"id"`
	Text     string     `json:"text"`
	CreateAt time.Time  `json:"create_at"`
	GetAt    *time.Time `json:"get_at"`
}
