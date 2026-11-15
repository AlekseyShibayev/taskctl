# taskctl

<img src="assets/taskctl.svg" alt="taskctl" width="800" valign="middle">

Локальный трекер задач: MCP поверх stdio, база — SQLite на рабочей машине, без сети.

Трекер — рабочая память агента: задачи заводятся и ведутся из сессии MCP, человек только формулирует, что нужно.

## Ключевые решения

- **MCP вместо CLI и файлов.** Схемы аргументов и коды ошибок приходят при подключении, разбирать stdout не нужно; CLI остался для установки и диагностики.
- **SQLite вместо сервера.** Один пользователь, одна машина, одна таблица `tasks`, никаких фоновых процессов.
- **Отсутствие удаления.** Задачи не удаляются: запись остаётся в базе.
- **Работа только через MCP.** Задачи ведутся вызовами `add`/`list`/`show`/`update`; CLI-команд для этого нет.

## Контракт

```yaml
tasks:                    
  id:         int, AUTOINCREMENT
  name:       str, название задачи
  content:    str, описание задачи
  status:     enum create | in_progress | done
  created_at: datetime, UTC ISO-8601
```

## MCP-инструменты

```yaml
tools:                    # по одному на вызов; * = необязательный
  add:    {args: [name, content*]}
  list:   {args: [status*, limit*, offset*], note: сортировка по id}
  show:   {args: [id]}
  update: {args: [id, name*, content*, status*]}
```

Детали: `list` — `limit` 50 (1..500), `offset`, `has_more`; `update` меняет `status` только по разрешённым переходам (`create → in_progress → done`, назад — `in_progress → create`); ошибка — `{code, message}`; база — `$XDG_DATA_HOME/taskctl/tasks.db`.

## Запуск

```sh
git clone https://github.com/AlekseyShibayev/taskctl.git && cd taskctl && make install
```

Подключение к MCP-клиенту: имя `taskctl`, команда `taskctl`, аргументы `mcp`, транспорт stdio. `make test` проверяет переходы и форматы ответов. Целевая система — Linux; секреты в задачах не размещаются.
