# Changelog

Every release of callmeter. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release carrying the section below and the four platform binaries.

## [Unreleased]

### Changed

- The cost ledger is read only from `CALLMETER_COST_LEDGER_DIR`. callmeter no longer reads `PFM_COST_LEDGER_DIR` or `PFM_HOME`, and no longer looks at a default `~/.local/state/pfm/cost-ledger`: with the variable unset, no ledger is read, and the `cost` report notes `side calls are not counted: no cost ledger is set; CALLMETER_COST_LEDGER_DIR names its directory`. A tool that writes a ledger sets the variable in Claude Code's environment.
- `scripts/reconcile` (development only) searches `~/.claude/projects` and the `projects` directory of every seat the snapshot records, where it searched a fixed list of seat directories, when `--projects` is not given.

## [0.4.5] — 2026-10-27

### Fixed

- A recorded async hook (every event but `SessionStart`, `SessionEnd` and `StopFailure`) waits up to 120 s for a concurrent writer, where it gave up after 5 s: a report's write transaction on a large store can hold the write lock longer than that, and the hooks that met it lost their events (`store-busy-gave-up`, each left as a `terminated by store busy` line in `missed.log`). Claude Code never waits on an async hook, and a signal still ends the wait at once. The sync hooks keep their bounds (`SessionEnd` and `StopFailure` 800 ms, `SessionStart` 5 s), as does an event no hook is known to run.
- A `SessionEnd` that ran out of its 1.2 s budget before every transcript read, an expected outcome that report-time recovery completes, no longer logs the plugin warning `transcript-unreadable`: its plugin log row is the info row `session-end-budget-spent`. Its `faults` row and message are unchanged.
- `less`, `more`, `nl` and `bat` attribute the files they name as whole reads, as `cat` does, where their operands were `unknown` (#1). Their separated and joined value-taking options are consumed, never read as files; a `less` or `more` `+command` (`+G`, `+/pattern`) is no file unless it follows `--`, and a `bat cache` subcommand attributes no displayed file. A reader option holding an unresolved expansion leaves the existing argument files `unknown` with a parser note.
- `awk` attributes the files it reads: its `-f` program files and its data operands, never the inline program text, the values of `-v` and `-F`, a data assignment `x=value` or the standard input `-`. The joined `-fprog.awk`, `-vx=a` and `-F,` forms read as the split ones, `--` ends the options and options stop at the first operand; an option or the program text holding an unresolved expansion leaves the existing argument files `unknown` with a parser note (#3). `cmdparse.Version` moves from 7 to 8, so the next report parses historical commands again and refreshes their stored parts.
- The outcomes report names the test runner for `jest` and `npx jest` (`jest`) and for `pnpm`, `yarn` and `bun` `test` or `run test` (each runner named for its manager); a runner's name given as an argument of another command is not a run (#2).
- `scripts/release-check.sh` reads a prerelease README badge: shields.io writes its dash as `--` (`version-0.4.5--dev.1-blue`), which the check decodes instead of stopping at the first dash.

## [0.4.4] — 2026-10-17

### Changed
- `/callmeter:report` with arguments that open with no topic treats them as a question: it loads `/callmeter:callmeter`, which reads the store directly, and runs no report.

## [0.4.3] — 2026-10-16

### Fixed
- A hook that gave up on a locked store after its full busy wait (up to 5 s) logged the plugin error row `store-busy-gave-up` with nothing saying the gap was already counted: its event goes unrecorded, and the `terminated by store busy` line it leaves in `missed.log` becomes a `terminated` fault at the next ingest, so the reports show it. That row is now a warning whose message says the event went unrecorded and is counted as a `terminated` fault from `missed.log`; it stays an error only when the line could not be appended too, the one case where the loss is not counted.
- The plugin warning `recover-skipped` (a transcript read that quiet-session recovery skipped) carried no detail. It now carries the session and, as `detail`, the `stage` of the read that was skipped and a `reason` from a fixed set (`not found`, `permission denied`, `malformed`, `refused` for a call input recovery read but would not store, `unreadable`); the transcript path and the error text stay out of the plugin log, as before. The `callmeter report` stderr line is unchanged.

## [0.4.2] — 2026-10-16

### Added
- `CALLMETER_RETENTION_DAYS`, the environment form of `config.json`'s `retention_days`: the same integer from 0 to 36500, read by `callmeter report` ahead of the file (the variable beats the file, the file beats the default of keeping every row). Unset or blank counts as absent. A value that is not such an integer is reported on stderr by the variable's name and in the plugin's error log as an error row with event `config-env-invalid`, and the default stands: the report runs and prunes nothing, never with the file's window. A `config.json` the report cannot trust still stops it, whatever the variable holds.

### Fixed
- A `SubagentStart` or `SubagentStop` hook no longer holds the store's write lock for a time that grows with the store. Its lookups of the agent's own start and stop events (`RebuildAgentTurns`, and the rebuilt-stop delete of `DropRecoveredAgentStop`) had no index by agent, so each read every `SubagentStart` and `SubagentStop` event ever stored; with a cold page cache one hold measured over a second. Every other writer waits on that lock, and a hook still waiting at the 5 s busy timeout gives up (`terminated by store busy`, each left in `missed.log` and as a `faults` row, none silent); the 2026-10-13 burst of those errors is consistent with this cause, not proven by it. The store gains the index `events_agent_ts`, added to an existing store by the next open that can take the write lock; a hook that could not wait and lost its event stays lost.

## [0.4.1]

### Fixed

- Hook-failure level rows include the cause, truncated to the row limit with the session preserved.

## [0.4.0] — 2026-10-14

### Added

- Plugin errors, warnings and info append to level files under `PLUGIN_LOG_DIR/callmeter`, or the resolved state home's `logs` directory.
- Bounded JSONL rows, 20 MB rename-aside caps and one logging-failure notice per failing streak, alongside unchanged legacy output.

## [0.3.0] — 2026-10-11

### Added
- `request_iterations`: each entry of a request's transcript usage `iterations` whose type is not `message` (such as `fallback_message`, a fallback model's answer), with its own token counts, written wherever the request is and carried through prune and `archive.db`. A request answered by a fallback model keeps the answering attempt's token columns, never a sum.
- A `request_cache` view, computed when read: each request's gap since the previous request of its main chat or sub-agent, the TTL of the cache it could read, the read a full hit would make, the outcome (`first`, `hit`, `partial`, `miss`) and, for a partial or miss, the cause the stored columns prove (`model_changed`, `compacted`, `expired`, else `unknown`). It reads only the store, so plain `sqlite3` and Python keep a working file; an open rewrites it when its SQL changed, never a newer release's view, and the `cache` topic of an older release notes a newer view instead of reading it. A new index `requests_session_agent_ts` serves it.
- A `cache` report topic, the 21st: those outcomes and causes per main chat or sub-agent and per cache TTL.
- The `callmeter` skill's schema covers the new table and the view, with a recipe for cache outcomes.
- An optional `config.json` under `CALLMETER_HOME` with one key, `retention_days` (0 to 36500): how many days of rows the store keeps. A config the report cannot trust (unreadable, empty, malformed, an unknown key, a value out of range) stops the report with a line naming the file.
- `/callmeter:ultimate-visualization [focus]`: a skill, invoked by the user only, that has Claude explore the store and publish one animated Artifact of what happened in the past, in a form it chooses. It carries its own build discipline, so it needs no other plugin or skill; it falls back to asking before it builds a plain HTML file when the session cannot publish an Artifact, and the page it publishes carries structure, timing and counts only, never commands, paths or prompts.
- A request is priced when it is written: Claude Code's own price from the cost ledger when it has one, else callmeter's embedded list, NULL only for a pending request or a model neither names. Applies to rows written after this release; no stored row is repriced, backfilled or swept.
- GPT models (`gpt-6-sol`, `gpt-6.1-sol`, `gpt-6-luna`, `gpt-6-astra`, `gpt-5.6-sol`, `gpt-5.6-luna`, `gpt-5.6-terra`) are priced from the list as a list-equivalent, long tier above 272 000 context tokens; the `cost` topic labels their share `list-equivalent`, since that route runs on a ChatGPT subscription and has no per-token bill.
- The `Stop`, `SubagentStop` and `SessionEnd` hooks ingest the session's new cost-ledger lines, so a request carries Claude Code's price without a report run: they read from a byte offset kept in the store (`sessions.ledger_offset`), at most 4 MiB a hook, regular files only, a torn last line waits for the next hook, and a missing or unreadable ledger is never a failure. A report run's ingest stays and changes nothing twice.
- Each tool call now records what its result cost the context once the next request is written: `calls.result_tokens`, `result_basis` (`exact`, or `split` by delivered bytes when a request issued several calls) and `result_write_micros`, the same in whatever order the hooks land. Forward only.
- `schema.md` documents the price columns, the three `calls` columns and `sessions.ledger_offset`, says an agent's cost is the sum of its requests, and says `session_costs.model_costs` of a non-Claude model is Claude Code's fallback rate, not a price.

### Changed
- `PriceUnpriced` (the report run's pass over rows no price reached) no longer prices `gpt-` rows, so a GPT request stored before the list named it stays without a price.
- A report no longer prunes rows older than 30 days: the store keeps every row by default, `--since` defaults to the whole store and a report's title reads `window: all time`. Set `retention_days` in `config.json` to prune again; `--since` then defaults to that window. An `archive.db` left by the earlier prune stays readable.
- A stored Bash command cuts more content to `'[cut]'`, its bytes under `operand_bytes`: a `git commit` message also under a wrapper (`sudo`, `env`, `timeout`, `xargs`, `xargs -I{}`, `nohup`, `command`, `nice`, …); the operands of an `echo` or `printf` also when its output reaches a file through `1<>`, a device path that is a file (`/dev/shm/…`), a pipe whose output goes to a file, a bare `exec` redirect earlier in the script (one in a `{ … }` block, a `&&` or `||` list or an `if` branch included), a block or loop at a pipe's end that writes a file or a `>&N` to a descriptor the script opened, with standard output and standard error followed apart (`>&2` into a file is cut, `>&2` to the terminal is not); and every here-string (`<<< word`), cut like a heredoc body. A report notes calls stored before these rules; `callmeter redact` rewrites them.
- A `PermissionRequest` or `PermissionDenied` event of an `mcp__` tool keeps no string of its `tool_input` in `events.detail`, each one `{key}_bytes`, as a call's input already did; `callmeter redact` rewrites stored details, reading the tool from `events.tool_name`, or from the detail itself for a `PermissionDenied` stored before that column held it.

### Fixed
- A named pipe, device or socket where a cost-ledger file or a transcript is expected no longer stalls `callmeter report`: the report run's ledger read and the transcript scan open a file without blocking and refuse anything but a regular file, with an error, as the hooks' ledger read already did.
- A hook whose write batch failed no longer holds off its signal handler while the fault row waits for a store another writer took meanwhile: the fault goes in under the hold only when the store is free at once, so a cancel during that wait leaves its `missed.log` line instead of being cut short by Claude Code's SIGKILL with nothing recorded.
- A hook that panics after its event was recorded logs the panic and its stack to `callmeter.log` again, as an unrecorded one does; only the `missed.log` line stays reserved for the unrecorded event.
- A signal landing while an unrecorded hook's panic unwinds no longer adds a `terminated` line to `missed.log` beside the panic's own, which counted one lost event twice.
- A report run's ledger ingest no longer leaves a `source = 'ledger'` row beside a transcript request that a hook gave the call in between: the side upsert writes only while no request of another source holds the call's requestId, decided in the write transaction, and the report run decides its side calls again there, so a call a hook stored in between is counted once, as held, at the ledger's price.
- The hooks' ledger ingest reads a ledger file that was replaced and grown past the kept offset from its start: the offset is always the end of a complete line, so a file shorter than it, or with no line end just before it, is read again from 0, and that reset is kept even when the new file holds no complete line yet. A line longer than the read cap is passed to its end, so the offset stays at a line end.
- A ledger error that lasts (a ledger directory that is a file, an unreadable file) writes one `faults` row per session and error text, not one on every `Stop` and `SubagentStop`; a repeat is logged to `callmeter.log` only. The store gains the index `faults_session_stage` for that check.
- A `SessionEnd` ledger ingest runs under the hook's budget: its wait for a busy store's write lock ends at its deadline, and one still running at the deadline rolls back and is logged, never a fault, and its bytes are read at the next hook.
- A ledger line with a token count above 1e12 or a `cost_usd_micros` above 1e15 is a bad line (counted, stored nowhere), so no count can wrap and no price leave int64.
- `schema.md` and `docs/design.md` say `result_write_micros` is also NULL when the next request's model has no row in the embedded price table, and that a `source = 'ledger'` row is a call no stored transcript request holds (a side call, or until its transcript request is written, a call the hooks have not stored yet).

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
