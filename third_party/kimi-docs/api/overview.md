---
title: API Overview
source: https://platform.kimi.ai/docs/api/overview
fetched: 2026-08-13
---

# API Overview

## Service Address

```
https://api.moonshot.ai
```

Kimi Open Platform provides OpenAI-compatible HTTP APIs. You can use the OpenAI SDK directly.

When using SDKs, set `base_url` to `https://api.moonshot.ai/v1`. When calling HTTP endpoints directly, use the full path such as `https://api.moonshot.ai/v1/chat/completions`.

## OpenAI Compatibility

Our API is compatible with the OpenAI Chat Completions API in request/response format. This means:

* You can use the official OpenAI SDKs (Python / Node.js) directly
* Most OpenAI-compatible third-party tools and frameworks (LangChain, Dify, Coze, etc.) are supported
* Simply point `base_url` to `https://api.moonshot.ai/v1` to switch

<Note>
  Some parameters are Kimi-specific extensions: the `thinking` parameter needs to be passed via the SDK's `extra_body`; `partial` is a field on assistant messages within the messages array (`"partial": true`), not a top-level request parameter. See [Tool Use](tool-use.md) and [Partial Mode](partial.md) for details.
</Note>

## Authentication

All API requests require an API Key in the HTTP header:

```
Authorization: Bearer $MOONSHOT_API_KEY
```

API Keys can be created and managed in the [Kimi Open Platform Console](https://platform.kimi.ai/console/api-keys).

<Warning>
  Your API Key is sensitive. Do not expose it in client-side code, public repositories, or logs. Use environment variables to manage it.
</Warning>

## SDK Installation

<CodeGroup>
  ```bash Python theme={null}
  pip install --upgrade 'openai>=1.0'
  ```

  ```bash Node.js theme={null}
  npm install openai
  ```
</CodeGroup>

Initialize the client:

<CodeGroup>
  ```python Python theme={null}
  import os

  from openai import OpenAI

  client = OpenAI(
      api_key=os.environ["MOONSHOT_API_KEY"],
      base_url="https://api.moonshot.ai/v1",
  )
  ```

  ```javascript Node.js theme={null}
  const OpenAI = require("openai");

  const client = new OpenAI({
      apiKey: process.env.MOONSHOT_API_KEY,
      baseURL: "https://api.moonshot.ai/v1",
  });
  ```
</CodeGroup>

<Note>
  Python version ≥ 3.7.1, Node.js version ≥ 18, OpenAI SDK version ≥ 1.0.0.

  ```bash theme={null}
  python -c 'import openai; print("version =", openai.__version__)'
  ```
</Note>

## Common Request Headers

| Header          | Value                      | Description          |
| --------------- | -------------------------- | -------------------- |
| `Content-Type`  | `application/json`         | Request body format  |
| `Authorization` | `Bearer $MOONSHOT_API_KEY` | Authentication token |

## Error Handling

When a request fails, the API returns a JSON error response containing `error.type` and `error.message` fields. Common HTTP status codes include 400 (bad request), 401 (authentication failure), 429 (rate limit), 500 (server error), etc.

For the full list of error types, messages, and troubleshooting tips, see [Errors](errors.md).

## API Endpoints

| Endpoint                              | Method | Description                            |
| ------------------------------------- | ------ | -------------------------------------- |
| `/v1/chat/completions`                | POST   | [Create Chat Completion](chat.md)    |
| `/v1/models`                          | GET    | [List Models](list-models.md)        |
| `/v1/tokenizers/estimate-token-count` | POST   | [Estimate Tokens](estimate.md)       |
| `/v1/users/me/balance`                | GET    | [Check Balance](balance.md)          |
| `/v1/files`                           | POST   | [Upload File](files-upload.md)       |
| `/v1/files`                           | GET    | [List Files](files-list.md)          |
| `/v1/files/{file_id}`                 | GET    | [Get File Info](files-retrieve.md)   |
| `/v1/files/{file_id}`                 | DELETE | [Delete File](files-delete.md)       |
| `/v1/files/{file_id}/content`         | GET    | [Get File Content](files-content.md) |

## Next Steps

<CardGroup cols={2}>
  <Card title="Quickstart" icon="rocket" href="/docs/api/quickstart">
    Send your first API request
  </Card>

  <Card title="Models Overview" icon="cubes" href="/docs/api/models-overview">
    Compare model capabilities and parameters
  </Card>

  <Card title="Tool Use" icon="wrench" href="/docs/api/tool-use">
    Enable function calling
  </Card>

  <Card title="Create Chat Completion" icon="code" href="/docs/api/chat">
    Full endpoint parameter reference
  </Card>
</CardGroup>
