# SPEC — taskctl: очередь create/get

| Поле | Значение |
|---|---|
| Статус | accepted |
| Версия | 3 |
| Дата | 2026-11-17 |
| Код | `internal/store`, `internal/mcp`, `cmd/taskctl` |

## 1. Что это и зачем

`taskctl` — локальная очередь задач для агента: один бинарник, MCP-сервер поверх
stdio, SQLite в домашнем каталоге, без сети. ОС — Linux, один пользователь.

Цель: задача, положенная один раз, выдаётся не более одного раза — при
параллельных вызовах, при нескольких процессах и после перезапуска.

## 2. Границы

**In scope:** команда `taskctl mcp`; инструменты `create(text)` и `get()`;
путь и права файла БД; отказ открывать базу чужой схемы; тексты ошибок; кадры
JSON-RPC и рукопожатие MCP.

**Out of scope:** сеть и удалённый доступ; статусы и переходы задач; lease,
таймауты, возврат задачи в очередь; удаление и редактирование; список и
пагинация; миграции схемы; macOS и Windows.

**Ограничения:** Go, один бинарник без cgo; `modernc.org/sqlite` — единственное
хранилище; только stdio, сеть не открывается.

## 3. Поведение

### `create(text)`

1. `text` обязателен: отсутствие аргумента — E1.
2. `text` проверяется в порядке E2 → E4 → E3; при ошибке запись не создаётся.
3. `text` хранится как передан, без TrimSpace и нормализации.
4. Ответ — объект задачи: `id`, `text`, `create_at` (UTC), `get_at: null`.
5. `id` монотонно растёт, порядок `id` = порядок создания; идентификатор не
   переиспользуется.
6. `create` не меняет `get_at` существующих задач. Дедупликации нет.

### `get()`

1. Аргументов нет; любое лишнее поле — E6.
2. Берётся задача с минимальным `id` среди `get_at is null`.
3. В той же атомарной операции ей проставляется `get_at` (UTC) и она
   возвращается целиком: `id`, `text`, `create_at`, `get_at`.
4. Свободных задач нет — ошибка E5 (не пустая строка и не `null`).
5. Порядок выдачи = порядок создания. Задача не удаляется и не редактируется.

### Протокол

- Транспорт — stdio, один JSON-RPC 2.0 объект на строку; версия MCP — `2025-06-18`.
- `tools/list` и `tools/call` до `initialize` — ошибка `-32002`.
- `tools/call` без `id` (notification) не исполняется: побочный эффект, о котором
  нельзя отчитаться, запрещён.
- Строка длиннее 4 MiB — `-32600`, поток продолжает работу.
- Один вызов ограничен 30 с; переполнение очереди вызовов — `-32603`
  `server busy: too many pending tool calls`.

## 4. Инварианты и гарантии

| ID | Утверждение |
|---|---|
| INV-1 | Взятая задача не выдаётся повторно: ни тем же процессом, ни другим, ни после перезапуска. |
| INV-2 | N параллельных `get` на M свободных задачах дают min(N, M) разных `id`; остальные получают E5. |
| INV-3 | `create` не меняет `get_at`; `get` не меняет `text` и `create_at`. |
| INV-4 | Пустая очередь — всегда ошибка E5; текст задачи не может быть принят за сигнал пустоты. |
| INV-5 | `create` вернул `id` ⇒ строка закоммичена и видна следующему `get`, в том числе другому процессу. |
| INV-6 | Задачи не исчезают и не переписываются: после `get` строка остаётся с заполненным `get_at`. |
| INV-7 | Все времена — UTC, RFC3339, точность до секунды. |
| INV-8 | База со схемой, отличной от `[id, text, create_at, get_at]`, не открывается; миграции нет. |

## 5. Данные и ошибки

**Хранение.** `$XDG_DATA_HOME/taskctl/tasks.db`, иначе
`~/.local/share/taskctl/tasks.db`; создаётся при первой сессии. Каталог — `0700`,
файл — `0600`. SQLite в режиме WAL, `busy_timeout` 5000 мс, одно соединение на
процесс. Задача — строка `[id, text, create_at, get_at]`; `get_at` равен `null`,
пока задачу не взяли.

**Ответ `create` и `get`** — JSON в `content[0].text`:

```json
{"id": 7, "text": "починить сборку", "create_at": "2026-11-17T09:41:12Z", "get_at": null}
```

**Ошибки.** Бизнес-ошибки инструмента — `isError: true`, текст в
`content[0].text`; JSON-RPC error для них не используется.

| ID | Когда | Канал | Текст |
|---|---|---|---|
| E1 | `create` без `text` | tool | `text is required` |
| E2 | `text` пуст или только пробелы | tool | `text must not be empty` |
| E3 | `text` длиннее 2000 символов (не байт) | tool | `text must be at most 2000 characters` |
| E4 | `text` — не валидный UTF-8 | tool | `text must be valid UTF-8` |
| E5 | `get`: свободных задач нет | tool | `queue is empty: every task has been taken` |
| E6 | лишнее поле, неизвестный инструмент | JSON-RPC `-32602` | `invalid arguments: <причина>` · `unknown tool "<имя>"` |
| E7 | база от старой версии | stderr, код 1 | `database <путь> has an incompatible schema: columns [...], want [...]` |

## 6. Критерии приёмки

| ID | Проверка | Тест |
|---|---|---|
| ACC-1 | `create` → `get`: тот же `id` и `text`, `get_at` заполнен, UTC до секунды | `TestCreateAndGet` |
| ACC-2 | Второй `get` на единственной задаче → E5 | `TestGetTakesEachTaskOnce` |
| ACC-3 | 40 `get` из двух `Store` на 20 задачах → 20 разных `id`, остальные E5 | `TestTakeIsAtomic` |
| ACC-4 | Выдача идёт по возрастанию `id` | `TestTakeOldestUnclaimed` |
| ACC-5 | После перезапуска взятая задача не выдаётся | `TestTakeSkipsClaimed` |
| ACC-6 | Пустой и пробельный `text` — ошибка без записи; 2000 символов — успех, 2001 — ошибка | `TestAddValidation` |
| ACC-7 | 2000 эмодзи принимаются (счёт по символам, не байтам) | `TestTextLimitCountsCharacters` |
| ACC-8 | `text` возвращается байт в байт | `TestAddKeepsTextAsSent` |
| ACC-9 | База со старыми колонками → E7 | `TestIncompatibleSchemaRejected` |
| ACC-10 | Каталог `0700`, файл `0600`, режим WAL | `TestDatabasePermissionsAndDirectory`, `TestWALMode` |
| ACC-11 | `tools/list` и `tools/call` до `initialize` → `-32002` | `TestInitializeRequired` |
| ACC-12 | `tools/call` без `id` не меняет очередь | `TestToolsCallWithoutIDHasNoEffect` |
| ACC-13 | Строка > 4 MiB → `-32600`, сервер продолжает работу | `TestOversizedMessageRejected` |
| ACC-14 | `taskctl mcp` обслуживает `create`/`get` через stdio | `TestStdioSmoke` |

## 7. Открытые вопросы

Очистка старых взятых задач — не решена, вне версии 3. Владелец: автор модуля.

## 8. Ссылки

Код: `internal/store/store.go`, `internal/mcp/server.go`, `internal/mcp/tools.go`,
`cmd/taskctl/main.go`.
Тесты: `internal/store/store_test.go`, `internal/mcp/server_test.go`,
`cmd/taskctl/main_test.go`, `cmd/taskctl/e2e_test.go`.
Установка и запуск — `README.md`.
