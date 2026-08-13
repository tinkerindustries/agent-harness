---
title: Reasoning Effort
source: https://platform.kimi.ai/docs/guide/use-reasoning-effort
fetched: 2026-08-13
---

# Reasoning Effort

Kimi K3 always reasons and configures **reasoning effort** with the top-level `reasoning_effort` request field. It supports `"low"`, `"high"`, and `"max"`, with `"max"` as the default.

## Set the reasoning effort

Set `reasoning_effort` at the top level of the Chat Completions request:

```json theme={null}
{
  "model": "kimi-k3",
  "messages": [{"role": "user", "content": "Derive the general formula for this sequence: 1, 4, 9, 25, 64, ..."}],
  "reasoning_effort": "high"
}
```

When migrating from K2.x to K3, remove the K2.x `thinking` configuration and use top-level `reasoning_effort` as needed.

<Tabs>
  <Tab title="curl">
    ```bash theme={null}
    $ curl https://api.moonshot.ai/v1/chat/completions \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $MOONSHOT_API_KEY" \
        -d '{
            "model": "kimi-k3",
            "messages": [
                {
                    "role": "user",
                    "content": "Derive the general formula for this sequence: 1, 4, 9, 25, 64, ..."
                }
            ],
            "reasoning_effort": "high"
        }'
    ```
  </Tab>

  <Tab title="python">
    ```python theme={null}
    import os
    from openai import OpenAI

    client = OpenAI(
        api_key=os.environ["MOONSHOT_API_KEY"],
        base_url="https://api.moonshot.ai/v1",
    )

    completion = client.chat.completions.create(
        model="kimi-k3",
        messages=[
            {"role": "user", "content": "Derive the general formula for this sequence: 1, 4, 9, 25, 64, ..."},
        ],
        reasoning_effort="high",
    )

    message = completion.choices[0].message
    if hasattr(message, "reasoning_content"):
        print(getattr(message, "reasoning_content"))
    print(message.content)
    ```
  </Tab>
</Tabs>

## Fields

| Field              | Type   | Required | Description                                                                                                  |
| ------------------ | ------ | -------- | ------------------------------------------------------------------------------------------------------------ |
| `reasoning_effort` | string | No       | K3's top-level reasoning-effort field; supports `"low"`, `"high"`, and `"max"`, with `"max"` as the default. |

For multi-turn conversations and tool calls, K3 requires the complete assistant message returned by the API to be passed back to `messages` as-is, including `reasoning_content` and `tool_calls`.

## Related reading

* [Kimi K3 API Tool Calling Best Practices](kimi-k3-tool-calling-best-practice.md): reasoning-effort configuration guidance for tool-calling scenarios
* [Thinking Mode](use-thinking-models.md): per-model thinking behavior and Preserved Thinking
* [Model Parameter Reference](../api/models-overview.md): parameter differences across models
