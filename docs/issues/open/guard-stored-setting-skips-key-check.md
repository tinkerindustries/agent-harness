# A stored guard setting skips the create-time key check on some runs

The Managed Agents `sessions.events` path and background-completion wakes start a run through `beginRun` without
a guard setting. They keep the stored one, as intended, but they skip the create-time `ANTHROPIC_API_KEY` check.
A session whose stored guard has no key in the process therefore fails the run, where a create would have been
refused.

Only `stdio-session` accepts a guard setting, so the path that can store one is the Responses dialect. Check
whether a wake in that dialect can reach the same state.
