# claude-session accepts a custom tool result for an id it never declared

`Server.customReady` stashes a `user.custom_tool_result` that arrives before its call has
registered. Within that window the server can't tell an early answer from an id it never
issued. A bogus id is accepted instead of refused with `-32002`, as
`docs/STDIO-MANAGED-AGENTS.md` specifies.

Fixing it needs a registry that knows every `custom_tool_use_id` it has emitted, separately
from the ones it is waiting on.
