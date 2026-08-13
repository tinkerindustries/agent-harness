---
title: Use Playground to Debug the Model
source: https://platform.kimi.ai/docs/guide/use-playground-to-debug-the-model
fetched: 2026-08-13
---

# Use Playground to Debug the Model

The [Playground development workbench](https://platform.kimi.ai/playground) is a powerful platform for model debugging and testing, providing an intuitive interface for interacting with and testing AI models. Through this workbench, you can:

1. Adjust and observe model performance and output effects under different parameters
2. Experience the model's tool calling capabilities using Kimi Open Platform's built-in tools
3. Compare different models' effects under the same parameters
4. Monitor token usage to optimize costs

## Model Debugging Features

**Prompt Settings**

* Set system prompts at the top to define behavioral guidelines that direct model output
* Support defining prompts for three roles: system/user/assistant

**Model Configuration**

* **Model Selection**: Choose from currently available models such as Kimi K3, Kimi K2.7 Code, and Kimi K2.6
* **Parameter Configuration**: For supported parameters and field descriptions, see [Request Parameter Description](../api/chat.md)

**Model Conversation**

* Send chat content through the input box below
* **Tool Call Display**: Shows the tool calling process, including call ID/tool parameters/return results
* **View Code**: View and copy the API call code for the current session
* Bottom Statistics: Displays the input/output/total token consumption for this conversation, including context history messages and prompt information

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/prompt-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=99c716f14cda4c6c61b021b482af8ef1" alt="prompt" width="1902" height="1840" data-path="assets/pics/playground/prompt-en.png" />

## Tool Debugging

### Official Tools

* Kimi Open Platform provides officially supported tools that execute for free. You can select tools in the playground, and the model will automatically determine whether tool calls are needed to complete your instructions. If tool calls are required, the model will generate parameters according to the tool's requirements and integrate them into the final answer.
* **Quota and Rate Limiting**: The tools provided by Kimi Open Platform are pre-built functions that can be quickly executed online without requiring you to prepare a local tool execution environment. Currently, tool execution on Kimi Open Platform is temporarily free, but temporary rate limiting measures may be implemented when tool load reaches capacity limits.
* Currently supported tools: Date/Time tools, Excel file analysis tools, Web search tools, Random number generation tools, etc.
* Currently, it supports calling official tools through Kimi API, see the document [How to Use Official Tools in Kimi API](use-official-tools.md)
* Custom tool upload and execution is not currently supported.

### Use MCP Server

* In Kimi Playground, you can configure ModelScope MCP servers to use ModelScope's tools.
* Configuration steps: [Configure ModelScope MCP Server in Playground](configure-the-modelscope-mcp-server.md)
* You can configure other MCP servers by adding MCP server features, inputting or selecting MCP server URL/transport protocol/authentication method, and clicking add.

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/add-mcp-server-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=99a5c909e65aa80ace86f9d3a472f04f" alt="mcp" width="1862" height="1836" data-path="assets/pics/playground/add-mcp-server-en.png" />

### Show Case 1: Today's News Report

* Scenario: Using tool capabilities to request the model to search for today's news and organize it into an HTML web report
* Tool Selection: date tool, web\_search tool, rethink tool
* Note: The web\_search tool calls Kimi Open Platform's web search service. Single web searches are billed, see [Pricing](../pricing/tools.md) for specific billing standards
* Click the showcase button on the page to quickly experience the tool effects

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/showcase-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=f94fef057a0257be2f35fc153e40b9bc" alt="date" width="1928" height="1852" data-path="assets/pics/playground/showcase-en.png" />

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/today-news-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=b7b098ffbbb174eff6ed0e5e9d9432cb" alt="date" width="1874" height="1570" data-path="assets/pics/playground/today-news-en.png" />

### Show Case 2: Spreadsheet Analysis Tool

* Tool Selection: Excel analysis tool

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/excel-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=56c5e62542bfc4dacf513a24e0ff85d8" alt="excel" width="1896" height="1580" data-path="assets/pics/playground/excel-en.png" />

## Model Comparison

* Create new conversations through the add conversation feature, supporting up to 3 models running simultaneously

<img src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/model-compare-en.png?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=ea36ba49fab6ff73b9b95e5dcf7aed56" alt="Model Comparison" width="1870" height="1838" data-path="assets/pics/playground/model-compare-en.png" />

## Share Conversations

* **Export**: Export the current conversation content, including all configurations and context, as a .json format file
* **Import**: Import shared or previously exported .json conversation content, and the playground will render the session on the page
* Note: Data after rerun will regenerate and overwrite previous chat content. If the imported case includes uploaded files, the imported session cannot be rerun

<Frame>
  <video controls style={{ width: '100%', height: 'auto' }}>
    <source src="https://mintcdn.com/moonshotai/AP772Is8kVTEjpLL/assets/pics/playground/upload-en.mp4?fit=max&auto=format&n=AP772Is8kVTEjpLL&q=85&s=a3f12d99c03d9ea378807bbd15d55b4d" type="video/mp4" data-path="assets/pics/playground/upload-en.mp4" />

    Your browser does not support video playback.
  </video>
</Frame>
