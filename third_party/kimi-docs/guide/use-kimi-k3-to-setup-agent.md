---
title: Build an Agent with Kimi K3
source: https://platform.kimi.ai/docs/guide/use-kimi-k3-to-setup-agent
fetched: 2026-08-13
---

# Build an Agent with Kimi K3

Kimi K3 provides reasoning, coding, and tool-calling capabilities for complex tasks. This guide uses an industry-research agent to show how to combine an official web-search tool with one custom tool in a runnable, bounded agent.

## Break Down the Task

Split industry research into three stages before choosing tools and writing prompts:

1. **Retrieve**: define the research scope and find current data, company information, and news;
2. **Analyze**: compare sources, identify conflicts, and separate facts, estimates, and inferences;
3. **Deliver**: produce a structured report with a summary, key findings, risks, and sources.

This split lets the model plan and make judgments while tools handle retrieval or deterministic operations. Avoid repeating tool behavior in a long system prompt.

## Design Tools

This example combines two kinds of tools:

* Official `web-search`: retrieves current industry sources. The platform also provides `fetch`, `code-runner`, `excel`, and other tools. See [Official Tools](use-official-tools.md) for the full list and Formula API flow;
* Custom `build_research_plan`: generates a deterministic scope from a topic, region, and year range, demonstrating how to declare and execute a local function tool.

Custom tools describe arguments with JSON Schema. Setting `additionalProperties` to `false` and listing mandatory fields in `required` reduces invalid arguments.

When your inventory grows to dozens or hundreds of tools, do not send every schema in every request. Follow [Kimi K3 Tool Calling Best Practices](kimi-k3-tool-calling-best-practice.md) and [Dynamically Loaded Tools](use-dynamic-tool-loading.md) to retrieve candidates first and load them on demand.

## Design the Prompt

Keep the system prompt focused on the role, workflow, and quality boundaries. Leave argument details in the tool schema:

```python theme={null}
SYSTEM_PROMPT = """You are an industry research assistant. Define the scope first, retrieve and cross-check information, then write a concise report.
Requirements:
- Separate confirmed facts, estimates, and inferences; support key findings with multiple sources where possible.
- Never fabricate data or sources; state the search scope and gaps when evidence is insufficient.
- Include an executive summary, key findings, risks and limitations, and a source list.
- Answer in the same language as the user.
"""
```

Add constraints incrementally when the business format, compliance requirements, or audience changes. See [Prompt Best Practices](prompt-best-practice.md) for more guidance.

## Configure the K3 API

Use Python 3.9 or later, and install the OpenAI Python SDK and the HTTP client used for official Formula tools:

```bash theme={null}
python3 -m pip install --upgrade openai httpx
export MOONSHOT_API_KEY="YOUR_API_KEY"
```

The Global endpoint is `https://api.moonshot.ai/v1`, and the model is `kimi-k3`. The API key is read only from the `MOONSHOT_API_KEY` environment variable.

Kimi K3 always reasons, and its reasoning effort is configured with the top-level `reasoning_effort` request field, which supports `"low"` / `"high"` / `"max"` (default `"max"`). A tool loop must append the complete assistant message returned by the SDK to `messages`. Copying only `content` and `tool_calls` drops any returned `reasoning_content` and breaks the context needed by later tool calls. For parameters and model-specific behavior, see [Thinking Mode](use-thinking-models.md), [Reasoning Effort](use-reasoning-effort.md), and the [Model Parameter Reference](../api/models-overview.md).

## Complete Agent Loop

Save the following code as `agent.py`. It loads the `web-search` declaration dynamically, executes the custom tool locally, and handles tool calls in a loop capped at eight rounds.

