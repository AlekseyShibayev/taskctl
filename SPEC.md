# taskctl — спецификация

## Ограничения

- Очередь реализуется на goqite; собственная реализация запрещена.

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
    description: "Забирает самую старую невзятую задачу, помечает её взятой и возвращает её текст. Если свободных задач нет, возвращает текст no tasks."
    output: { type: string }
    empty: "no tasks"
```
