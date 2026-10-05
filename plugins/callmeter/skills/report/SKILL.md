---
name: report
description: "Show callmeter reports: tool calls, files, commands, context, tokens, sessions, prompts, effort, agents, outcomes, coverage, lifecycle events, compactions, cost, Stop-hook overhead, turn durations, resumes and waiting on the user, recorded from Claude Code hooks and transcripts."
argument-hint: "{topic} [--since D] [--project P] [--agent-type T] [--session S] [--limit N] [--json]"
allowed-tools: Bash
---

# callmeter report

Show one callmeter report over the local store the hooks fill.

The topics are `files`, `writes`, `commands`, `context`, `sequences`, `faults`, `sessions`, `prompts`, `effort`, `tokens`, `agents`, `outcomes`, `coverage`, `events`, `compactions`, `cost`, `hooks`, `turns`, `resumes` and `waiting`. The flags are `--since D` (a duration such as `7d` or `24h`, or a date such as `2026-09-01`), `--project P`, `--agent-type T`, `--session S`, `--limit N` and `--json`.

## Steps

1. The user's arguments are: `$ARGUMENTS`
2. With no argument, list the 20 topics above with their flags and run nothing.
3. Otherwise run this with the Bash tool, the arguments passed through as given:

   ```
   "${CLAUDE_PLUGIN_ROOT}/libexec/callmeter" report $ARGUMENTS
   ```

4. Show its stdout unchanged.
5. On exit code 1 or 2, show its stderr verbatim and stop.

Never open, query or modify the store file directly: the report runs through the command above only. A `note:` line naming `callmeter redact` is for the user to act on: never run that command yourself.