```python theme={null}
import asyncio
import json
import os

import httpx
from openai import AsyncOpenAI

BASE_URL = "https://api.moonshot.ai/v1"
MODEL = "kimi-k3"
MAX_TOOL_ROUNDS = 8
SYSTEM_PROMPT = """You are an industry research assistant. Define the scope first, retrieve and cross-check information, then write a concise report.
Requirements:
- Separate confirmed facts, estimates, and inferences; support key findings with multiple sources where possible.
- Never fabricate data or sources; state the search scope and gaps when evidence is insufficient.
- Include an executive summary, key findings, risks and limitations, and a source list.
- Answer in the same language as the user.
"""

RESEARCH_PLAN_TOOL = {
    "type": "function",
    "function": {
        "name": "build_research_plan",
        "description": "Build a research plan from an industry topic, region, and time range",
        "parameters": {
            "type": "object",
            "properties": {
                "topic": {
                    "type": "string",
                    "description": "Industry or subject to research",
                },
                "region": {
                    "type": "string",
                    "enum": ["China", "Global", "United States", "Europe"],
                    "description": "Region to research",
                },
                "start_year": {
                    "type": "integer",
                    "description": "First year in the research period",
                },
                "end_year": {
                    "type": "integer",
                    "description": "Last year in the research period",
                },
            },
            "required": ["topic", "region", "start_year", "end_year"],
            "additionalProperties": False,
        },
    },
}


def build_research_plan(
    topic: str, region: str, start_year: int, end_year: int
) -> str:
    """Build a deterministic scope as a minimal custom-tool example."""
    if start_year > end_year:
        return json.dumps({"error": "start_year must not exceed end_year"})

    plan = {
        "topic": topic,
        "region": region,
        "period": f"{start_year}-{end_year}",
        "dimensions": [
            "market size and growth",
            "value chain and major companies",
            "technology trends",
            "policy and risks",
        ],
        "search_queries": [
            f"{region} {topic} market size {start_year} {end_year}",
            f"{region} {topic} major companies technology trends",
            f"{region} {topic} policy risks",
        ],
    }
    return json.dumps(plan)


class IndustryResearchAgent:
    def __init__(self) -> None:
        api_key = os.environ["MOONSHOT_API_KEY"]
        self.openai = AsyncOpenAI(api_key=api_key, base_url=BASE_URL)
        self.http = httpx.AsyncClient(
            base_url=BASE_URL,
            headers={"Authorization": f"Bearer {api_key}"},
            timeout=60.0,
        )

    async def load_formula(
        self, formula_uri: str
    ) -> tuple[list[dict], dict[str, str]]:
        response = await self.http.get(f"/formulas/{formula_uri}/tools")
        response.raise_for_status()
        tools = response.json().get("tools", [])
        if not tools:
            raise RuntimeError(f"Formula {formula_uri} returned no tools")

        tool_to_formula = {
            tool["function"]["name"]: formula_uri
            for tool in tools
            if tool.get("type") == "function" and tool.get("function")
        }
        if not tool_to_formula:
            raise RuntimeError(f"Formula {formula_uri} returned no callable function tools")
        return tools, tool_to_formula

    async def call_formula(
        self, formula_uri: str, name: str, arguments: dict
    ) -> str:
        response = await self.http.post(
            f"/formulas/{formula_uri}/fibers",
            json={"name": name, "arguments": json.dumps(arguments)},
        )
        response.raise_for_status()
        fiber = response.json()
        context = fiber.get("context", {})

        if fiber.get("status") == "succeeded":
            result = context.get("output") or context.get("encrypted_output") or ""
        else:
            result = fiber.get("error") or context.get("error") or "Unknown tool error"

        if isinstance(result, str):
            return result
        return json.dumps(result)

    async def run(self, question: str) -> str:
        official_tools, tool_to_formula = await self.load_formula(
            "moonshot/web-search:latest"
        )
        tools = [RESEARCH_PLAN_TOOL, *official_tools]
        messages = [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": question},
        ]

        for _ in range(MAX_TOOL_ROUNDS):
            response = await self.openai.chat.completions.create(
                model=MODEL,
                messages=messages,
                tools=tools,
                max_completion_tokens=8192,
            )
            choice = response.choices[0]
            message = choice.message

            # Append the complete SDK message, preserving reasoning_content and tool_calls.
            messages.append(message)

            if choice.finish_reason == "length":
                raise RuntimeError(
                    "The model output was truncated by max_completion_tokens; "
                    "increase the limit or shorten tool results"
                )

            if not message.tool_calls:
                if choice.finish_reason != "stop":
                    raise RuntimeError(
                        f"Unexpected finish reason: {choice.finish_reason}"
                    )
                if not message.content:
                    raise RuntimeError("The model returned no final report")
                return message.content

            for tool_call in message.tool_calls:
                name = tool_call.function.name
                try:
                    arguments = json.loads(tool_call.function.arguments or "{}")
                    if not isinstance(arguments, dict):
                        raise ValueError("Tool arguments must be a JSON object")

                    if name == "build_research_plan":
                        result = build_research_plan(**arguments)
                    elif name in tool_to_formula:
                        result = await self.call_formula(
                            tool_to_formula[name], name, arguments
                        )
                    else:
                        raise ValueError(f"Unknown tool: {name}")
                except Exception as exc:
                    result = json.dumps(
                        {"error": f"{type(exc).__name__}: {exc}"}
                    )

                # Return every result and continue other calls even if one tool fails.
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": tool_call.id,
                        "content": result,
                    }
                )

        raise RuntimeError(f"Tool calls exceeded {MAX_TOOL_ROUNDS} rounds")

    async def close(self) -> None:
        try:
            await self.openai.close()
        finally:
            await self.http.aclose()


async def main() -> None:
    agent = IndustryResearchAgent()
    try:
        report = await agent.run(
            "Research the development of China's humanoid-robot industry from 2024 to 2026"
        )
        print(report)
    finally:
        await agent.close()


if __name__ == "__main__":
    asyncio.run(main())
```

