---
title: Use Kimi in Claude Code
source: https://platform.kimi.ai/docs/guide/claude-code-kimi
fetched: 2026-08-13
---

# Use Kimi in Claude Code

> [Claude Code](https://claude.com/product/claude-code) is a programming agent product from Anthropic. Its interface, configuration options, and supported capabilities may change across versions. This guide describes a general integration approach: forwarding Claude Code model requests to the Kimi API through environment variables.

## Install Claude Code

Skip this step if Claude Code is already installed. Run:

```shell theme={null}
npm install -g @anthropic-ai/claude-code
```

<Accordion title="No Node.js yet? Expand for installation and initialization">
  macOS and Linux:

  ```shell theme={null}
  # Install Node.js
  curl -fsSL https://fnm.vercel.app/install | bash

  # Open a new terminal so fnm takes effect
  fnm install 24.3.0
  fnm default 24.3.0
  fnm use 24.3.0
  ```

  Windows (PowerShell):

  ```powershell theme={null}
  # Right-click the Windows button, click "Terminal", then run:
  winget install OpenJS.NodeJS
  Set-ExecutionPolicy -Scope CurrentUser RemoteSigned

  # Close the terminal window and open a new one
  ```

  After installing Node.js, run the initialization once:

  ```shell theme={null}
  node --eval "
      const fs = require('fs');
      const path = require('path');
      const os = require('os');
      const homeDir = os.homedir();
      const filePath = path.join(homeDir, '.claude.json');
      if (fs.existsSync(filePath)) {
          const content = JSON.parse(fs.readFileSync(filePath, 'utf-8'));
          fs.writeFileSync(filePath, JSON.stringify({ ...content, hasCompletedOnboarding: true }, null, 2), 'utf-8');
      } else {
          fs.writeFileSync(filePath, JSON.stringify({ hasCompletedOnboarding: true }, null, 2), 'utf-8');
      }"
  ```
</Accordion>

<Accordion title="Not a fresh install? Clean up legacy configuration and environment variables">
  If you previously modified `~/.claude/settings.json` through third-party tools or by hand, stale entries in its `env` field **override** environment variables exported in your terminal, which can silently prevent the new configuration from taking effect or rewrite model requests. Run the following script to clean them up first:

  ```shell theme={null}
  node --eval "
      const fs = require('fs');
      const path = require('path');
      const os = require('os');
      const settingsPath = path.join(os.homedir(), '.claude', 'settings.json');
      if (fs.existsSync(settingsPath)) {
          const content = JSON.parse(fs.readFileSync(settingsPath, 'utf-8'));
          if (content && typeof content === 'object' && content.env && typeof content.env === 'object') {
              for (const key of [
                  'ANTHROPIC_BASE_URL',
                  'ANTHROPIC_API_KEY',
                  'ANTHROPIC_AUTH_TOKEN',
                  'ANTHROPIC_MODEL',
                  'ANTHROPIC_SMALL_FAST_MODEL',
                  'CLAUDE_CODE_SUBAGENT_MODEL',
                  'ANTHROPIC_DEFAULT_OPUS_MODEL',
                  'ANTHROPIC_DEFAULT_OPUS_MODEL_NAME',
                  'ANTHROPIC_DEFAULT_SONNET_MODEL',
                  'ANTHROPIC_DEFAULT_SONNET_MODEL_NAME',
                  'ANTHROPIC_DEFAULT_HAIKU_MODEL',
                  'ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME',
                  'ANTHROPIC_DEFAULT_FABLE_MODEL',
                  'ANTHROPIC_DEFAULT_FABLE_MODEL_NAME',
                  'ENABLE_TOOL_SEARCH',
                  'CLAUDE_CODE_AUTO_COMPACT_WINDOW',
                  'CLAUDE_CODE_EFFORT_LEVEL',
              ]) {
                  delete content.env[key];
              }
              fs.writeFileSync(settingsPath, JSON.stringify(content, null, 2), 'utf-8');
          }
      }"
  ```

  This script only removes endpoint, credential, and model-related variables from `env`; other settings (such as permissions and theme) are left untouched.

  Also check shell configuration files such as `~/.zshrc` and `~/.bashrc` for stale `ANTHROPIC_*` exports (on Windows, check your user environment variables) and remove any you find — they interfere with the new configuration just as much.
</Accordion>

## Get a Kimi API Key

Visit [Kimi Open Platform](https://platform.kimi.ai/console/api-keys) to create an API key (choose the default project), and use it in place of `YOUR_MOONSHOT_API_KEY` below.

## Configure Environment Variables

Choose **one of the two methods below and do not mix them**: Method 1 takes effect immediately but only lasts for the current terminal session; Method 2 writes to a configuration file and persists.

### Method 1: Terminal Environment Variables (Current Session Only)

macOS and Linux:

```shell theme={null}
export ANTHROPIC_BASE_URL="https://api.moonshot.ai/anthropic"
export ANTHROPIC_AUTH_TOKEN="${YOUR_MOONSHOT_API_KEY}"
export ANTHROPIC_MODEL="kimi-k3[1m]"
export ANTHROPIC_DEFAULT_OPUS_MODEL="kimi-k3[1m]"
export ANTHROPIC_DEFAULT_SONNET_MODEL="kimi-k3[1m]"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="kimi-k3[1m]"
export ANTHROPIC_DEFAULT_FABLE_MODEL="kimi-k3[1m]"
export CLAUDE_CODE_SUBAGENT_MODEL="kimi-k3[1m]"
export CLAUDE_CODE_AUTO_COMPACT_WINDOW="1048576"
export CLAUDE_CODE_EFFORT_LEVEL="max"
claude
```

Windows (PowerShell):

```powershell theme={null}
$env:ANTHROPIC_BASE_URL="https://api.moonshot.ai/anthropic";
$env:ANTHROPIC_AUTH_TOKEN="YOUR_MOONSHOT_API_KEY"
$env:ANTHROPIC_MODEL="kimi-k3[1m]"
$env:ANTHROPIC_DEFAULT_OPUS_MODEL="kimi-k3[1m]"
$env:ANTHROPIC_DEFAULT_SONNET_MODEL="kimi-k3[1m]"
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL="kimi-k3[1m]"
$env:ANTHROPIC_DEFAULT_FABLE_MODEL="kimi-k3[1m]"
$env:CLAUDE_CODE_SUBAGENT_MODEL="kimi-k3[1m]"
$env:CLAUDE_CODE_AUTO_COMPACT_WINDOW="1048576"
$env:CLAUDE_CODE_EFFORT_LEVEL="max"
claude
```

### Method 2: Write To settings.json (Persistent)

Write the same variables into the `env` field of `~/.claude/settings.json`:

```json theme={null}
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.moonshot.ai/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "YOUR_MOONSHOT_API_KEY",
    "ANTHROPIC_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "kimi-k3[1m]",
    "CLAUDE_CODE_SUBAGENT_MODEL": "kimi-k3[1m]",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_EFFORT_LEVEL": "max"
  }
}
```

Note: `env` values in `settings.json` **override** variables exported in your terminal; this file contains your API key in plaintext — do not commit it to a git repository; restart Claude Code after saving.

### Configuration Reference

Claude Code uses different model tiers for different scenarios (main conversation, background summarization, sub-agents, and so on). Configuring only some of the variables makes the corresponding scenarios fail silently:

| Variable                                                                                                                              | Purpose                                                         | Impact if missing or misconfigured                                                                                                                                                           |
| ------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ANTHROPIC_BASE_URL`                                                                                                                  | Forwards model requests to Kimi's Anthropic-compatible endpoint | Requests go to Anthropic's official endpoint and fail authentication                                                                                                                         |
| `ANTHROPIC_AUTH_TOKEN`                                                                                                                | Authenticates with your Kimi API key                            | Returns 401 authentication errors                                                                                                                                                            |
| `ANTHROPIC_MODEL`                                                                                                                     | Model used for the main conversation                            | Falls back to a default Claude model name that the Kimi endpoint cannot recognize, resulting in model-not-found errors                                                                       |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` / `ANTHROPIC_DEFAULT_SONNET_MODEL` / `ANTHROPIC_DEFAULT_HAIKU_MODEL` / `ANTHROPIC_DEFAULT_FABLE_MODEL` | Model names Claude Code uses when selecting models by task tier | Tasks on the corresponding tier (e.g. background title generation and summarization on the haiku tier) fail                                                                                  |
| `CLAUDE_CODE_SUBAGENT_MODEL`                                                                                                          | Model used by sub-agents                                        | Sub-agent tasks fail or degrade noticeably                                                                                                                                                   |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW`                                                                                                     | Context window size that triggers automatic compaction          | Must match the model's context: `1048576` for `kimi-k3` (1M), `262144` for `kimi-k2.7-code` (256K). Too small compacts prematurely and loses context; too large causes context-length errors |
| `CLAUDE_CODE_EFFORT_LEVEL`                                                                                                            | Controls Claude Code's reasoning effort                         | Set to `max` to enable the most thorough reasoning; lower values may reduce quality for complex tasks                                                                                        |

## Models And Thinking Behavior

How the three models actually behave in Claude Code (verified against the Anthropic-compatible endpoint):

| Model                            | Thinking      | Notes                                                                                                                                                                                                |
| -------------------------------- | ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kimi-k3` (default on this page) | On by default | Works out of the box, no extra configuration needed                                                                                                                                                  |
| `kimi-k2.7-code`                 | Always on     | Requests must explicitly enable thinking — keep Thinking on (press `Tab`) in Claude Code. With thinking off, requests are rejected (`invalid thinking: only type=enabled is allowed for this model`) |
| `kimi-k2.6`                      | Optional      | Can run with thinking off; good for latency-sensitive simple tasks                                                                                                                                   |

When switching models, replace the value of every model variable in the configuration with the new model name.

## Confirm That The Configuration Took Effect

In Claude Code, enter `/status` to confirm the configuration:

* Base URL should show `https://api.moonshot.ai/anthropic`
* Model should show `kimi-k3[1m]`

Claude Code's `/model` menu is a built-in list of fixed aliases — it **does not show Kimi models**, and you do not need to switch anything there. Whether the configuration took effect is determined by what `/status` shows.

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/cline/status.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=051f6e987302797e4bde2ce4fc277cab" alt="status" width="1138" height="458" data-path="assets/pics/cline/status.png" />

Finally, send any message (for example `hi`). Receiving a normal reply confirms the end-to-end setup works.

## Turn On Thinking

`kimi-k3` thinks by default and works out of the box. If you switch to `kimi-k2.7-code`, it requires requests to explicitly enable thinking: press `Tab` in Claude Code to turn Thinking on and start working once you see the "Thinking on" indicator — otherwise the model rejects requests (`400 invalid thinking`), and features such as WebSearch do not work.

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/cline/thinking-on.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=acf616ab4147291f80c8713a1a35bb5e" alt="thinking-on" width="1140" height="656" data-path="assets/pics/cline/thinking-on.png" />

You can now use Claude Code for development normally.

## Switch To The High-Speed Model

Kimi K2.7 Code offers a high-speed variant, `kimi-k2.7-code-highspeed`, with an output speed about 5-6x that of the regular version. If you prioritize output speed, change the value of every model variable in the configuration to `kimi-k2.7-code-highspeed` (note that it requires explicitly enabled thinking — see Models And Thinking Behavior). See [K2.7 Code pricing](../pricing/chat-k27-code.md) for details.

## Third-Party Tool: cc-switch

Community tools such as cc-switch can switch between multiple provider configurations. These tools are not maintained by Kimi, and their presets may differ from the values recommended on this page. After using them, verify each variable against the Configuration Reference above, and use `/status` to confirm the Base URL and model actually in effect.

## FAQ

<AccordionGroup>
  <Accordion title="WebSearch fails with 400 invalid thinking / WebFetch unavailable">
    * **WebSearch fails with `400 invalid thinking: only type=enabled is allowed for this model`**: `kimi-k2.7-code` requires thinking, and WebSearch requests without it are rejected by the platform. Press `Tab` to turn Thinking on first, then retry; if it still fails, switch to `kimi-k2.6` (thinking optional, not subject to this restriction). `kimi-k3` is not affected. This is unrelated to your local configuration or cc-switch.
    * **WebFetch reports `temporarily unavailable` or returns no fetched content**: the endpoint does not support WebFetch for now — unrelated to your configuration; it will work once the platform adds support. As a workaround, paste the web page content into the chat, or use an MCP scraping tool instead.
  </Accordion>

  <Accordion title="401 authentication errors">
    Check that `ANTHROPIC_AUTH_TOKEN` is a valid Kimi API key. If you previously configured `ANTHROPIC_API_KEY`, remove it to avoid conflicts with `ANTHROPIC_AUTH_TOKEN` when both are present.
  </Accordion>

  <Accordion title="Model not found">
    Check the spelling of every model variable (`kimi-k3[1m]`), and make sure there are no extra spaces or quotes.
  </Accordion>

  <Accordion title="Background tasks or sub-agents fail">
    This usually means `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `ANTHROPIC_DEFAULT_FABLE_MODEL`, or `CLAUDE_CODE_SUBAGENT_MODEL` is not set, so those scenarios requested a model name the Kimi endpoint cannot recognize. Fill in the missing variables per the Configuration Reference above.
  </Accordion>

  <Accordion title="Configuration changes do not take effect">
    * Check for stale entries in the `env` field of `~/.claude/settings.json` (they override terminal environment variables); you can run the cleanup script in the collapsed section above;
    * Variables exported in a terminal only apply to the current session and must be set again in a new window. If you appended them to `~/.zshrc` or used the settings.json method, make sure you restarted Claude Code after the change.
  </Accordion>

  <Accordion title="API key and endpoint mismatch">
    Make sure `ANTHROPIC_BASE_URL` matches the platform where you created the API key — create the key on the platform linked in "Get a Kimi API Key" above and use the endpoint shown on this page.
  </Accordion>

  <Accordion title="Previously logged in with /login">
    Once set, `ANTHROPIC_AUTH_TOKEN` takes precedence over a saved claude.ai login, so no action is usually needed. Run `/status` in a session to confirm which credential source is active; to remove the saved login, run `/logout`.
  </Accordion>
</AccordionGroup>
