# taskctl

<img src="assets/banner.svg?v=12" alt="taskctl" width="800" valign="middle">

Локальный трекер задач для агента: MCP поверх stdio, SQLite на рабочей машине,
без сети. ОС: Linux.

## Установка

```sh
make install                      # ~/.local/bin/taskctl
```

База — `~/.local/share/taskctl/tasks.db` (или `$XDG_DATA_HOME/taskctl/tasks.db`),
создаётся при первой сессии.

MCP-клиент: `taskctl mcp`, stdio.

## MCP-инструменты

| Инструмент | Аргументы |
|------------|-----------|
| `add`      | `text`    |
| `get`      |           |

## Схема данных

```yaml
tasks:
  id:         int
  text:       str
  add_at:     datetime
  get_at:     datetime
```
