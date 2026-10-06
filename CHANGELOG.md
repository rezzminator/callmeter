# Changelog

Every release of callmeter. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release carrying the section below and the four platform binaries.

## [0.2.0] — 2026-10-06

### Added
- A `callmeter` skill (`/callmeter:callmeter`, loaded by the model on its own): how to read the store directly with SQL or Python. It covers where the store lives, safe reads (read-only, a busy timeout, a `.backup` snapshot for long work, never a plain copy or `immutable=1`, the archive without double counting), the units and NULL meanings that silently mislead (millisecond UTC times, the main chat as `agent_id IS NULL`, pending requests, agent totals written only at a recorded stop), what `calls.input` keeps of each tool, that a report run prunes and so runs only when the user asks, recipes checked against a real store (per-agent spend and peak context, spawn to agent, an agent's descendant tree, one agent's work, the tools that fill the context, compactions, cost per session and model, agents with no recorded stop, turns with their wall time), Python access, and every table's columns in `schema.md`. Built from the frictions of earlier chats that queried the store by hand. `/callmeter:report` now sends a question no topic answers to it.
- Five metrics read from Claude Code's own transcript entries and stored: the thinking part of each request's output tokens (`requests.thinking_tokens`, NULL when the transcript carried no split), every compaction (`compactions`), Claude Code's latest cost snapshot per session (`session_costs`), the Stop-hook runs of each turn (`stop_hooks`, `stop_hook_runs`: a derived hook name, never the command, and no duration for an async hook) and each turn's wall time (`turn_durations`).
- Six report topics: `compactions`, `cost`, `hooks`, `turns`, `resumes` (the cost of resuming a session cold, from `SessionStart` events) and `waiting` (time waiting on the user, from `Notification` events); `--agent-type` does not apply to them and each says so. `callmeter report` now has 20 topics, in text and `--json`.
- `tokens` gains `THINKING` and `THINK %`, and `prompts` gains `WALL S`; each shows `-` where nothing was recorded, never 0.
- An existing store gains the new columns, tables and indexes on its next open, once and best effort; the schema version stays 1, so an older binary keeps opening the store. A store that is busy at that moment opens without them, its topics over them say so, and a later open adds them.

### Fixed
- Hooks no longer lose events to a busy store on long sessions. The Stop hook re-counted every request of its session by scanning all of `calls`, so it held the store's write lock for seconds and concurrent hooks gave up after their 5 s wait. `calls` gains an index on `request_id`, added once to an existing store on its next open; the schema version is unchanged.
- A prompt-type Stop hook no longer stores a word of its prompt: its run was named from the first word of the prompt text (`stop_hook_runs.name` = `I`). Such a hook, and a command that reads as prose, is now `prompt#` and a hash; a command hook is named only from a path or a known interpreter, never a bare word. `callmeter redact` rewrites every stored name the new rule could not produce, reported as `stop_hook_runs.name`.

## [0.1.0] — 2026-10-03

### Added
- callmeter as a standalone Claude Code plugin, ported from Professor's built-in recorder: the hook, the store, the Bash command parser (shell through `mvdan.cc/sh`, Python through its own `ast`) and the `files`, `writes`, `commands`, `context`, `sequences` and `faults` reports, unchanged in what they answer.
- A fresh store at `CALLMETER_HOME` (default `~/.local/state/callmeter/callmeter.db`), one per machine across every Claude Code config dir, schema version 1, with a 30-day prune that archives every pruned row to `archive.db`.
- The plugin and its wrapper: one hook registration per recorded event, a POSIX sh wrapper that downloads the release binary on first use, verifies it against the committed `SHA256SUMS`, caches it per version, and logs any hook it could not serve to `missed.log` for the next run to count.
- New tables: `turns`, `events`, `agent_turns` and `sessions`, and new columns on calls, requests and agents: prompt id, effort, permission mode, interrupts, the request token split by cache TTL, lines added and removed, commits and test runners. Prompts and messages are stored as byte counts only.
- New reports: `sessions`, `prompts`, `effort`, `tokens`, `agents`, `outcomes`, `coverage` and `events`, and `/callmeter:report` inside Claude Code.
- `--json` on every report: one object with the topic, columns, rows and notes.