Two context details in the loop are mandatory:

1. `messages.append(message)` appends the complete assistant message and preserves K3's `reasoning_content`;
2. every tool message uses the matching `tool_call_id`, so the model can associate each result with its call.

The example fails immediately on `finish_reason="length"` instead of treating truncated output as a final report. If one tool call fails, its error is returned as the matching tool result while the loop continues processing the other calls in that round.

`MAX_TOOL_ROUNDS` prevents endless tool calls and avoids the growing call stack of a recursive implementation.

## Run and Troubleshoot

Run the example:

```bash theme={null}
python3 agent.py
```

Replace the question in `main()` with your target industry, region, and years. The final response should contain a summary, key findings, risks, and sources instead of a dump of raw tool output.

Common issues:

* **`finish_reason` is `length`**: the example raises `RuntimeError`; increase `max_completion_tokens` using the [Model Parameter Reference](../api/models-overview.md), or shorten tool results;
* **Maximum tool rounds reached**: check for overlapping tool descriptions and ambiguous results, then narrow the task;
* **A tool repeatedly returns argument errors**: check the function schema, `required`, `enum`, and `additionalProperties`; do not replace argument constraints with prompt text;
* **A later tool round fails**: confirm that you append the complete SDK assistant message and preserve the correct `tool_call_id` on every result;
* **An official tool request fails**: verify the endpoint, API key, Formula URI, and tool availability. See [Official Tools](use-official-tools.md) for the full API flow.

## Custom Tools and Optimization

The included `build_research_plan` example covers the complete path: declare a schema, dispatch locally, return JSON, and attach the matching `tool_call_id`. To connect a database, internal search service, or file generator, replace the function implementation while keeping its schema and return shape aligned.

For further optimization:

* expose only tools needed for the current task and avoid overlapping tools;
* validate tool inputs in your application and return structured errors so the model can correct arguments;
* use [Dynamically Loaded Tools](use-dynamic-tool-loading.md) for large inventories;
* before using `tool_choice`, reasoning effort, or related controls, check [Kimi K3 Tool Calling Best Practices](kimi-k3-tool-calling-best-practice.md) and the [Model Parameter Reference](../api/models-overview.md).
