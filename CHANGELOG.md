# Changelog

Every release of callmeter. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release carrying the section below and the four platform binaries.

## [Unreleased]

### Added
- `request_iterations`: each entry of a request's transcript usage `iterations` whose type is not `message` (such as `fallback_message`, a fallback model's answer), with its own token counts, written wherever the request is and carried through prune and `archive.db`. A request answered by a fallback model keeps the answering attempt's token columns, never a sum.
- A `request_cache` view, computed when read: each request's gap since the previous request of its main chat or sub-agent, the TTL of the cache it could read, the read a full hit would make, the outcome (`first`, `hit`, `partial`, `miss`) and, for a partial or miss, the cause the stored columns prove (`model_changed`, `compacted`, `expired`, else `unknown`). It reads only the store, so plain `sqlite3` and Python keep a working file; an open rewrites it when its SQL changed, never a newer release's view, and the `cache` topic of an older release notes a newer view instead of reading it. A new index `requests_session_agent_ts` serves it.
- A `cache` report topic, the 21st: those outcomes and causes per main chat or sub-agent and per cache TTL.
- The `callmeter` skill's schema covers the new table and the view, with a recipe for cache outcomes.

### Changed
- A stored Bash command cuts more content to `'[cut]'`, its bytes under `operand_bytes`: a `git commit` message also under a wrapper (`sudo`, `env`, `timeout`, `xargs`, `xargs -I{}`, `nohup`, `command`, `nice`, …); the operands of an `echo` or `printf` also when its output reaches a file through `1<>`, a device path that is a file (`/dev/shm/…`), a pipe whose output goes to a file, a bare `exec` redirect earlier in the script (one in a `{ … }` block, a `&&` or `||` list or an `if` branch included), a block or loop at a pipe's end that writes a file or a `>&N` to a descriptor the script opened, with standard output and standard error followed apart (`>&2` into a file is cut, `>&2` to the terminal is not); and every here-string (`<<< word`), cut like a heredoc body. A report notes calls stored before these rules; `callmeter redact` rewrites them.
- A `PermissionRequest` or `PermissionDenied` event of an `mcp__` tool keeps no string of its `tool_input` in `events.detail`, each one `{key}_bytes`, as a call's input already did; `callmeter redact` rewrites stored details, reading the tool from `events.tool_name`, or from the detail itself for a `PermissionDenied` stored before that column held it.

### Fixed
- A hook whose write batch failed no longer holds off its signal handler while the fault row waits for a store another writer took meanwhile: the fault goes in under the hold only when the store is free at once, so a cancel during that wait leaves its `missed.log` line instead of being cut short by Claude Code's SIGKILL with nothing recorded.
- A hook that panics after its event was recorded logs the panic and its stack to `callmeter.log` again, as an unrecorded one does; only the `missed.log` line stays reserved for the unrecorded event.
- A signal landing while an unrecorded hook's panic unwinds no longer adds a `terminated` line to `missed.log` beside the panic's own, which counted one lost event twice.

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
