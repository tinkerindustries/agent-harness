# claude-session refuses a create with no initial events

`sessions.create` with an empty `initial_events` answers `-32004`. Anthropic's surface allows
creating an idle session and posting the first `user.message` later.

Building it needs `internal/stdiosession`'s `Server` to hold a session's create-time facts
(cwd, model, tools, permission mode) before any run exists. Today every create path mints a run
immediately. `docs/STDIO-MANAGED-AGENTS.md` records this as a deviation.
