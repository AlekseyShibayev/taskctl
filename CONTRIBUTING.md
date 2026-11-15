# Contributing

Thanks for taking a look at taskctl. The project is small on purpose; keep
changes focused and explain the reason in the commit message.

## Requirements

- Go 1.25 or newer (see `go.mod`);
- Linux (the target platform);
- `golangci-lint` v2.6 for the full check.

## Before a pull request

```sh
make check
```

`make check` runs `gofmt`, `go vet`, golangci-lint, the race-enabled test suite
and the coverage gate (70% minimum). A release build is verified with:

```sh
make build && ./bin/taskctl doctor
```

## Commits

- Conventional Commits: `fix(mcp): ...`, `feat(store): ...`, `docs: ...`.
- One logical change per commit; tests belong with the change.
- Do not mix formatting-only churn with behaviour changes.

## Code notes

- `cmd/taskctl` is the CLI shell; `internal/mcp` owns the protocol and the tool
  schemas; `internal/store` owns SQLite; `internal/logging` configures stderr.
- stdout belongs to MCP. All diagnostics and logs go to stderr.
- Schema changes are a new entry in `migrations`, never an edit of an applied
  one. Bump `SupportedSchemaVersion` through that list.
- Tasks are never deleted: do not add a delete path without discussing it first.

## Pull requests

Fill in the template, describe the user-visible effect, and keep the diff
reviewable. A self-review before pushing is expected.
