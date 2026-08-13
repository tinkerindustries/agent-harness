---
title: MoonPalace - Moonshot AI's Kimi API Debugging Tool
source: https://platform.kimi.ai/docs/guide/use-moonpalace
fetched: 2026-08-13
---

# MoonPalace - Moonshot AI's Kimi API Debugging Tool

MoonPalace (Moon Palace) is an API debugging tool provided by Moonshot AI. It has the following features:

* **Cross-platform support**:
  * [x] Mac
  * [x] Windows
  * [x] Linux
* **Easy to use**, just replace `base_url` with `http://localhost:9988` after launching to start debugging;
* **Captures complete requests**, including the "scene of the accident" when network errors occur;
* **Quickly search and view request information** using `request_id` and `chatcmpl_id`;
* **One-click export of BadCase structured reporting data**, helping to enhance Kimi's model capabilities;

**We recommend using MoonPalace as your API "supplier" during the code writing and debugging phase, so you can quickly identify and locate various issues related to API calls and code writing. For any unexpected outputs from Kimi large language model, you can also export the request details via MoonPalace and submit them to Moonshot AI to improve Kimi large language model.**

## Installation Methods

### Using the `go` Command to Install

If you have the `go` toolchain installed, you can run the following command to install MoonPalace:

```shell theme={null}
$ go install github.com/MoonshotAI/moonpalace@latest
```

The above command will install the compiled binary file in your `$GOPATH/bin/` directory. Run the `moonpalace` command to check if it has been installed successfully:

```shell theme={null}
$ moonpalace
MoonPalace is a command-line tool for debugging the Moonshot AI HTTP API.

Usage:
  moonpalace [command]

Available Commands:
  cleanup     Cleanup Moonshot AI requests.
  completion  Generate the autocompletion script for the specified shell
  export      export a Moonshot AI request.
  help        Help about any command
  inspect     Inspect the specific content of a Moonshot AI request.
  list        Query Moonshot AI requests based on conditions.
  start       Start the MoonPalace proxy server.

Flags:
  -h, --help      help for moonpalace
  -v, --version   version for moonpalace

Use "moonpalace [command] --help" for more information about a command.
```

*If you still cannot find the `moonpalace` binary file, try adding the `$GOPATH/bin/` directory to your `$PATH` environment variable.*

### Downloading from the Releases Page

