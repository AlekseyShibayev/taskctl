# AGENTS.md

`taskctl` — Linux-only local task queue: a single Go binary exposing MCP over
stdio (`create`/`get`) on top of SQLite. The contract is `SPEC.md` (v7).

## Commands

- `make build` → `bin/taskctl`; version comes from `git describe` via `-ldflags -X main.version`.
- `make install` → `~/.local/bin/taskctl`. A running MCP client keeps the old binary until it is reconnected.
- `make test` is plain cached `go test ./...`; use `go test -race -count=1 ./...` for real verification (concurrency is the point of INV-2/INV-7).
- Fast loop: `go test -short ./...` — the three e2e tests build a binary and spawn up to 8 processes.
- Single test: `go test ./internal/store -run TestGetIsAtomic -race -count=1`.
- Go 1.25. No CI, linter, formatter or codegen config exists. If `go` is absent, `bin/taskctl` is the only build.

## Contract

- `SPEC.md` is authoritative; ACC-1…19 name the exact test proving each item. A behavior change means code + SPEC + tests together.
- Scope is deliberately `create`/`get` only: no statuses, leases, retries, dedup, list/delete/edit, extra CLI commands or flags.
- Docs are Russian; code, comments and commit subjects are English. Commits are conventional and lowercase (`fix: …`, `docs: …`).

## Wiring

- `internal/store`: goqite over modernc sqlite (pure Go). The schema is hand-copied from goqite's `schema_sqlite.sql` — the library never creates its table.
- `get` is one transaction: `ReceiveTx` → `DeleteTx` → `Commit`. Never split it or put the message back.
- The DSN is a `file:` URI, so paths are made absolute; the pool is capped at one connection.
- The DB's parent directory is chmod 0700 (an existing one included); the DB and `-wal`/`-shm` are 0600, and the file is pre-created to defeat umask. First init is flock-ed on `.taskctl.lock`; busy retries last 5 s.
- `syscall.Flock` makes the code Linux-only; there are no build tags.

## MCP

- stdio, one JSON-RPC object per line. stdout is protocol-only; diagnostics go to stderr (`e2e_test.go` parses every stdout line).
- A message without `id` is a notification: no response and no side effect.
- Bad arguments or unknown tool → JSON-RPC `-32602`. Runtime tool failures (empty queue, missing `text`) → result with `isError: true`. Keep the two distinct.
- `protocolVersion` is `2025-06-18`; `tools/list` returns exactly `create` and `get`; no handshake is enforced.

## Testing

- Store/MCP tests use `t.TempDir()` and `t.Setenv`; e2e passes `XDG_DATA_HOME` explicitly. Never let a test read the real `~/.local/share/taskctl/tasks.db`: it holds live queued tasks, and `get` consumes them.
- `cmd/taskctl/e2e_test.go` runs `go build` inside `cmd/taskctl`, so it needs the toolchain, not just `make build`.

## Build gotcha

- SPEC requires a cgo-free static binary, but `Makefile` never sets `CGO_ENABLED=0` and `bin/taskctl` is dynamically linked. Check with `file bin/taskctl` before claiming the build meets the spec.
