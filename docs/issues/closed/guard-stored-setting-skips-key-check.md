# A stored guard setting skips the create-time key check on some runs

The Managed Agents `sessions.events` path and background-completion wakes start a run through `beginRun` without
a guard setting. They keep the stored one, as intended, but they skip the create-time `ANTHROPIC_API_KEY` check.
A session whose stored guard has no key in the process therefore fails the run, where a create would have been
refused.

Only `stdio-session` accepts a guard setting, so the path that can store one is the Responses dialect. Check
whether a wake in that dialect can reach the same state.

## Resolution

Not reachable. `cmd/harness` reads `ANTHROPIC_API_KEY` once at process start, so the key a create was checked
against is the key every later run in that process sees. A background wake or a Managed Agents `sessions.events`
turn only starts a run for a session this process already holds, and that session passed the check when it was
created or resumed. A session whose stored setting is active cannot be created or resumed under `claude-session` or
`gemini-session`, because `resolveGuard` refuses it there.
