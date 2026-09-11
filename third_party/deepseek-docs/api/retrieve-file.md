---
title: Retrieve File
source: https://api-docs.deepseek.com/api/retrieve-file
fetched: 2026-09-11
---

# Retrieve File

```
GET /files/:file_id
```

Returns information about a specific file.

## Request

**application/json**

### Schema

- `id` (string) **required** — The file identifier, of the form `file-api-...`, which can be referenced in chat completion requests.
- `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
- `bytes` (integer) **required** — The size of the file in bytes.
- `created_at` (integer) **required** — The Unix timestamp (in seconds) of when the file was created.
- `filename` (string) **required** — The name of the file.
- `purpose` (string) **required** — Possible values: [user_data]. The intended purpose of the file.
- `expires_at` (integer) — The Unix timestamp (in seconds) of when the file expires. Only present when an expiration was set at upload time.

## Responses

### 200

OK, returns the `file object`.

**Schema**

- `id` (string) **required** — The file identifier, of the form `file-api-...`, which can be referenced in chat completion requests.
- `object` (string) **required** — Possible values: [file]. The object type, which is always `file`.
- `bytes` (integer) **required** — The size of the file in bytes.
- `created_at` (integer) **required** — The Unix timestamp (in seconds) of when the file was created.
- `filename` (string) **required** — The name of the file.
- `purpose` (string) **required** — Possible values: [user_data]. The intended purpose of the file.
- `expires_at` (integer) — The Unix timestamp (in seconds) of when the file expires. Only present when an expiration was set at upload time.

**Example**

```json
{
  "id": "file-api-0a1b2c3d4e5f60718293a4b5c6d7e8f9",
  "object": "file",
  "bytes": 102400,
  "created_at": 1700000000,
  "filename": "image.jpg",
  "purpose": "user_data"
}
```