You can download the precompiled binary (executable) files from the [Releases](https://github.com/MoonshotAI/moonpalace/releases) page:

* moonpalace-linux
* moonpalace-macos-amd64 {'=>'} for Intel-based Macs
* moonpalace-macos-arm64 {'=>'} for Apple Silicon-based Macs
* moonpalace-windows.exe

Download the binary (executable) file that matches your platform and place it in a directory that is included in your `$PATH` environment variable. Rename it to `moonpalace` and then grant it executable permissions.

## Usage

### Starting the Service

Use the following command to start the MoonPalace proxy server:

```shell theme={null}
$ moonpalace start --port <PORT>
```

MoonPalace will start an HTTP server locally, with the `--port` parameter specifying the local port that MoonPalace will listen on. The default value is `9988`. When MoonPalace starts successfully, it will output:

```shell theme={null}
[MoonPalace] 2024/07/29 17:00:29 MoonPalace Starts {'=>'} change base_url to "http://127.0.0.1:9988/v1"
```

As instructed, replace `base_url` with the displayed address. If you are using the default port, set `base_url=http://127.0.0.1:9988/v1`. If you are using a custom port, replace `base_url` with the displayed address.

**Additionally, if you want to always use a debugging `api_key` during debugging, you can use the `--key` parameter when starting MoonPalace to set a default `api_key` for MoonPalace. This way, you don't have to manually set the `api_key` in each request. MoonPalace will automatically add the `api_key` you set with `--key` when requesting the Kimi API.**

If you have correctly set `base_url` and successfully called the Kimi API, MoonPalace will output the following information:

```shell theme={null}
$ moonpalace start --port <PORT>
[MoonPalace] 2024/07/29 17:00:29 MoonPalace Starts {'=>'} change base_url to "http://127.0.0.1:9988/v1"
[MoonPalace] 2024/07/29 21:30:53 POST   /v1/chat/completions 200 OK
[MoonPalace] 2024/07/29 21:30:53   - Request Headers: 
[MoonPalace] 2024/07/29 21:30:53     - Content-Type:   application/json
[MoonPalace] 2024/07/29 21:30:53   - Response Headers: 
[MoonPalace] 2024/07/29 21:30:53     - Content-Type:   application/json
[MoonPalace] 2024/07/29 21:30:53     - Msh-Request-Id: c34f3421-4dae-11ef-b237-9620e33511ee
[MoonPalace] 2024/07/29 21:30:53     - Server-Timing:  7134
[MoonPalace] 2024/07/29 21:30:53     - Msh-Uid:        cn0psmmcp7fclnphkcpg
[MoonPalace] 2024/07/29 21:30:53     - Msh-Gid:        enterprise-tier-5
[MoonPalace] 2024/07/29 21:30:53   - Response: 
[MoonPalace] 2024/07/29 21:30:53     - id:                cmpl-12be8428ebe74a9e8466a37bee7a9b11
[MoonPalace] 2024/07/29 21:30:53     - prompt_tokens:     1449
[MoonPalace] 2024/07/29 21:30:53     - completion_tokens: 158
[MoonPalace] 2024/07/29 21:30:53     - total_tokens:      1607
[MoonPalace] 2024/07/29 21:30:53   New Row Inserted: last_insert_id=15
```

MoonPalace will output the details of the request in the form of logs in the command line (if you want to persist the log content, you can redirect `stderr` to a file).

Note: In the logs, the value of the `Msh-Request-Id` field in the Response Headers corresponds to the `--requestid` parameter in the **Search Request** and **Export Request** sections below. The `id` in the Response corresponds to the `--chatcmpl` parameter, and `last_insert_id` corresponds to the `--id` parameter.

```shell theme={null}
[MoonPalace] 2024/08/05 19:06:19   it seems that your max_tokens value is too small, please set a larger value
```

If the current mode is non-streaming output (stream=False), MoonPalace will suggest an appropriate `max_tokens` value.

#### Enabling Repeated Content Output Detection

MoonPalace offers a feature to detect repeated content output from the Kimi large language model. Repeated content output refers to the model continuously outputting a specific word, sentence, or blank character without stopping before reaching the `max_tokens` limit. This can lead to additional Token costs when using more expensive models like `moonshot-v1-128k`. Therefore, MoonPalace provides the `--detect-repeat` option to enable repeated content output detection, as shown below:

```shell theme={null}
$ moonpalace start --port <PORT> --detect-repeat --repeat-threshold 0.3 --repeat-min-length 20
```

After enabling the `--detect-repeat` option, MoonPalace will interrupt the output of the Kimi large language model and log the following message when it detects repeated content:

```shell theme={null}
[MoonPalace] 2024/08/05 18:20:37   it appears that there is an issue with content repeating in the current response
```

*Note: The `--detect-repeat` option only interrupts the output in streaming mode (stream=True). It does not apply to non-streaming output.*

You can adjust MoonPalace's blocking behavior using the `--repeat-threshold` and `--repeat-min-length` parameters:

* The `--repeat-threshold` parameter sets MoonPalace's tolerance for repeated content. A higher threshold means lower tolerance, and repeated content will be blocked more quickly. The range is 0 {'<='} threshold {'<='} 1.
* The `--repeat-min-length` parameter sets the minimum number of characters before MoonPalace starts detecting repeated content. For example, --repeat-min-length=100 means that repeated content detection will only start when the output exceeds 100 UTF-8 characters.

#### Enabling Forced Streaming Output

MoonPalace provides the `--force-stream` option to force all `/v1/chat/completions` requests to use streaming output mode:

```shell theme={null}
$ moonpalace start --port <PORT> --force-stream
```

MoonPalace will set the `stream` field in the request parameters to `True`. When receiving a response, it will automatically determine the response format based on whether the caller has set `stream`:

* If the caller has set `stream=True`, the response will be returned in streaming format without any special handling by MoonPalace.
* If the caller has not set `stream` or has set `stream=False`, MoonPalace will concatenate all the streaming data chunks into a complete completion structure and return it to the caller after receiving all the data chunks.

For the caller (developer), enabling the `--force-stream` option will not affect the Kimi API response content you receive. You can still use your original code logic to debug and run your program. In other words, **enabling the `--force-stream` option will not change or break anything**. You can safely enable this option.

Why provide this option?

> We initially hypothesize that common network connection errors and timeouts (Connection Error/Timeout) occur because, in non-streaming request scenarios (stream=False), intermediate gateways or proxy servers may have set read\_header\_timeout or read\_timeout. This can cause the gateway or proxy server to disconnect while the Kimi API server is still assembling the response (since no response, or even the response header, has been received), resulting in Connection Error/Timeout.
>
> We added the `--force-stream` parameter to MoonPalace. When starting with `moonpalace start --force-stream`, MoonPalace converts all non-streaming requests (stream=False or unset) to streaming requests. After receiving all data chunks, it assembles them into a complete completion response structure and returns it to the caller.
>
> For the caller, you can still use the non-streaming API as before. However, after MoonPalace's conversion, it can reduce Connection Error/Timeout issues to some extent because MoonPalace has already established a connection with the Kimi API server and started receiving streaming data chunks.

### Retrieving Requests

After MoonPalace is started, all requests routed through MoonPalace are recorded in an sqlite database located at `$HOME/.moonpalace/moonpalace.sqlite`. You can directly connect to the MoonPalace database to query the specific content of the requests, or you can use the MoonPalace command-line tool to query the requests:

```shell theme={null}
$ moonpalace list
+----+--------+-------------------------------------------+--------------------------------------+---------------+---------------------+
| id | status | chatcmpl                                  | request_id                           | server_timing | requested_at        |
+----+--------+-------------------------------------------+--------------------------------------+---------------+---------------------+
| 15 | 200    | cmpl-12be8428ebe74a9e8466a37bee7a9b11     | c34f3421-4dae-11ef-b237-9620e33511ee | 7134          | 2024-07-29 21:30:53 |
| 14 | 200    | cmpl-1bf43a688a2b48eda80042583ff6fe7f     | c13280e0-4dae-11ef-9c01-debcfc72949d | 3479          | 2024-07-29 21:30:46 |
| 13 | 200    | chatcmpl-2e1aa823e2c94ebdad66450a0e6df088 | c07c118e-4dae-11ef-b423-62db244b9277 | 1033          | 2024-07-29 21:30:43 |
| 12 | 200    | cmpl-e7f984b5f80149c3adae46096a6f15c2     | 50d5686c-4d98-11ef-ba65-3613954e2587 | 774           | 2024-07-29 18:50:06 |
| 11 | 200    | chatcmpl-08f7d482b8434a869b001821cf0ee0d9 | 4c20f0a4-4d98-11ef-999a-928b67d58fa8 | 593           | 2024-07-29 18:49:58 |
| 10 | 200    | chatcmpl-6f3cf14db8e044c6bfd19689f6f66eb4 | 49f30295-4d98-11ef-95d0-7a2774525b85 | 738           | 2024-07-29 18:49:55 |
| 9  | 200    | cmpl-2a70a8c9c40e4bcc9564a5296a520431     | 7bd58976-4d8a-11ef-999a-928b67d58fa8 | 40488         | 2024-07-29 17:11:45 |
| 8  | 200    | chatcmpl-59887f868fc247a9a8da13cfbb15d04f | ceb375ea-4d7d-11ef-bd64-3aeb95b9dfac | 867           | 2024-07-29 15:40:21 |
| 7  | 200    | cmpl-36e5e21b1f544a80bf9ce3f8fc1fce57     | cd7f48d6-4d7d-11ef-999a-928b67d58fa8 | 794           | 2024-07-29 15:40:19 |
| 6  | 200    | cmpl-737d27673327465fb4827e3797abb1b3     | cc6613ac-4d7d-11ef-95d0-7a2774525b85 | 670           | 2024-07-29 15:40:17 |
+----+--------+-------------------------------------------+--------------------------------------+---------------+---------------------+
```

Use the `list` command to view the content of the most recent requests. By default, it displays fields that are easy to search, such as `id`/`chatcmpl`/`request_id`, as well as `status`/`server_timing`/`requested_at` for checking the request status. If you want to view a specific request, you can use the `inspect` command to retrieve it:

```shell theme={null}
# The following three commands will retrieve the same request information
$ moonpalace inspect --id 13
$ moonpalace inspect --chatcmpl chatcmpl-2e1aa823e2c94ebdad66450a0e6df088
$ moonpalace inspect --requestid c07c118e-4dae-11ef-b423-62db244b9277
+--------------------------------------------------------------+
| metadata                                                     |
+--------------------------------------------------------------+
| {                                                            |
|     "chatcmpl": "chatcmpl-2e1aa823e2c94ebdad66450a0e6df088", |
|     "content_type": "application/json",                      |
|     "group_id": "enterprise-tier-5",                         |
|     "moonpalace_id": "13",                                   |
|     "request_id": "c07c118e-4dae-11ef-b423-62db244b9277",    |
|     "requested_at": "2024-07-29 21:30:43",                   |
|     "server_timing": "1033",                                 |
|     "status": "200 OK",                                      |
|     "user_id": "cn0psmmcp7fclnphkcpg"                        |
| }                                                            |
+--------------------------------------------------------------+
```

By default, the `inspect` command does not print the body of the request and response. If you want to print the body, you can use the following command:

```shell theme={null}
$ moonpalace inspect --chatcmpl chatcmpl-2e1aa823e2c94ebdad66450a0e6df088 --print request_body,response_body
# Since the body information is too lengthy, the detailed content of the body is not shown here
+--------------------------------------------------+--------------------------------------------------+
| request_body                                     | response_body                                    |
+--------------------------------------------------+--------------------------------------------------+
| ...                                              | ...                                              |
+--------------------------------------------------+--------------------------------------------------+
```

### Exporting Requests

If you find that a request does not meet your expectations, or if you want to report a request to Moonshot AI (whether it's a Good Case or a Bad Case, we welcome both), you can use the `export` command to export a specific request:

```shell theme={null}
# You only need to choose one of the id/chatcmpl/requestid options to retrieve the corresponding request
$ moonpalace export \
    --id 13 \
    --chatcmpl chatcmpl-2e1aa823e2c94ebdad66450a0e6df088 \
    --requestid c07c118e-4dae-11ef-b423-62db244b9277 \
    --good/--bad \
    --tag "code" --tag "python" \
    --directory $HOME/Downloads/
```

Here, the usage of `id`/`chatcmpl`/`requestid` is the same as in the `inspect` command, used to retrieve a specific request. The `--good`/`--bad` options are used to mark the request as a Good Case or a Bad Case. The `--tag` option is used to add relevant tags to the request. For example, in the example above, we assume that the request is related to the Python programming language, so we add two tags: `code` and `python`. The `--directory` option specifies the path to the directory where the exported file will be saved.

The content of the successfully exported file is:

```shell theme={null}
$ cat $HOME/Downloads/chatcmpl-2e1aa823e2c94ebdad66450a0e6df088.json
{
    "metadata":
    {
        "chatcmpl": "chatcmpl-2e1aa823e2c94ebdad66450a0e6df088",
        "content_type": "application/json",
        "group_id": "enterprise-tier-5",
        "moonpalace_id": "13",
        "request_id": "c07c118e-4dae-11ef-b423-62db244b9277",
        "requested_at": "2024-07-29 21:30:43",
        "server_timing": "1033",
        "status": "200 OK",
        "user_id": "cn0psmmcp7fclnphkcpg"
    },
    "request":
    {
        "url": "https://api.moonshot.ai/v1/chat/completions",
        "header": "Accept: application/json\r\nAccept-Encoding: gzip\r\nConnection: keep-alive\r\nContent-Length: 2450\r\nContent-Type: application/json\r\nUser-Agent: OpenAI/Python 1.36.1\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Async: false\r\nX-Stainless-Lang: python\r\nX-Stainless-Os: MacOS\r\nX-Stainless-Package-Version: 1.36.1\r\nX-Stainless-Runtime: CPython\r\nX-Stainless-Runtime-Version: 3.11.6\r\n",
        "body":
        {}
    },
    "response":
    {
        "status": "200 OK",
        "header": "Content-Encoding: gzip\r\nContent-Type: application/json; charset=utf-8\r\nDate: Mon, 29 Jul 2024 13:30:43 GMT\r\nMsh-Cache: updated\r\nMsh-Gid: enterprise-tier-5\r\nMsh-Request-Id: c07c118e-4dae-11ef-b423-62db244b9277\r\nMsh-Trace-Mode: on\r\nMsh-Uid: cn0psmmcp7fclnphkcpg\r\nServer: nginx\r\nServer-Timing: inner; dur=1033\r\nStrict-Transport-Security: max-age=15724800; includeSubDomains\r\nVary: Accept-Encoding\r\nVary: Origin\r\n",
        "body":
        {}
    },
    "category": "goodcase",
    "tags":
    [
        "code",
        "python"
    ]
}
```

**We recommend that developers use [Github Issues](https://github.com/MoonshotAI/moonpalace/issues) to submit Good Cases or Bad Cases**, but if you do not want to make your request information public, you can also submit the Case to us via enterprise WeChat, email, or other means.

You can send the exported file to the following email address:

[api-feedback@moonshot.cn](mailto:api-feedback@moonshot.cn)
