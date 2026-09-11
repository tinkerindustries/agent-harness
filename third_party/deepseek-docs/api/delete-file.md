---
title: Delete File
source: https://api-docs.deepseek.com/api/delete-file
fetched: 2026-09-11
---

# Delete File

```
DELETE /files/:file_id
```

Deletes a file.

## Request

**application/json**

### Schema

- `id` (string) **required** — The ID of the deleted file.
- `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
- `deleted` (boolean) **required** — Whether the file was successfully deleted.

## Responses

### 200

OK, returns the deletion status.

**Schema**

- `id` (string) **required** — The ID of the deleted file.
- `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
- `deleted` (boolean) **required** — Whether the file was successfully deleted.

**Example**

```json
{
  "id": "file-api-0a1b2c3d4e5f60718293a4b5c6d7e8f9",
  "object": "file",
  "deleted": true
}
```
