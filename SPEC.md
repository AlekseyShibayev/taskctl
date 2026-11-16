# taskctl — спецификация

## MCP-инструменты

```yaml
tools:
  create:
    description: Кладёт задачу в очередь, ничего не возвращает.
    input:
      text: { type: string, minLength: 1, maxLength: 2000, pattern: '\S' }
    errors:
      - { id: C1, when: "пустая строка или одни пробелы", message: "text must not be empty" }
      - { id: C2, when: "длиннее 2000 символов", message: "text must be at most 2000 characters" }

  get:
    description: "Забирает самую старую свободную задачу (ту, у которой get_at не заполнен), проставляет ей get_at и возвращает её текст. Если свободных задач нет, возвращает текст no tasks."
    output: { type: string }
    ordering: [create_at asc, id asc]
    empty: "no tasks"
```

## Схема БД

```yaml
tables:
  tasks:
    columns:
      id: { type: integer, primary_key: true, autoincrement: true }
      text: { type: text, not_null: true }
      create_at: { type: text, not_null: true, format: "ISO 8601, UTC" }
      get_at: { type: text, nullable: true, format: "ISO 8601, UTC", default: null }
    indexes:
      tasks_free:
        columns: [create_at, id]
        where: "get_at IS NULL"
```
