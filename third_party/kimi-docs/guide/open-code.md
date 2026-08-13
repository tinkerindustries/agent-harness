---
title: Use Kimi Models in OpenCode
source: https://platform.kimi.ai/docs/guide/open-code
fetched: 2026-08-13
---

# Use Kimi Models in OpenCode

> Install OpenCode, connect it to the Kimi Open Platform through built-in authentication, and use Kimi K3 with its thinking-effort variants.

[OpenCode](https://opencode.ai/) is an open-source programming agent. This guide shows how to connect OpenCode to the Kimi Open Platform through built-in authentication and use the `kimi-k3` model with its 1M-token context window.

<Note>
  This guide is based on OpenCode 1.18.3. Its interface, configuration options, and supported capabilities may change between versions.
</Note>

## Prerequisites

Complete these prerequisites before you start. Follow the linked official guides for installation and account setup.

<CardGroup cols={3}>
  <Card title="Install OpenCode" icon="terminal" href="https://opencode.ai/docs/">
    Install or update OpenCode with the official documentation.
  </Card>

  <Card title="Create an API key" icon="key" href="https://platform.kimi.ai/console/api-keys">
    Create a Kimi Open Platform API key and keep it private.
  </Card>

  <Card title="Check account settings" icon="gauge" href="/docs/guide/account-and-payments">
    Confirm that the account has available balance, then check limits, budgets, and organization settings.
  </Card>
</CardGroup>

Kimi K3 requires available balance in your account; vouchers granted from new-user verification cannot be used for Kimi K3. Rate limits vary by user tier. See [Rate Limits](../pricing/limits.md). If your organization uses an IP whitelist, follow [Organization Best Practices](org-best-practice.md) and add the public egress IPv4 address of the computer that calls the API.

## Step 1: Configure the API key

Run `opencode auth login` and select **Moonshot AI** in the provider list:

```text theme={null}
$ opencode auth login
┌  Add credential
│
◆  Select provider
│  Search: Moon█ (2 matches)
│  ● Moonshot AI
│  ↑/↓ to select • Enter: confirm • Type: to search
└
```

Then paste your Kimi Open Platform API key and press `Enter`:

```text theme={null}
$ opencode auth login
┌  Add credential
│
◇  Select provider
│  Moonshot AI
│
◇  Enter your API key
│  ▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪▪
│
└  Done
```

<Warning>
  Do not put the API key in configuration files, screenshots, or a Git repository. This guide uses a Global Kimi Open Platform key; do not substitute a key from another Kimi service or region.
</Warning>

## Step 2: Select the Kimi K3 model

Run `opencode` to start OpenCode:

```bash theme={null}
opencode
```

Run the `/models` command in the input field:

<img src="https://mintcdn.com/moonshotai/gDhPv8C9OsSiwQmv/assets/pics/opencode/models-command.png?fit=max&auto=format&n=gDhPv8C9OsSiwQmv&q=85&s=746dbb26a7cbfbce1efcb5705fbbf198" alt="Run the /models command" width="1342" height="304" data-path="assets/pics/opencode/models-command.png" />

Search for and select **Kimi K3** in the Select model dialog:

<img src="https://mintcdn.com/moonshotai/gDhPv8C9OsSiwQmv/assets/pics/opencode/select-model.png?fit=max&auto=format&n=gDhPv8C9OsSiwQmv&q=85&s=d40d1b661ad5e7740b7e60d4de166207" alt="Select the Kimi K3 model" width="990" height="187" data-path="assets/pics/opencode/select-model.png" />

## Step 3: Adjust the reasoning effort

Run the `/variants` command in the input field:

<img src="https://mintcdn.com/moonshotai/gDhPv8C9OsSiwQmv/assets/pics/opencode/variants-command.png?fit=max&auto=format&n=gDhPv8C9OsSiwQmv&q=85&s=2965e82cf4c6c14ac67b42e7d287b01e" alt="Run the /variants command" width="1327" height="305" data-path="assets/pics/opencode/variants-command.png" />

Select **max** in the Select variant dialog:

<img src="https://mintcdn.com/moonshotai/gDhPv8C9OsSiwQmv/assets/pics/opencode/select-variant-max.png?fit=max&auto=format&n=gDhPv8C9OsSiwQmv&q=85&s=350bef5d3f0fe8a9d09ac739912637cc" alt="Select the max variant" width="1005" height="249" data-path="assets/pics/opencode/select-variant-max.png" />

`kimi-k3` defaults to `max` reasoning effort and can also switch to `low` / `high`. See the [Model Parameter Reference](../api/models-overview.md).

When setup is complete, the status bar should show **Kimi K3**, **Moonshot AI**, and **max**:

<img src="https://mintcdn.com/moonshotai/gDhPv8C9OsSiwQmv/assets/pics/opencode/final-config.jpg?fit=max&auto=format&n=gDhPv8C9OsSiwQmv&q=85&s=fd9615f2304b054f3bedc56656d97a30" alt="Final configuration" width="1619" height="518" data-path="assets/pics/opencode/final-config.jpg" />

## Learn more

* [OpenCode documentation](https://opencode.ai/docs)
* [Kimi Open Platform quickstart](../overview.md)
