---
title: Use the Streaming Feature of the Kimi API
source: https://platform.kimi.ai/docs/guide/utilize-the-streaming-output-feature-of-kimi-api
fetched: 2026-08-13
---

# Use the Streaming Feature of the Kimi API

After receiving a question, the Kimi large language model first performs inference and then generates the answer one Token at a time. Streaming sends Tokens to the client as soon as a certain number of them (usually 1 Token) is generated, instead of waiting until the full response is complete. Waiting for the complete response usually takes several seconds — for complex questions and long replies it can stretch to 10 or even 20 seconds; with streaming, users see the first Token immediately, which significantly reduces wait time. When you chat with [Kimi AI Assistant](https://kimi.ai), the reply appears character by character — that is streaming in action.

## Enable Streaming Output

Set `stream=True` in the request to enable streaming. The SDK then returns an iterable — loop over it to read data chunks one by one. Each chunk has a structure similar to a completion, except the `message` field is replaced by a `delta` field:

<Note>
  The examples on this page use the latest model `kimi-k3` by default. K3 configures reasoning effort with the top-level `reasoning_effort` request field (supports `"low"` / `"high"` / `"max"`, default `"max"`). To use another model such as `kimi-k2.6` or `kimi-k2.5`, just replace the `model` field — parameter configurations differ across models. See the [Model Parameter Reference](../api/models-overview.md).
</Note>

<Tabs>
  <Tab title="python">
    ```python theme={null}
    import os
    from openai import OpenAI
     
    client = OpenAI(
        api_key = os.environ["MOONSHOT_API_KEY"], # Set the MOONSHOT_API_KEY environment variable before running this example
        base_url = "https://api.moonshot.ai/v1",
    )
     
    stream = client.chat.completions.create(
        model = "kimi-k3",
        messages = [
            {"role": "system", "content": "You are Kimi, an artificial intelligence assistant provided by Moonshot AI, who is better at conversing in Chinese and English. You provide users with safe, helpful, and accurate answers. At the same time, you refuse to answer any questions related to terrorism, racism, pornography, and violence. Moonshot AI is a proper noun and should not be translated into other languages."},
            {"role": "user", "content": "Hello, my name is Li Lei, what is 1+1?"}
        ],
        stream=True, # <-- Note here, we enable streaming output mode by setting stream=True
    )

    # When streaming output mode is enabled (stream=True), the content returned by the SDK also changes. We no longer directly access the choice in the return value
    # Instead, we access each individual chunk in the return value through a for loop

    for chunk in stream:
    	# Here, the structure of each chunk is similar to the previous completion, but the message field is replaced with the delta field
    	delta = chunk.choices[0].delta # <-- The message field is replaced with the delta field

    	if delta.content:
    		# When printing the content, since it is streaming output, to ensure the coherence of the sentence, we do not add
    		# line breaks manually, so we set end="" to cancel the line break of print.
    		print(delta.content, end="")
    ```
  </Tab>

  <Tab title="node.js">
    ```js theme={null}
    const OpenAI = require('openai')
     
    const client = new OpenAI({
        apiKey: process.env.MOONSHOT_API_KEY, // Set the MOONSHOT_API_KEY environment variable before running this example
        baseURL: "https://api.moonshot.ai/v1",
    })

    async function main() {
        const stream = await client.chat.completions.create({
            model: "kimi-k3",
            messages: [
                {role: "system", content: "You are Kimi, an artificial intelligence assistant provided by Moonshot AI, who is better at conversing in Chinese and English. You provide users with safe, helpful, and accurate answers. At the same time, you refuse to answer any questions related to terrorism, racism, pornography, and violence. Moonshot AI is a proper noun and should not be translated into other languages."},
                {role: "user", content: "Hello, my name is Li Lei, what is 1+1?"}
            ],
            stream: true, // <-- Note here, we enable streaming output mode by setting stream=True
        })
         
        // When streaming output mode is enabled (stream=True), the content returned by the SDK also changes. We no longer directly access the choice in the return value
        // Instead, we access each individual chunk in the return value through a for loop
         
        for await (chunk of stream) {
            // Here, the structure of each chunk is similar to the previous completion, but the message field is replaced with the delta field
            delta = chunk.choices[0].delta // <-- The message field is replaced with the delta field
         
            if (delta.content) {
                // When printing the content, since it is streaming output, to ensure the coherence of the sentence, we do not add
                // line breaks manually, so we set end="" to cancel the line break of print.
                console.log(delta.content, end="")
            }
        }
    }

    main()
    ```
  </Tab>
</Tabs>

## Parse the SSE Response Body

With streaming enabled, the API no longer returns a JSON response (`Content-Type: application/json`); it returns `Content-Type: text/event-stream` (SSE) instead, which lets the server continuously push Tokens to the client. An [SSE](https://kimi.ai/share/cr7boh3dqn37a5q9tds0) response body looks like this:

```text theme={null}
data: {"id":"cmpl-1305b94c570f447fbde3180560736287","object":"chat.completion.chunk","created":1698999575,"model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}
 
data: {"id":"cmpl-1305b94c570f447fbde3180560736287","object":"chat.completion.chunk","created":1698999575,"model":"kimi-k3","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
 
...
 
data: {"id":"cmpl-1305b94c570f447fbde3180560736287","object":"chat.completion.chunk","created":1698999575,"model":"kimi-k3","choices":[{"index":0,"delta":{"content":"."},"finish_reason":null}]}
 
data: {"id":"cmpl-1305b94c570f447fbde3180560736287","object":"chat.completion.chunk","created":1698999575,"model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":19,"completion_tokens":13,"total_tokens":32}}]}
 
data: [DONE]
```

In the response body, each data chunk starts with the `data: ` prefix, followed by a valid JSON object, and ends with two newline characters `\n\n`. Once all chunks are transmitted, the server sends `data: [DONE]` to mark completion, at which point you can close the connection.

*Note: always use `data: [DONE]` to determine whether the data has been fully transmitted, not `finish_reason` or any other means. If you have not received `data: [DONE]`, do not consider the transmission complete even if `finish_reason=stop` was received; in other words, until `data: [DONE]` arrives, the message should be considered **incomplete**.*

During streaming, the `content` field is delivered chunk by chunk; `role` and `usage` are not repeated in every chunk — `role` appears only in the first chunk, and `usage` only in the last one.

## Count Token Usage

There are two ways to count tokens. The most direct and accurate one is to wait until all chunks have been transmitted, then read the `usage` field of the last chunk to see the request's `prompt_tokens`/`completion_tokens`/`total_tokens`:

```text theme={null}
...
 
data: {"id":"cmpl-1305b94c570f447fbde3180560736287","object":"chat.completion.chunk","created":1698999575,"model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":19,"completion_tokens":13,"total_tokens":32}}]}
                                               ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
                                               Check the number of tokens generated by the current request through the usage field in the last data chunk
data: [DONE]
```

<Note>
  Note that `usage` is nested inside `choices[0]` of the last chunk (`choices[0].usage`), not at the top level of the chunk. With the OpenAI SDK, `chunk.usage` is `None` — read `chunk.choices[0].usage` instead, or parse the raw SSE chunks directly.
</Note>

However, a stream can be interrupted by uncontrollable factors such as a network drop or a client-side error, in which case the last chunk never arrives and the request's token consumption cannot be determined. To avoid this, save the content of every chunk you receive and, once the request ends (whether successfully or not), call the token-count endpoint to compute the actual consumption:

<Tabs>
  <Tab title="python">
    ```python theme={null}
    import os
    import httpx
    from openai import OpenAI
     
    client = OpenAI(
        api_key = os.environ["MOONSHOT_API_KEY"], # Set the MOONSHOT_API_KEY environment variable before running this example
        base_url = "https://api.moonshot.ai/v1",
    )
     
    stream = client.chat.completions.create(
        model = "kimi-k3",
        messages = [
            {"role": "system", "content": "You are Kimi, an AI assistant provided by Moonshot AI, who excels in Chinese and English conversations. You provide users with safe, helpful, and accurate answers while rejecting any questions related to terrorism, racism, or explicit content. Moonshot AI is a proper noun and should not be translated."},
            {"role": "user", "content": "Hello, my name is Li Lei. What is 1+1?"}
        ],
        stream=True, # <-- Note here, we enable streaming output mode by setting stream=True
    )


    def estimate_token_count(input: str) -> int:
        """
        Implement your token calculation logic here, or directly call our token calculation interface to compute tokens.

        https://api.moonshot.ai/v1/tokenizers/estimate-token-count
        """
        header = {
            "Authorization": f"Bearer {os.environ['MOONSHOT_API_KEY']}",
        }
        data = {
            "model": "kimi-k3",
            "messages": [
                {"role": "user", "content": input},
            ]
        }
        r = httpx.post("https://api.moonshot.ai/v1/tokenizers/estimate-token-count", headers=header, json=data)
        r.raise_for_status()
        return r.json()["data"]["total_tokens"]


    completion = []
    for chunk in stream:
    	delta = chunk.choices[0].delta
    	if delta.content:
    		completion.append(delta.content)


    print("completion_tokens:", estimate_token_count("".join(completion)))
    ```
  </Tab>

  <Tab title="node.js">
    ```js theme={null}
    const axios = require('axios');
    const OpenAI = require('openai');
     
    client = new OpenAI({
        apiKey: process.env.MOONSHOT_API_KEY,
        baseURL: "https://api.moonshot.ai/v1",
    })
     
     
    async function estimate_token_count(input_messages) {
        /*
        Implement your token calculation logic here, or directly call our token calculation interface to compute tokens.
     
        https://api.moonshot.ai/v1/tokenizers/estimate-token-count
        */
        header = {
            "Authorization": `Bearer ${process.env.MOONSHOT_API_KEY}`,
        }
        data = {
            "model": "kimi-k3",
            "messages": input_messages,
        }
        r = await axios.post("https://api.moonshot.ai/v1/tokenizers/estimate-token-count", data, {headers: header})
        .catch(function (error) {
            console.log(error)
        })
        return r.data.data.total_tokens
    } 

    async function main() {

        const stream = await client.chat.completions.create({
            model: "kimi-k3",
            messages: [
                {role: "system", content: "You are Kimi, an AI assistant provided by Moonshot AI, who excels in Chinese and English conversations. You provide users with safe, helpful, and accurate answers while rejecting any questions related to terrorism, racism, or explicit content. Moonshot AI is a proper noun and should not be translated."},
                {role: "user", content: "Hello, my name is Li Lei. What is 1+1?"}
            ],
            stream: true, // <-- Note here, we enable streaming output mode by setting stream=True
        })
        
        const completion = [];
        for await (chunk of stream) {
            const delta = chunk.choices[0].delta
            if (delta.content) {
                completion.push(delta.content)
            }
        }
         
        console.log("completion_tokens:", await estimate_token_count(completion.join("")))
    }

    main()
    ```
  </Tab>
</Tabs>

## Stop Streaming Output

To terminate the output early, simply close the HTTP connection or discard subsequent chunks — for example, `break` out of the loop:

```python theme={null}
for chunk in stream:
	if condition:
		break
```

## Handle SSE Without an SDK

In a language without an SDK, or when the SDK cannot accommodate your business logic, you can interface with the HTTP API directly to handle streaming output. The following examples show how to read and parse the [SSE](https://kimi.ai/share/cr7boh3dqn37a5q9tds0) response body line by line; see the code comments for details:

<Tabs>
  <Tab title="python">
    ```python theme={null}
    import os
    import json
    import httpx # We use the httpx library to make our HTTP requests


    data = {
    	"model": "kimi-k3",
    	"messages": [
    		# Specific messages
    	],
    	"stream": True,
    }


    # Use httpx to send a chat request to the Kimi large language model and get the response r
    r = httpx.post("https://api.moonshot.ai/v1/chat/completions", headers={"Authorization": f"Bearer {os.environ['MOONSHOT_API_KEY']}"}, json=data)
    if r.status_code != 200:
    	raise Exception(r.text)


    data: str

    # Here, we use the iter_lines method to read the response body line by line
    for line in r.iter_lines():
    	# Remove leading and trailing spaces from each line to better handle data chunks
    	line = line.strip()

    	# Next, we need to handle three different cases:
    	#   1. If the current line is empty, it indicates that the previous data chunk has been received (as mentioned earlier, the data chunk transmission ends with two newline characters), we can deserialize the data chunk and print the corresponding content;
    	#   2. If the current line is not empty and starts with data:, it indicates the start of a data chunk transmission, we remove the data: prefix and first check if it is the end symbol [DONE], if not, save the data content to the data variable;
    	#   3. If the current line is not empty but does not start with data:, it indicates that the current line still belongs to the previous data chunk being transmitted, we append the content of the current line to the end of the data variable;

    	if len(line) == 0:
    		chunk = json.loads(data)

    		# The processing logic here can be replaced with your business logic, printing is just to demonstrate the process
    		choice = chunk["choices"][0]
    		usage = choice.get("usage")
    		if usage:
    			print("total_tokens:", usage["total_tokens"])
    		delta = choice["delta"]
    		role = delta.get("role")
    		if role:
    			print("role:", role)
    		content = delta.get("content")
    		if content:
    			print(content, end="")

    		data = "" # Reset data
    	elif line.startswith("data: "):
    		data = line.lstrip("data: ")

    		# When the data chunk content is [DONE], it indicates that all data chunks have been sent, and the network connection can be disconnected
    		if data == "[DONE]":
    			break
    	else:
    		data = data + "\n" + line # We still add a newline character when appending content, as this data chunk may intentionally format the data in separate lines
    ```
  </Tab>

  <Tab title="node.js">
    ```js theme={null}
    const axios = require('axios'); // Use the axios library to make HTTP requests

    let data = {
        "model": "kimi-k3",
        "messages": [
            // Specific messages
        ],
        "stream": true,
    };

    // Use axios to send a chat request to the Kimi large language model and get the response r
    axios.post("https://api.moonshot.ai/v1/chat/completions", data, {
        responseType: 'stream'
    }).then(response => {
        let data = '';
        response.data.on('data', chunk => {
            // Remove leading and trailing spaces from each line to better handle data chunks
            let line = chunk.toString().trim();

            // Next, we need to handle three different cases:
            //   1. If the current line is empty, it indicates that the previous data chunk has been received (as mentioned earlier, the data chunk transmission ends with two newline characters), we can deserialize the data chunk and print the corresponding content;
            //   2. If the current line is not empty and starts with data:, it indicates the start of a data chunk transmission, we remove the data: prefix and first check if it is the end symbol [DONE], if not, save the data content to the data variable;
            //   3. If the current line is not empty but does not start with data:, it indicates that the current line still belongs to the previous data chunk being transmitted, we append the content of the current line to the end of the data variable;

            if (line === '') {
                try {
                    let chunk = JSON.parse(data);
                    // The processing logic here can be replaced with your business logic, printing is just to demonstrate the process
                    let choice = chunk.choices[0];
                    let usage = choice.usage;
                    if (usage) {
                        console.log("total_tokens:", usage.total_tokens);
                    }
                    let delta = choice.delta;
                    let role = delta.role;
                    if (role) {
                        console.log("role:", role);
                    }
                    let content = delta.content;
                    if (content) {
                        console.log(content);
                    }
                } catch (error) {
                    console.error("Error parsing JSON:", error);
                }
                data = ''; // Reset data
            } else if (line.startsWith('data: ')) {
                data = line.substring(6);
                // When the data chunk content is [DONE], it indicates that all data chunks have been sent, and the network connection can be disconnected
                if (data === '[DONE]') {
                    response.data.destroy();
                }
            } else {
                data += '\n' + line; // We still add a newline character when appending content, as this data chunk may intentionally format the data in separate lines
            }
        });
    }).catch(error => {
        console.error("Error in request:", error);
    });
    ```
  </Tab>
</Tabs>

Whatever the language, the basic steps for handling streaming output are the same:

1. Send an HTTP request with the `stream` parameter set to `true` in the request body;
2. Check the `Content-Type` in the response `Headers` — `text/event-stream` means the response is a streaming output;
3. Read the response line by line and parse the data chunks (in JSON format), locating chunk boundaries via the `data: ` prefix and newline characters `\n`;
4. A chunk whose content is `[DONE]` marks the end of the transmission.

## Multiple Responses (`n` Parameter)

<Note>
  Current models (`kimi-k3`, `kimi-k2.7-code`, `kimi-k2.6`) fix `n` at `1` and do not support returning multiple responses in a single request. Passing an `n` greater than 1 returns a 400 error (`invalid n: only 1 is allowed for this model`) for both streaming and non-streaming requests. See the [Model Parameter Reference](../api/models-overview.md) for per-model parameter constraints.
</Note>
