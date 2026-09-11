---
title: Lists Models
source: https://api-docs.deepseek.com/api/list-models
fetched: 2026-09-11
---

# Lists Models

```
GET /models
```

Lists the currently available models, and provides basic information about each one such as the owner and availability. Check [Models & Pricing](../quick_start/pricing.md) for our currently supported models.

## Responses

### 200

OK, returns A list of models

**Schema**

- `object` (string) **required** — Possible values: [list]
- `data` (Model[]) **required**
  - `id` (string) **required** — The model identifier, which can be referenced in the API endpoints.
  - `object` (string) **required** — Possible values: [model]. The object type, which is always "model".
  - `owned_by` (string) **required** — The organization that owns the model.

**Example**

```json
{
  "object": "list",
  "data": [
    {
      "id": "deepseek-flash",
      "object": "model",
      "owned_by": "deepseek"
    },
    {
      "id": "deepseek-v4-pro",
      "object": "model",
      "owned_by": "deepseek"
    }
  ]
}
```
