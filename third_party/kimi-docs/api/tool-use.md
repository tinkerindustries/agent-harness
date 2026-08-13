---
title: Tool Use
source: https://platform.kimi.ai/docs/api/tool-use
fetched: 2026-08-13
---

# Tool Use

Mastering the use of tools is a key hallmark of intelligence, and the Kimi large language model is no exception. Tool Use or Function Calling is a crucial feature of the Kimi large language model. When invoking the API to use the model service, you can describe tools or functions in the Messages, and the Kimi large language model will intelligently select and output a JSON object containing the parameters required to call one or more functions, thus enabling the Kimi large language model to link and utilize external tools.

Here is a simple example of tool invocation:

```json theme={null}
{
  "model": "kimi-k3",
  "messages": [
    {
      "role": "user",
      "content": "Determine whether 3214567 is a prime number through programming."
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "CodeRunner",
        "description": "A code executor that supports running Python and JavaScript code",
        "parameters": {
          "properties": {
            "language": {
              "type": "string",
              "enum": ["python", "javascript"]
            },
            "code": {
              "type": "string",
              "description": "The code is written here"
            }
          },
          "type": "object"
        }
      }
    }
  ]
}
```

<Frame>
  <img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/images/tooluse_whiteboard_example.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=29c34bce2bf0cc278436cfb11ea39848" alt="A diagram of the example above" width="835" height="644" data-path="assets/images/tooluse_whiteboard_example.png" />
</Frame>

In the `tools` field, we can add a list of optional tools.

Each tool in the list must include a `type`. Within the `function` structure, we need to include a `name` (which must follow this regular expression: `^[a-zA-Z_][a-zA-Z0-9-_]{2,63}$`). A name that is an easily understandable English word is more likely to be accepted by the model. There should also be a `description` or `enum`. The `description` part explains what the tool can do, which helps the model make judgments and selections.

Each function also supports a `strict` parameter (boolean, optional) that controls whether the tool call arguments are strictly constrained according to the `parameters` JSON Schema:

* **`true` (default, omitting is equivalent to `true`)**: The system strictly constrains output according to the `parameters` schema, which must conform to the [MFJS specification](https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md)
* **`false` (must be explicitly passed)**: Only guarantees output is a valid JSON object, without enforcing internal structure

The `function` structure must have a `parameters` field. The root of `parameters` must be an `object`, and the content must be a subset of JSON Schema conforming to the [MFJS specification](https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md).
If you encounter JSON Schema validation issues, please submit feedback at [walle GitHub Issues](https://github.com/MoonshotAI/walle/issues).

Like other APIs, we can call it through the Chat API.

<CodeGroup>
  ```python Python expandable theme={null}
  import os

  from openai import OpenAI

  client = OpenAI(
      api_key=os.environ["MOONSHOT_API_KEY"],
      base_url = "https://api.moonshot.ai/v1",
  )

  completion = client.chat.completions.create(
      model = "kimi-k3",
      messages = [
          {"role": "system", "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are proficient in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will reject any questions involving terrorism, racism, or explicit content. Moonshot AI is a proper noun and should not be translated."},
          {"role": "user", "content": "Determine whether 3214567 is a prime number through programming."}
      ],
      tools = [{
          "type": "function",
          "function": {
              "name": "CodeRunner",
              "description": "A code executor that supports running Python and JavaScript code",
              "parameters": {
                  "properties": {
                      "language": {
                          "type": "string",
                          "enum": ["python", "javascript"]
                      },
                      "code": {
                          "type": "string",
                          "description": "The code is written here"
                      }
                  },
              "type": "object"
              }
          }
      }]
  )

  print(completion.choices[0].message)
  ```

  ```bash cURL expandable theme={null}
  curl https://api.moonshot.ai/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer $MOONSHOT_API_KEY" \
      -d '{
          "model": "kimi-k3",
          "messages": [
              {"role": "system", "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are proficient in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will reject any questions involving terrorism, racism, or explicit content. Moonshot AI is a proper noun and should not be translated."},
              {"role": "user", "content": "Determine whether 3214567 is a prime number through programming."}
          ],
          "tools": [{
              "type": "function",
              "function": {
                  "name": "CodeRunner",
                  "description": "A code executor that supports running Python and JavaScript code",
                  "parameters": {
                      "properties": {
                          "language": {
                              "type": "string",
                              "enum": ["python", "javascript"]
                          },
                          "code": {
                              "type": "string",
                              "description": "The code is written here"
                          }
                      },
                  "type": "object"
                  }
              }
          }]
     }'
  ```

  ```javascript Node.js expandable theme={null}
  const OpenAI = require("openai");

  const client = new OpenAI({
      apiKey: process.env.MOONSHOT_API_KEY,
      baseURL: "https://api.moonshot.ai/v1",
  });

  async function main() {
      const completion = await client.chat.completions.create({
          model: "kimi-k3",
          messages: [
              {"role": "system", "content": "You are Kimi, an AI assistant provided by Moonshot AI. You are proficient in Chinese and English conversations. You provide users with safe, helpful, and accurate answers. You will reject any questions involving terrorism, racism, or explicit content. Moonshot AI is a proper noun and should not be translated."},
              {"role": "user", "content": "Determine whether 3214567 is a prime number through programming."}
          ],
          tools: [{
              "type": "function",
              "function": {
                  "name": "CodeRunner",
                  "description": "A code executor that supports running Python and JavaScript code",
                  "parameters": {
                      "properties": {
                          "language": {
                              "type": "string",
                              "enum": ["python", "javascript"]
                          },
                          "code": {
                              "type": "string",
                              "description": "The code is written here"
                          }
                      },
                  "type": "object"
                  }
              }
          }]
      });
      console.log(completion.choices[0].message);
  }

  main();
  ```
</CodeGroup>

### Tools Configuration

You can also use Agent platforms such as [Coze](https://coze.cn/), [Bisheng](https://github.com/dataelement/bisheng), [Dify](https://github.com/langgenius/dify/), and [LangChain](https://github.com/langchain-ai/langchain) to create and manage these tools, and design more complex workflows in conjunction with the Kimi large language model.
