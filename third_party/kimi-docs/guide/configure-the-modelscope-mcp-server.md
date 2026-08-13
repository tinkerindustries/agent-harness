---
title: Configure ModelScope MCP Server in Playground
source: https://platform.kimi.ai/docs/guide/configure-the-modelscope-mcp-server
fetched: 2026-08-13
---

# Configure ModelScope MCP Server in Playground

Through an official partnership between the Kimi API Platform and ModelScope, you can sync all hosted MCP service configurations under your ModelScope account into Kimi Playground with a single API token. Follow this page when you want models in Playground to call MCP tools.

## Sync ModelScope-Hosted MCP Services

Log in to Kimi Playground ([https://platform.kimi.ai/playground](https://platform.kimi.ai/playground)) and make sure you can have basic conversations with the Kimi K2 model.

MCP services are added in "MCP Server Settings", where ModelScope is selected as the default MCP service provider. If you haven't used the ModelScope MCP marketplace before, first refer to the [ModelScope official documentation](https://modelscope.cn/mcp/kimi-playground) to select and host your MCP services; you can also discover numerous MCP servers in the ModelScope community.

### Open MCP Server Settings

Click the configuration button to open "MCP Server Settings":

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/config-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=698d460d6b8f4bfcf7ae4e370b10fa08" alt="mcp-server-setting" width="1872" height="1786" data-path="assets/pics/modelscope/config-en.png" />

### Enter Your ModelScope API Token and Sync

In the panel that appears, choose to sync with the external platform:

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/syc-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=f2a2d3477ec9438b9756234bf4fc327f" alt="syc" width="1934" height="1806" data-path="assets/pics/modelscope/syc-en.png" />

You can obtain the API token from the [ModelScope Homepage - Access Token](https://modelscope.cn/my/myaccesstoken) page:

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/get-keys-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=63b9ba39f0f7169a6e1c4bf5084ababe" alt="keys" width="1906" height="1796" data-path="assets/pics/modelscope/get-keys-en.png" />

Paste the token into the field in Step 3 and click "Start Sync":

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/start-syc-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=186f9650a2485d22afa8a22da4764232" alt="start-syc" width="1900" height="1784" data-path="assets/pics/modelscope/start-syc-en.png" />

Once syncing completes, all configured and connected ModelScope Hosted MCP services appear in the available MCP services list in Kimi Playground:

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/mcp-list-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=d4b27ecfeaabffb6eec332586fc25911" alt="mcp-list" width="1896" height="1790" data-path="assets/pics/modelscope/mcp-list-en.png" />

### Incrementally Sync MCP Services

If you later add or remove hosted MCP services in the ModelScope MCP marketplace, click the sync button in "Settings - MCP Server - Sync Server" to perform an incremental update:

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/add-mcp-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=a11227134a055c1301c855af7d8a9dfe" alt="add-mcp" width="1912" height="1804" data-path="assets/pics/modelscope/add-mcp-en.png" />

## Enable MCP Services in a Conversation

After syncing, the imported "MCP Services List" appears on the left side of the Kimi Playground page. Multi-select and enable the MCP services you want to use in the current conversation:

<img src="https://mintcdn.com/moonshotai/dink2O4VJx7ks4ld/assets/pics/modelscope/manage-mcp-en.png?fit=max&auto=format&n=dink2O4VJx7ks4ld&q=85&s=542cb613831a37e780deeeaaeb2fa3b2" alt="manage-mcp" width="2132" height="1824" data-path="assets/pics/modelscope/manage-mcp-en.png" />
