---
title: List Files
source: https://api-docs.deepseek.com/api/list-files
fetched: 2026-09-10
---

# List Files

```
GET /files
```

Returns a list of files that belong to the user, with cursor-based pagination.

## Request

**application/json**

### Schema

- `object` (string) **required** — Possible values: [list]. The object type, which is always `list`.
- `data` (object[]) **required** — The list of file objects.
  - `id` (string) **required** — The file identifier, of the form `file-api-...`, which can be referenced in chat completion requests.
  - `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
  - `bytes` (integer) **required** — The size of the file in bytes.
  - `created_at` (integer) **required** — The Unix timestamp (in seconds) of when the file was created.
  - `filename` (string) **required** — The name of the file.
  - `purpose` (string) **required** — Possible values: [user_data]. The intended purpose of the file.
  - `expires_at` (integer) — The Unix timestamp (in seconds) of when the file expires. Only present when an expiration was set at upload time.
- `first_id` (string) — The ID of the first file in the list. Useful as a pagination cursor.
- `last_id` (string) — The ID of the last file in the list. Useful as a pagination cursor.
- `has_more` (boolean) **required** — Whether there are more files beyond this page.

## Responses

### 200

OK, returns a list of `file object`.

**Schema**

- `object` (string) **required** — Possible values: [list]. The object type, which is always `list`.
- `data` (object[]) **required** — The list of file objects.
  - `id` (string) **required** — The file identifier, of the form `file-api-...`, which can be referenced in chat completion requests.
  - `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
  - `bytes` (integer) **required** — The size of the file in bytes.
  - `created_at` (integer) **required** — The Unix timestamp (in seconds) of when the file was created.
  - `filename` (string) **required** — The name of the file.
  - `purpose` (string) **required** — Possible values: [user_data]. The intended purpose of the file.
  - `expires_at` (integer) — The Unix timestamp (in seconds) of when the file expires. Only present when an expiration was set at upload time.
- `first_id` (string) — The ID of the first file in the list. Useful as a pagination cursor.
- `last_id` (string) — The ID of the last file in the list. Useful as a pagination cursor.
- `has_more` (boolean) **required** — Whether there are more files beyond this page.

**Example**

```json
{
  "object": "list",
  "data": [
    {
      "id": "file-api-0a1b2c3d4e5f60718293a4b5c6d7e8f9",
      "object": "file",
      "bytes": 102400,
      "created_at": 1700000000,
      "filename": "image.jpg",
      "purpose": "user_data"
    }
  ],
  "first_id": "file-api-0a1b2c3d4e5f60718293a4b5c6d7e8f9",
  "last_id": "file-api-0a1b2c3d4e5f60718293a4b5c6d7e8f9",
  "has_more": false
}
```
