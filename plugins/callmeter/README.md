# callmeter

**Claude Code plugin for tracking token usage and context growth — every tool call and sub-agent in local SQLite**

## What it records

Each tool call with its sanitized input, duration, failure, the bytes it produced and the bytes the model received, and the file it touched; each model request with its model and token split; each sub-agent and every one of its turns; each `Stop`; each session's start, end, model and seat; every lifecycle event (prompts by size only, compactions, instructions loaded, stop failures, permission requests, tasks); and every event it failed to record. No prompt text, message text or file content is stored, only byte counts.

## Reports

```text
/callmeter:report {topic} [--since D] [--project P] [--agent-type T] [--session S] [--limit N] [--json]
```

Topics: `files`, `writes`, `commands`, `context`, `sequences`, `faults`, `sessions`, `prompts`, `effort`, `tokens`, `agents`, `outcomes`, `coverage`, `events`.

## Where the store lives

`${CALLMETER_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/callmeter}/callmeter.db`: one store per machine, shared by every Claude Code config dir, never the plugin's per-seat data directory. The binary is downloaded on first use into `bin/` beside it and checked against `libexec/SHA256SUMS`.

Source, design and issues: [github.com/rezzminator/callmeter](https://github.com/rezzminator/callmeter).

Built and maintained with [Professor](https://github.com/rezzminator/professor).
