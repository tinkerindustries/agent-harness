---
title: Get User Balance
source: https://api-docs.deepseek.com/api/get-user-balance
fetched: 2026-08-13
---

# Get User Balance

```
GET /user/balance
```

Get user current balance

## Responses

### 200

OK, returns user balance info.

**Schema**

- `is_available` (boolean) — Whether the user's balance is sufficient for API calls.
- `balance_infos` (object[])
  - `currency` (string) — Possible values: [CNY, USD]. The currency of the balance.
  - `total_balance` (string) — The total available balance, including the granted balance and the topped-up balance.
  - `granted_balance` (string) — The total not expired granted balance.
  - `topped_up_balance` (string) — The total topped-up balance.

**Example**

```json
{
  "is_available": true,
  "balance_infos": [
    {
      "currency": "CNY",
      "total_balance": "110.00",
      "granted_balance": "10.00",
      "topped_up_balance": "100.00"
    }
  ]
}
```
