# taskctl

<img src="assets/banner.svg?v=11" alt="taskctl" width="800" valign="middle">

Локальный трекер задач для агента: MCP поверх stdio, SQLite на рабочей машине,
без сети. Только Linux (amd64, arm64).

## Установка

```sh
make install                      # ~/.local/bin/taskctl
```

База — `~/.local/share/taskctl/tasks.db` (или `$XDG_DATA_HOME/taskctl/tasks.db`),
создаётся при первой сессии.

В MCP-клиенте: команда `taskctl`, аргумент `mcp`, транспорт stdio.

## MCP-инструменты

| Инструмент | Аргументы |
| --- | --- |
| `create` | `name`, `content` |
| `slice` | `status*`, `limit*`, `offset*` |
| `get` | `id` |
| `update` | `id`, `status` |

Переходы статусов: `create → in_progress → done`, назад `in_progress → create`
(на доработку). `done` — терминальный. Задачи не удаляются.

## Схема данных

```yaml
tasks:
  id:         int
  name:       str
  content:    str
  status:     enum create | in_progress | done
  created_at: datetime
```
