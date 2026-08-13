---
title: Use Kimi K3 in Codex CLI
source: https://platform.kimi.ai/docs/guide/codex-kimi
fetched: 2026-08-13
---

# Use Kimi K3 in Codex CLI

This guide explains how to connect Codex CLI to Kimi Open Platform through CC Switch and use the `kimi-k3` model.

<Note>
  Codex CLI currently supports text and image input, but does not provide a native video input channel — you cannot submit a video file directly as multimodal input to the model. To analyze video using only Codex CLI's existing input methods, you can first extract key frames with ffmpeg and optionally combine them with audio transcription before handing them to the model. Note that this is a limitation of Codex CLI's input layer, not of the Kimi K3 model — the Kimi K3 API natively supports video input. Call `kimi-k3` directly as described in [Vision Input](use-kimi-vision-model.md) for full video understanding, with no manual frame extraction required.
</Note>

## Prerequisites

Complete the following preparations first. Follow the corresponding official instructions for installation and account-related operations; this guide does not repeat those procedures.

<CardGroup cols={3}>
  <Card title="Install Codex CLI" icon="terminal" href="https://developers.openai.com/codex/cli">
    Follow the official Codex documentation and start Codex CLI at least once.
  </Card>

  <Card title="Create an API key" icon="key" href="https://platform.kimi.ai/console/api-keys">
    Create and save an API key in Kimi Open Platform.
  </Card>

  <Card title="Install CC Switch" icon="download" href="https://ccswitch.io/en/docs?section=getting-started&item=installation">
    Follow the official CC Switch instructions to install the version for your operating system.
  </Card>
</CardGroup>

<Note>
  CC Switch is a third-party open-source tool and is not part of Kimi Open Platform. Before using it, evaluate it according to your organization's security and compliance requirements; your API key and Codex requests and responses will be processed by its local router.
</Note>

After installation, open CC Switch and follow the steps below.

## Step 1: Enable Codex Routing

In CC Switch, open **Settings > Routing**, then:

1. Turn on **Routing Master Switch** to start the local routing service.
2. Under **Routing Enabled**, turn on **Codex**.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/enable-codex-routing.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=1b8e790a022ac6aa1a8461ecfd1f80c2" alt="Enable CC Switch Local Routing and Codex routing" width="2704" height="1698" data-path="assets/pics/codex-kimi/enable-codex-routing.png" />

<Note>
  Codex CLI uses the Responses API, while Kimi Open Platform provides an OpenAI-compatible Chat Completions API. CC Switch Local Routing converts requests and streaming responses between the two protocols. Keep CC Switch and Codex routing running while using the Kimi provider.
</Note>

## Step 2: Add the Kimi Provider

1. Return to the CC Switch home screen and select the **Codex** tab at the top.
2. Click **+** in the upper-right corner to add a provider.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/open-codex-provider-list.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=f20cc6e4d74219608c21359052f60b38" alt="Open the Codex tab and add a provider" width="2704" height="1697" data-path="assets/pics/codex-kimi/open-codex-provider-list.png" />

3. Confirm that you are on the **Codex Provider** page. Search for Kimi if needed, then select **Kimi** from the preset provider list.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/select-kimi-provider.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=d056b58e17011e92ebf13eef250850ad" alt="Select the Kimi provider preset" width="2704" height="1697" data-path="assets/pics/codex-kimi/select-kimi-provider.png" />

4. Enter the following settings:

| Setting                    | Value                                     |
| -------------------------- | ----------------------------------------- |
| API request URL (Base URL) | `https://api.moonshot.ai/v1`              |
| API key                    | The API key created in Kimi Open Platform |
| Default model              | `kimi-k3`                                 |

5. After changing the default model to `kimi-k3`, click **Add to mapping**.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/configure-kimi-provider.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=8abd9e56fed3be05cb1df6211bb0e85c" alt="Enter the Kimi API key, request URL, and default model" width="2704" height="1698" data-path="assets/pics/codex-kimi/configure-kimi-provider.png" />

6. Scroll down and confirm the following advanced settings:

| Setting                   | Value                                 |
| ------------------------- | ------------------------------------- |
| Upstream format           | `Chat Completions (routing required)` |
| Prompt cache routing      | `Auto (recommended)`                  |
| Supports thinking mode    | On                                    |
| Supports reasoning effort | On                                    |
| Menu display name         | `kimi-k3`                             |
| Actual request model      | `kimi-k3`                             |
| Context window            | `1048576`                             |

7. After confirming the settings, click **Add** in the lower-right corner.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/configure-kimi-model-mapping.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=17f26f9a7a2ee4b697447a687183cac2" alt="Configure the Kimi upstream format, reasoning capabilities, and model mapping" width="2704" height="1698" data-path="assets/pics/codex-kimi/configure-kimi-model-mapping.png" />

## Step 3: Enable the Kimi Provider

Return to the Codex provider list after adding the provider and click **Enable** on the Kimi provider you just added.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/enable-kimi-provider.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=664f1463fd93fb0717d6722eeaf5d939" alt="Enable the Kimi provider" width="2704" height="1697" data-path="assets/pics/codex-kimi/enable-kimi-provider.png" />

Confirm that:

* Kimi is the active provider under the Codex tab;
* CC Switch Local Routing is running;
* Codex is enabled under the routing settings.

## Step 4: Start Codex CLI

If Codex CLI is already running, exit the current session. Enter the project directory where you want to work and start Codex CLI again:

```bash theme={null}
cd /path/to/your/project
codex
```

Restarting allows Codex CLI to load the latest provider and model configuration written by CC Switch.

After startup, confirm that Codex CLI shows `kimi-k3` as the current model, then send a simple request:

```text theme={null}
hello
```

If Codex CLI returns a response and the status bar shows `kimi-k3`, the configuration is working.

<img src="https://mintcdn.com/moonshotai/7fdJgPBk53qHhnI4/assets/pics/codex-kimi/verify-codex-cli.png?fit=max&auto=format&n=7fdJgPBk53qHhnI4&q=85&s=0be42db37cca1137aa03df902d6bf8e3" alt="Verify kimi-k3 in Codex CLI" width="2704" height="1698" data-path="assets/pics/codex-kimi/verify-codex-cli.png" />

You can also check the CC Switch routing counter or request logs for a new Codex request.
