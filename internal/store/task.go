package store

import "time"

// Status is the lifecycle state of a task.
type Status string

// Task statuses in lifecycle order.
const (
	StatusCreate     Status = "create"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
)

// allStatuses lists every valid status in lifecycle order. It stays private so
// no package can mutate the canonical list.
var allStatuses = []Status{
	StatusCreate,
	StatusInProgress,
	StatusDone,
}

// Statuses returns a copy of the valid statuses in lifecycle order.
func Statuses() []Status {
	out := make([]Status, len(allStatuses))
	copy(out, allStatuses)
	return out
}

// statuses is the internal read-only view used inside this package.
func statuses() []Status { return allStatuses }

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, known := range allStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// CanTransitionTo reports whether moving from s to next is allowed.
// Repeating the current status is allowed and changes nothing.
func (s Status) CanTransitionTo(next Status) bool {
	if s == next {
		return true
	}
	switch s {
	case StatusCreate:
		return next == StatusInProgress
	case StatusInProgress:
		return next == StatusCreate || next == StatusDone
	default:
		return false
	}
}

// ParseStatus converts a wire value into a Status.
func ParseStatus(raw string) (Status, error) {
	s := Status(raw)
	if !s.Valid() {
		return "", invalidArgumentf("unknown status %q: expected one of %v", raw, Statuses())
	}
	return s, nil
}

// Task is a single row of the tasks table.
type Task struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}
