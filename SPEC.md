# SPEC — taskctl: очередь create/get

| Поле | Значение |
|---|---|
| Статус | accepted |
| Версия | 2 |
| Дата | 2026-11-16 |
| Код | `internal/store`, `internal/mcp`, `cmd/taskctl` |

## 1. Контекст и цель

MCP-сервер по stdio, задачи в локальной SQLite, без сети. Одна команда —
`taskctl mcp`. Цель: задача, положенная один раз, выдаётся не более одного раза
при параллельных вызовах и перезапусках.

## 2. Границы

**In scope:** `create(text)`, `get()`, схема `tasks`, путь к БД, отказ от чужой схемы.

**Out of scope:** сеть; статусы и переходы; lease, таймауты, возврат задачи в
очередь; удаление и редактирование; список и пагинация; миграции; macOS/Windows.

## 3. Поведение

`create(text)` — `text` обязателен, непустой после TrimSpace, ≤ 1 048 576 байт,
валидный UTF-8; хранится и возвращается как передан. Возвращает задачу с
`get_at: null`. `id` монотонно растёт.

`get()` — без аргументов; лишние аргументы отклоняются. Берёт задачу с
минимальным `id` среди `get_at IS NULL`, проставляет `get_at = now` (UTC) и
возвращает её. Порядок выдачи = порядок создания. Выбор и запись — один
`UPDATE ... RETURNING`; отдельного `SELECT` нет.

## 4. Инварианты и гарантии

| ID | Утверждение |
|----|-------------|
| INV-1 | Взятая задача не выдаётся повторно: ни этим процессом, ни другим, ни после перезапуска. |
| INV-2 | N параллельных `get` на M свободных задачах дают min(N, M) разных `id`; остальные получают ошибку пустой очереди. |
| INV-3 | `create` не меняет `get_at`; `get` не меняет `text` и `create_at`. |
| INV-4 | `get` на пустой очереди — ошибка (`isError: true`), не `null`. |
| INV-5 | `create` вернул `id` — строка закоммичена и видна следующему `get`. |
| INV-6 | База со схемой, отличной от `[id, text, create_at, get_at]`, не открывается; миграции нет. |

## 5. Данные и ошибки

```sql
CREATE TABLE IF NOT EXISTS tasks (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  text      TEXT NOT NULL,
  create_at TEXT NOT NULL,
  get_at    TEXT
);
CREATE INDEX IF NOT EXISTS tasks_unclaimed ON tasks (id) WHERE get_at IS NULL;
```

WAL, `busy_timeout(5000)`, одно соединение на процесс. БД:
`$XDG_DATA_HOME/taskctl/tasks.db`, иначе `~/.local/share/taskctl/tasks.db`;
каталог `0700`, файл `0600`.

Ответ — JSON в `content[0].text`; время RFC3339, UTC, до секунды:
`{"id": 1, "text": "...", "create_at": "2026-11-16T12:00:00Z", "get_at": null}`.

Ошибки инструмента: `isError: true`, текст в `content[0].text`; JSON-RPC error
для бизнес-ошибок не используется. Тексты:
`text is required` · `text must not be empty` · `text must be at most 1048576 bytes`
· `text must be valid UTF-8` · `queue is empty: every task has been taken` ·
`invalid arguments: <причина>` · `... has an incompatible schema: columns [...], want [...]`
(с указанием файла БД и требованием удалить её).

## 6. Критерии приёмки

| ID | Проверка | Тест |
|----|----------|------|
| ACC-1 | `create` → `get`: тот же `id` и `text`, `get_at` заполнен, UTC до секунды | `TestCreateAndGet` |
| ACC-2 | Второй `get` на единственной задаче → ошибка пустой очереди | `TestGetTakesEachTaskOnce` |
| ACC-3 | 40 `get` из двух `Store` на 20 задачах → 20 разных `id`, остальные `ErrEmpty` | `TestTakeIsAtomic` |
| ACC-4 | `get` на пустой БД → `isError: true` | `TestTakeOldestUnclaimed` |
| ACC-5 | Пустой и пробельный `text` — ошибка без записи; ровно 1 MiB — успех | `TestAddValidation` |
| ACC-6 | База со старыми колонками → `incompatible schema` | `TestIncompatibleSchemaRejected` |
| ACC-7 | Взятая задача не выдаётся, пока есть более новые свободные | `TestTakeSkipsClaimed` |

## 7. Открытые вопросы

Очистка старых взятых задач — не решена, вне версии 2. Владелец: автор модуля.

## 8. Ссылки

`internal/store/store.go` · `internal/mcp/tools.go` · `internal/store/store_test.go` ·
`docs/spec-guide.md`

## Отклонено

- goqite — перевыдача после таймаута; `MaxReceive: 1` копит мёртвые строки.
- River — in-process воркеры, 25 попыток по умолчанию.
- Статусы, `name`/`content`, `slice`/`update` — удалены из модели.
