---
title: Tool Choice
source: https://platform.kimi.ai/docs/guide/use-tool-choice
fetched: 2026-08-13
---

# Tool Choice

Once tools are declared (via `tools`), the model decides on its own whether the current turn needs a tool call. The `tool_choice` parameter gives you explicit control over this behavior: force a call, forbid calls entirely, or keep the default.

## Force a tool call: `"required"`

Use this when your workflow must go through the tool path — for example, mandatory retrieval or a mandatory database lookup — and the model is not allowed to answer from memory:

```json theme={null}
{
  "tool_choice": "required"
}
```

The model must call at least one tool in this turn. Make sure the request declares at least one callable tool. A typical use is the tool-search pattern: set `"required"` on the first turn to force the model to call `search_tools`, then switch back to `"auto"` after retrieval — see [Kimi K3 API Tool Calling Best Practices](kimi-k3-tool-calling-best-practice.md).

## Forbid tool calls: `"none"`

Use this when the request only needs a plain-text answer and you don't want the model to trigger a tool call by mistake:

```json theme={null}
{
  "tool_choice": "none"
}
```

The model replies with plain text and produces no `tool_calls`, which also reduces latency and token consumption.

## Let the model decide: `"auto"` (default)

Omitting `tool_choice` is the same as `"auto"`: the model decides based on the context whether to call a tool. This fits regular conversations.

## Force a specific tool: pass a function object

Beyond the three enum values, `tool_choice` also accepts a function object that forces the model to call the specified tool:

```json theme={null}
{
  "tool_choice": {"type": "function", "function": {"name": "get_weather"}}
}
```

<Note>
  Forcing a specific tool is currently incompatible with thinking: with thinking enabled, the request returns a 400 error (`tool_choice 'specified' is incompatible with thinking enabled`).
</Note>

## Full request example

The following example declares a weather tool and uses `tool_choice: "required"` to force the model to call it:

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
                    "content": "What is the weather like in Beijing today?"
                }
            ],
            "tools": [
                {
                    "type": "function",
                    "function": {
                        "name": "get_weather",
                        "description": "Get the current weather for a given city",
                        "parameters": {
                            "type": "object",
                            "properties": {
                                "city": {
                                    "type": "string",
                                    "description": "City name"
                                }
                            },
                            "required": ["city"]
                        }
                    }
                }
            ],
            "tool_choice": "required"
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
            {"role": "user", "content": "What is the weather like in Beijing today?"},
        ],
        tools=[
            {
                "type": "function",
                "function": {
                    "name": "get_weather",
                    "description": "Get the current weather for a given city",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "city": {"type": "string", "description": "City name"}
                        },
                        "required": ["city"],
                    },
                },
            }
        ],
        # Force the model to call at least one tool; defaults to "auto" when omitted
        tool_choice="required",
    )

    print(completion.choices[0].message.tool_calls)
    ```
  </Tab>
</Tabs>

## Notes

* `tool_choice` is a request-level parameter: it takes effect independently for each request and only constrains tool selection for that generation;
* Setting `tool_choice` (or not setting it) **does not invalidate the prefix cache**, so feel free to adjust it on a per-request basis.

## Related reading

* [Kimi K3 API Tool Calling Best Practices](kimi-k3-tool-calling-best-practice.md): combined practices for dynamic loading, tool\_choice, and reasoning effort
* [Dynamically Loaded Tools](use-dynamic-tool-loading.md): inject tool definitions on demand when you have a large tool inventory, reducing token usage and improving tool-selection accuracy
* [Use Kimi API for Tool Calls](use-kimi-api-to-complete-tool-calls.md): the complete tool-calling workflow and examples
* [Model Parameter Reference](../api/models-overview.md): per-model support for the `tool_choice` parameter
