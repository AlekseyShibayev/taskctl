# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-11-15

First tagged release.

### Added

- MCP server over stdio with `add`, `list`, `show` and `update` tools.
- Task lifecycle `create -> in_progress -> done`, with `in_progress -> create`
  as the rework path; tasks are never deleted.
- SQLite storage with WAL, `synchronous=NORMAL`, a versioned schema
  (`PRAGMA user_version`) and private file permissions (`0700`/`0600`).
- `doctor` diagnostics with human-readable and `--json` output.
- Graceful shutdown on SIGINT/SIGTERM, including a WAL checkpoint.
- E2E test that drives the built binary over a real stdio session.

### Fixed

- A parse error no longer stops the server: the request gets `-32700` and the
  stream continues.
- A missing `id` (or `add.name`) now returns "… is required" instead of a
  misleading range error.
- Concurrent updates cannot lose each other: read-modify-write runs in
  `BEGIN IMMEDIATE` and the write is guarded by the read status.
- `jsonrpc` is validated; anything other than `"2.0"` gets `-32600`.
- Incoming messages are limited to 4 MiB; oversized lines are discarded and
  answered with `-32600`.
- `notifications/cancelled` stops a running tool call and a call has a 30 s
  timeout.
- Unknown statuses in the database are counted in `Stats.Unknown` instead of
  leaking into `by_status`.
- `name` and `content` are bounded (1 KiB / 1 MiB), validated as UTF-8 and the
  stored name is trimmed.

[Unreleased]: https://github.com/AlekseyShibayev/taskctl/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/AlekseyShibayev/taskctl/releases/tag/v0.1.0
