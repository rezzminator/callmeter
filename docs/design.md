# callmeter

`callmeter` is a Claude Code plugin that records every tool call a chat or sub-agent makes, with what it touched and what it cost, every model request, every sub-agent turn and every lifecycle event Claude Code hands a hook, into one local SQLite file, and answers questions from it: which files are read most and how large they were, which commands return the most text into a context, how context grows call by call, which call sequences repeat often enough to deserve one command of their own, and what each session, prompt, agent and effort level spent. It exists because the measured cost of agent-driven development is context blown up in long-running agents, and nothing recorded where the context went.

Decisions live in this file. A change lands here first, then in the code. How it is tested lives in [testing.md](testing.md).

## Contents

- [What it answers](#what-it-answers)
- [The plugin](#the-plugin)
- [The wrapper](#the-wrapper)
- [What the harness gives a hook](#what-the-harness-gives-a-hook)
- [The hooks](#the-hooks)
- [Where each fact comes from](#where-each-fact-comes-from)
- [Where the store lives](#where-the-store-lives)
- [The store](#the-store)
- [Parsing a command](#parsing-a-command)
- [Reports](#reports)
- [Corpus findings and fixes](#corpus-findings-and-fixes)
- [Build decisions](#build-decisions)
- [What it does not do](#what-it-does-not-do)
- [Evidence](#evidence)
- [Open items](#open-items)

## What it answers

1. Every call: the tool, its input, who made it (chat, sub-agent, agent type), under which prompt, at which effort and permission mode, how long it ran, whether it failed or was interrupted, and how many bytes the model received.
2. Files: read most, by count and by bytes delivered; their size on disk at read time; whole reads against ranged reads; the same file read again by the same agent; files written most, how much each write grew them, and the lines each edit added and removed.
3. Commands: which command shapes (`go test`, `git diff`, `make test`) deliver the most bytes into a context, per call and in total.
4. Context: the context size at every model request, per agent, so the call that pushed an agent from 40K to 400K has a name; the token split of every request (input, cache read, cache write at 5 minutes and at 1 hour, output) per model and agent type.
5. Sequences: runs of calls that repeat across agents (a search, then a read of the file it found, then a narrower read of the same file), ranked by how often they recur and what they cost, as candidates for one command or one script.
6. Sessions, prompts and agents: when each session started and ended and why, which model it ran, what each user prompt cost in calls, requests and tokens, and every turn of every sub-agent, a woken agent's later turns included.
7. Outcomes and coverage: commits made, test runners invoked, lines changed per file; and which transcripts on disk were never recorded, so a missing hook reads as a gap, never as a quiet week.

## The plugin

The repository is a one-plugin marketplace. `.claude-plugin/marketplace.json` at the root lists the plugin `callmeter`, sourced as a `git-subdir` of this repository at `plugins/callmeter`, ref `main`. Only what installs lives under `plugins/callmeter/`:

| Path | Holds |
| --- | --- |
| `.claude-plugin/plugin.json` | name, version (the one version the wrapper reads), description, author, keywords; no `hooks` or `skills` key, both sit at their default locations |
| `.claude-plugin/icon.svg` | the plugin's icon |
| `hooks/hooks.json` | the registration below |
| `libexec/callmeter` | the POSIX sh wrapper every hook and the skill run |
| `libexec/SHA256SUMS` | the release assets' sums, equal to a fresh reproducible build's |
| `skills/report/SKILL.md` | `/callmeter:report {topic} [flags]`, which runs the wrapper's `report` |
| `README.md`, `LICENSE` | what an installed plugin shows |

Everything else (`cmd/callmeter/`, `internal/`, `e2e/`, `scripts/`, `.github/workflows/`, `docs/`) builds, tests and releases the binary and never installs. There is no top-level `bin/`, and nothing is written to `${CLAUDE_PLUGIN_DATA}`.

The registration. Every entry is `{"type": "command", "command": "${CLAUDE_PLUGIN_ROOT}/libexec/callmeter", "args": ["hook"]}` plus its mode:

| Event | Matcher | Mode |
| --- | --- | --- |
| `PreToolUse` | `Bash` | `"async": true` |
| `PostToolUse`, `PostToolUseFailure`, `SubagentStart`, `SubagentStop` | `*` | `"async": true` |
| `PostToolBatch`, `Stop` | none | `"async": true` |
| `SessionStart` | none | sync, `"timeout": 60` |
| `SessionEnd` | none | sync, `"timeout": 2` |
| `Setup`, `UserPromptSubmit`, `UserPromptExpansion`, `InstructionsLoaded`, `PreCompact`, `PostCompact`, `StopFailure`, `PermissionRequest`, `Notification`, `PermissionDenied`, `TaskCreated`, `TaskCompleted` | none | `"async": true` |

Never registered: `WorktreeCreate`, `WorktreeRemove`, `MessageDisplay`, `FileChanged`, `ConfigChange`, `CwdChanged`, `DirectoryAdded`, `TeammateIdle`, `Elicitation`, `ElicitationResult`, `PreModelSwitch`, `PostModelSwitch`. A `WorktreeCreate` hook replaces Claude Code's own worktree creation, so registering it would change behaviour; `MessageDisplay` fires per streamed chunk; the rest carry nothing a report asks for or were never seen firing ([Evidence](#evidence)).

`SessionStart` is synchronous with a 60 s budget because its run is the one that may download the binary on first use; `SessionEnd` is synchronous with 2 s so the session's end lands before a headless process exits and cancels async hooks. Every other entry is async: the hook fires on every call of every chat, and a synchronous hook would put a process start and a database write in front of each call.

## The wrapper

`plugins/callmeter/libexec/callmeter`, POSIX sh, mode 0755. It resolves a binary, then `exec`s it with its own arguments and untouched stdin. Its root is the directory above the script's own, so it works the same from a hook and from the skill; `{version}` is the `version` of `{root}/.claude-plugin/plugin.json`.

Resolution order:

1. `$CALLMETER_BIN`, when set and executable: a development or test override that skips every step below.
2. `{CALLMETER_HOME}/bin/{version}/callmeter`, when executable: the cached binary of this plugin version.
3. A download of `${CALLMETER_RELEASE_BASE}/callmeter--v{version}/callmeter_{version}_{os}_{arch}` (`CALLMETER_RELEASE_BASE` defaults to `https://github.com/rezzminator/callmeter/releases/download`; os `darwin` or `linux` from `uname -s`, arch `amd64` or `arm64` from `uname -m`, anything else `unsupported platform`).

- Lock: `mkdir {CALLMETER_HOME}/bin/.lock-{version}` is the lock, removed by its holder on every exit path. A waiter polls for the cached file for up to 50 s, then fails with `lock wait timed out`; a lock older than 120 s is stale, removed once and retried. Eight hooks firing together on an empty cache download once.
- Checksum: the download lands in a temporary file under `{CALLMETER_HOME}/bin/`, is summed with `shasum -a 256` or `sha256sum`, and must equal the line of `libexec/SHA256SUMS` naming its asset; a mismatch, no sum line or no sum tool fails. A verified file is made executable, renamed atomically into the cache path, and every other cached version directory is removed.
- `missed.log`: any failure under `hook` exits 0 with nothing on stdout and appends one line `{unix seconds}\t{hook_event_name or unknown}\t{reason}` to `{CALLMETER_HOME}/missed.log`. The binary turns each line into a `faults` row with stage `binary` on its next run, hook or report, so a hook the wrapper could not serve is counted, never silent.
- Any failure under another subcommand prints `callmeter: binary unavailable: {reason}` on stderr and exits 1.

The binary's CLI:

- `callmeter hook`: one hook payload on stdin; exit 0 on every path; nothing on stdout.
- `callmeter report {topic} [--since D] [--project P] [--agent-type T] [--session S] [--limit N] [--json]` ([Reports](#reports)).
- `callmeter version`: `callmeter {version}`, set at build by `-ldflags "-X main.version={v}"`, default `dev`.
- `callmeter help`, `-h`, `--help`: usage on stdout, exit 0; no argument or an unknown subcommand: usage on stderr, exit 2.

Release assets are raw binaries `callmeter_{version}_{os}_{arch}` for linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64 under the tag `callmeter--v{version}`, built with `CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version={v}"` on Go 1.27.1 exactly, so the committed `SHA256SUMS` reproduces from source.

## What the harness gives a hook

Measured on Claude Code 2.1.280 with a capture hook that logged every raw payload, and again on 2.1.286 as a plugin under `claude -p --plugin-dir`; see [Evidence](#evidence).

| Event | Fires | Carries what callmeter needs |
| --- | --- | --- |
| `PreToolUse` | once per tool call, before it runs | `tool_use_id`, `tool_input`, `cwd`: the directory the command starts in |
| `PostToolUse` | once per tool call that ran | `tool_use_id`, `tool_name`, full `tool_input`, the tool's own result object, `duration_ms`, `cwd` (it follows the shell's `cd`: for `cd /x/sub && grep …` started in `/x`, `PreToolUse` carries `/x` and `PostToolUse` `/x/sub`), `session_id`, `transcript_path`, `prompt_id`, `permission_mode`, `effort` (sonnet, not haiku); a sub-agent's call adds `agent_id` and `agent_type` |
| `PostToolUseFailure` | once per call that failed | the same, with `error` and `is_interrupt` in place of the result |
| `PostToolBatch` | once per model request, after all its calls | `tool_calls`: every call of that request, each with `tool_use_id` and `tool_response` as the exact text the model received (a string) |
| `SubagentStart` / `SubagentStop` | once per sub-agent turn: an agent woken again (an orchestrator waiting on its children, a `SendMessage`) starts and stops once per turn | `agent_id`, `agent_type`, `prompt_id`; the stop adds `agent_transcript_path`, `last_assistant_message`, `background_tasks`, `session_crons`, `stop_hook_active`, and fires 20-50 ms after the final message is stamped, before its line is flushed to the agent's transcript |
| `Stop` | once per turn of a chat | `transcript_path`, `last_assistant_message`, `background_tasks`, `session_crons`, `stop_hook_active` |
| `StopFailure` | a turn that ended in an API error instead of `Stop` | `error` (`model_not_found`, `max_output_tokens`), sometimes `effort` |
| `SessionStart` | once per process start, resume and compaction | `source` (`startup`, `resume`, `compact`); `model` only for `compact` |
| `SessionEnd` | once per process exit | `reason` |
| `UserPromptSubmit`, `UserPromptExpansion` | per user prompt, per slash-command expansion | `prompt` (measured, never stored), `command_name`, `command_source`, `expansion_type` |
| `InstructionsLoaded` | per instructions file loaded | `file_path`, `load_reason`, `memory_type` |
| `PreCompact`, `PostCompact` | around every compaction | `trigger` |
| `Setup` | `claude --init-only` | `trigger` |
| `PermissionRequest`, `PermissionDenied`, `Notification` | a permission prompt, an auto-mode denial, a notification | `tool_name`, `notification_type` |
| `TaskCreated`, `TaskCompleted` | the task tools | `task_id` |

What no payload carries: the context size of a request and the model message id. Both live in the transcript, keyed by `tool_use_id`. The request is not on disk when `PreToolUse` fires, is usually on disk when `PostToolUse` fires, and was on disk in every check made 0.5 s later.

Three sizes exist for one Bash output, and only one is the cost:

| Size | Where | `seq 1 7000` |
| --- | --- | --- |
| Real | `tool_response.persistedOutputSize`, or the stdout length when nothing was persisted | 33,893 |
| The tool's result object | `PostToolUse` `tool_response.stdout`, cut at `BASH_MAX_OUTPUT_LENGTH` (default 30,000 chars) | 30,000 |
| Delivered to the model | `PostToolBatch` `tool_response` | 2,241: a `<persisted-output>` notice, the file path and a 2 KB preview |

Output under 30,000 chars is delivered whole; output over it is saved to `{session}/tool-results/*.txt` and delivered as the preview. The costly band is therefore just under the limit, and a later `Read` of a `tool-results/` file is the model going back for the rest.

A `PostToolUse` hook can replace a Bash result before the model sees it, with `hookSpecificOutput.updatedToolOutput`. `PostToolBatch` carries the text after that replacement, so the delivered size stays true when a filter is in play, and the gap between real and delivered bytes measures what a filter saved.

## The hooks

One command, `callmeter hook`, reached through the wrapper and registered on every event of [The plugin](#the-plugin)'s table. It is one command registered once per event: a duplicate registration is the same command appearing twice under one event.

- `PreToolUse` for Bash only, for its `cwd`, and its `tool_input` as a fill for a call whose `PostToolUse` is lost: the parser replays a command's own `cd` from the stored directory, so it must be the one the command started in, and only `PreToolUse` carries that. Everything a pre-call stat would give is in the result: `Write` and `Edit` return `originalFile` and a `structuredPatch`, so the size before and after and the lines added and removed come from the result; `Read` returns `startLine`, `numLines` and `totalLines`, and the file is stat'ed when the record is written.
- The hook never blocks and never fails a call: it exits 0 on every path and prints nothing. A failure to record goes to `{CALLMETER_HOME}/callmeter.log` with the session and `tool_use_id`, and is counted in the store's `faults` table, so a report can say "N calls were not recorded" instead of showing fewer calls.
- It changes nothing a model sees: no hook output, no added context, no blocked call.
- A session records only from the moment Claude Code loaded the plugin's hooks; a session started before the plugin was installed or enabled records nothing until it starts again.

## Where each fact comes from

| Fact | Source |
| --- | --- |
| Tool, input, duration, agent, prompt, permission mode | `PostToolUse` / `PostToolUseFailure` |
| Effort | the payload's `effort.level`, else the hook process's `CLAUDE_EFFORT` when non-empty, else NULL |
| cwd | a Bash call's `PreToolUse`, the directory the command started in; `PostToolUse`'s, where the command left the shell, only fills a call whose `PreToolUse` was never recorded (either may land first: `PreToolUse` overwrites, `PostToolUse` fills only an empty `cwd`) |
| Real output size, persisted path | `PostToolUse` `tool_response` |
| Bytes delivered to the model; which calls shared one request | `PostToolBatch` |
| File size at read time | stat of `tool_input.file_path` when the record is written |
| File size before and after a write; lines added and removed | `Write` / `Edit` result: `originalFile`, the new content and `structuredPatch` |
| Commit sha and branch | a successful Bash `git … commit` call's first stdout line `[{branch} {sha}]` |
| Test runner | the first segment of a Bash command that runs `go test`, `pytest`, `npm test`, `vitest` or `cargo test` |
| Model message id, model, stop reason and token split of the request | the transcript: the assistant message holding the call's `tool_use_id`, its `usage`. Read when `PostToolBatch` is recorded; a request not yet on disk is marked pending and filled at the next batch of the same chat or agent, or at the next `Stop` or `SubagentStop` |
| Sub-agent transcript | `{transcript_path without .jsonl}/subagents/agent-{agent_id}.jsonl` |
| Parent of a sub-agent | the `Agent` call's result `agentId`, joined to `SubagentStart` |
| A sub-agent's totals | the agent's own transcript, read at every `SubagentStop` once its final message is on disk (the hook re-reads until the last assistant entry carries a stop reason other than `tool_use`, at most 3 s): `total_tokens` as the sum over its requests of input + cache read + cache write + output, `tool_uses` as its count of `tool_use` blocks. The stop with the greatest `ts` wins, so an agent woken for more turns holds all of them. The `Agent` call's result (`totalTokens`, `totalToolUseCount`, `resolvedModel`) counts the first turn only and only fills what no stop wrote; a background agent's result is `status: async_launched` and carries none of them. `model` is the result's `resolvedModel`, else the last model the transcript names. Claude Code writes `stop_reason: null` on many finished background-agent transcripts, so for those the stop's wait for a final entry runs its full 3 s before the sums are read |
| Sub-agent turns | the stored `SubagentStart` and `SubagentStop` events of the agent ([The store](#the-store)) |
| Session start, end and model | the stored `SessionStart` and `SessionEnd` events and the session's main-chat requests |
| Host and time zone | the hook process: host name, and the zone name and UTC offset it computes (Claude Code leaves `TZ` unset) |
| Seat | the hook process's `CLAUDE_CONFIG_DIR` ([The store](#the-store), `seat_dir`) |
| Chat name, in reports | the session's own transcript under each seat it ran in: its last `customTitle`, else `aiTitle`, else `summary`, else the session id's first 8 characters |

## Where the store lives

The store is `{CALLMETER_HOME}/callmeter.db`, where `CALLMETER_HOME` is `${CALLMETER_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/callmeter}` (`XDG_STATE_HOME` counts only when absolute). Beside it: `callmeter.log` (JSON lines), `missed.log`, `archive.db` and the binary cache `bin/{version}/callmeter`. Under `go test`, resolving `CALLMETER_HOME` with the variable unset is an error, never the real home.

This deviates from Claude Code's plugin convention on purpose. The store lives at `CALLMETER_HOME`, never `${CLAUDE_PLUGIN_DATA}`, because plugin data is per seat (`{CLAUDE_CONFIG_DIR}/plugins/data/callmeter-{marketplace}`, observed on Claude Code 2.1.286) and pfm runs several seats whose calls belong in one store, and because the CLI and pfm must find the store without plugin env. One machine, one store: every seat's sessions land in the same file, told apart by `seat_dir`.

## The store

One SQLite file, its own: every call of every chat writes to it. WAL mode, `busy_timeout` 5 s against the concurrent async writers, one short transaction per hook run.

`PRAGMA user_version = 1`. A file at version 0 gets the whole schema in one `BEGIN IMMEDIATE` transaction; a file at version 1 opens without writing; a file at a higher version is refused with `newer than this callmeter`, never opened and misread. There is no migration code. An open retries a busy open until the busy timeout, because the first async hooks race to create the file and SQLite's switch to WAL takes no busy wait.

`ts`, `started`, `stopped`, `first_ts` and `last_ts` are integer Unix milliseconds, UTC, the hook run's clock reading, in every table.

### Tables

Every table is created `IF NOT EXISTS`, its columns in the order listed.

| Table | One row per | Key | Columns |
| --- | --- | --- | --- |
| `calls` | tool call | `tool_use_id` | `session_id`, `agent_id`, `agent_type`, `request_id`, `prompt_id`, `ts`, `tool`, `input`, `cwd`, `duration_ms`, `failed`, `is_interrupt`, `error`, `bytes_real`, `bytes_delivered`, `persisted_path`, `file_path`, `file_bytes`, `file_bytes_before`, `read_start`, `read_lines`, `read_total_lines`, `effort`, `permission_mode`, `lines_added`, `lines_removed`, `commit_sha`, `commit_branch`, `test_runner`, `source`, `config_dir`, `seat_dir` |
| `requests` | model request | `request_id` | `session_id`, `agent_id`, `prompt_id`, `ts`, `model`, `stop_reason`, `input_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `cache_creation_5m_tokens`, `cache_creation_1h_tokens`, `context_tokens`, `output_tokens`, `calls`, `pending`, `source`, `config_dir`, `seat_dir` |
| `agents` | sub-agent | `agent_id` | `session_id`, `agent_type`, `prompt_id`, `parent_tool_use_id`, `started`, `stopped`, `transcript_path`, `total_tokens`, `tool_uses`, `model`, `source`, `config_dir`, `seat_dir` |
| `agent_turns` | turn of a sub-agent | `agent_id`, `seq` (both `NOT NULL`) | `session_id`, `agent_type`, `prompt_id`, `started`, `stopped`, `start_event_id`, `stop_event_id` |
| `turns` | `Stop` or `SubagentStop` firing | `event_id` | `event`, `session_id`, `agent_id`, `agent_type`, `prompt_id`, `ts`, `effort`, `permission_mode`, `background_tasks`, `session_crons`, `last_assistant_message_bytes`, `stop_hook_active`, `seat_dir` |
| `events` | lifecycle event: every registered event except the four tool events | `event_id` | `event`, `ts`, `session_id`, `agent_id`, `agent_type`, `prompt_id`, `effort`, `permission_mode`, `source`, `model`, `reason`, `trigger`, `error_type`, `load_reason`, `memory_type`, `file_path`, `tool_name`, `command_name`, `task_id`, `prompt_bytes`, `detail`, `seat_dir` |
| `sessions` | session | `session_id` | `first_ts`, `last_ts`, `engine`, `model`, `start_source`, `end_reason`, `cwd`, `transcript_path`, `seat_dir`, `config_dir`, `host`, `tz_name`, `tz_offset_minutes` |
| `command_parts` | simple command inside one Bash call | `tool_use_id`, `seq` | `lang` (`sh`, `python`, …), `program`, `args`, `files` (a JSON list, one `{action}\t{range}\t{exists 0\|1}\t{path}` per file), `conditional`, `parse_status`, `parser` (the `cmdparse.Version` that produced the part) |
| `faults` | failure to record or parse | — | `ts`, `session_id`, `tool_use_id`, `stage`, `error` |

Indexes: `calls_session_agent_ts(session_id, agent_id, ts)`, `calls_ts(ts)`, `calls_file_path(file_path)`, `calls_prompt(prompt_id)`, `requests_session_agent_pending(session_id, agent_id, pending)`, `turns_session_ts(session_id, ts)`, `events_session_ts(session_id, ts)`, `events_event_ts(event, ts)`.

- `source` is `hook`: every row comes from a hook event, and no reader branches on it.
- `config_dir` is `$HOME/.claude`, the history home whose `projects/` holds the transcripts. `seat_dir` is the hook process's `CLAUDE_CONFIG_DIR`, the seat the chat ran under, made absolute and clean with symlinks NOT resolved; unset or empty, it is `$HOME/.claude`. Both are set on every row the hook writes that carries them, and a set value is never overwritten by a write that lacks it.
- `calls.error` is the failure text of a failed call, cut to 500 characters.
- The request key: `requests.request_id` is the model message id once the transcript shows it. Until then it is the provisional key `pending:{first tool_use_id of the batch}`, with `pending = 1`. When the id is later read from the transcript, the fill rewrites that request row to its real `request_id` and rewrites every `calls.request_id` that still carried the provisional key, in one transaction. `context_tokens` is `input_tokens + cache_read_tokens + cache_creation_tokens` from that message's `usage`; `cache_creation_5m_tokens` and `cache_creation_1h_tokens` are the split of `cache_creation_tokens` by cache TTL.
- A sub-agent's parent comes from the `Agent` or `Task` call whose result carries `agentId`: that agent is upserted into `agents` with the calling `tool_use_id` as `parent_tool_use_id`, together with the result's `totalTokens`, `totalToolUseCount` and `resolvedModel`.
- `faults.stage` is one of `payload`, `store`, `transcript`, `parse`, `binary`, naming where the failure happened; `binary` rows come only from ingesting `missed.log`. A fault that cannot itself be written to the store (the store is unreachable) goes to `callmeter.log` only.
- Every tool-call write is an upsert by its row's key, setting only the columns its event owns and leaving the rest untouched, because async hooks arrive in any order: a `PostToolBatch` can land before its `PostToolUse`.

### Identity and order

- `event_id` is the lowercase hex SHA-256 of the hook run's raw stdin. Same bytes, one `turns`/`events` row whatever the ts: it holds the smallest-ts delivery; a tie keeps the stored row. So an agent woken twice in one prompt (identical `SubagentStart` bytes) has its later turn's `started` NULL.
- The store is a function of the set of (payload, ts) pairs, never of their arrival order. `agents.started` keeps the minimum, `agents.stopped` the maximum, and the totals of the stop with the greatest `ts`; `agent_turns` and the derived `sessions` columns are recomputed from stored rows inside the write transaction.
- `agent_turns` for one agent: its `events` rows with event `SubagentStart` or `SubagentStop`, ordered by `(ts, event_id)`, walked once. A start opens a turn (an already open turn stays with `stopped` NULL); a stop closes the open turn; a stop with no open turn is a row with `started` NULL; `seq` counts from 1. A compaction's `SubagentStop` (empty `agent_type`, transcript missing) makes `events` and `turns` rows but no `agents` or `agent_turns` row.
- `sessions` derived columns: `start_source` is the `source` of the session's earliest `SessionStart` event; `end_reason` the `reason` of its latest `SessionEnd`; `model` the `model` of its latest main-chat request (`agent_id` NULL) whose model is set, else of its latest `SessionStart` carrying one. `first_ts` is the minimum and `last_ts` the maximum over every run of the session; `cwd`, `transcript_path` (the main transcript), `seat_dir`, `config_dir`, `host`, `tz_name` and `tz_offset_minutes` come from the earliest run: a run with ts below the stored `first_ts` overwrites those it carries, others fill; `engine` is `claude`.

### Privacy

No prompt text, message text or file content is stored: each becomes `{name}_bytes`, its UTF-8 byte count. `last_assistant_message` is stored only as `last_assistant_message_bytes`, a prompt only as `prompt_bytes`.

- What is stored of a tool input: the sanitized input JSON keeps a Bash `command` and `description`, file paths, search patterns and the `Read` range. `content`, `old_string`, `new_string`, an `Edit`'s `edits`, and an agent's `prompt` are never stored; each is replaced by its byte length under the same key with a `_bytes` suffix (`content_bytes` in place of `content`). `prompt` is dropped for every tool; any other field over 4096 bytes becomes `{name}_bytes`.
- `detail`, `background_tasks` and `session_crons` are sanitized JSON: objects and arrays keep their shape; numbers, booleans and null stay; a string value is kept only when its key is one of `id, type, status, source, reason, trigger, error, level, mode, behavior, destination, notification_type, load_reason, memory_type, command_name, command_source, expansion_type, task_id, tool_name, model, file_path, trigger_file_path, schedule, cron`; every other string becomes `{key}_bytes`, and a string inside an array whose key is not on that list becomes its byte count. `detail` omits the keys that have a column or are common to every event: `session_id, transcript_path, cwd, hook_event_name, prompt_id, agent_id, agent_type, permission_mode, effort`.
- Commit messages, patch lines and command output are never stored: an outcome is a count, a sha, a branch name or a runner name.

### Effort

`effort` on `calls`, `turns` and `events` is the payload's `effort.level` when present, else the hook process's `CLAUDE_EFFORT` when non-empty, else NULL.

### Prune and archive

A report run prunes rows older than 30 days; the hook never prunes, so a write always succeeds even mid-prune-cycle. `calls`, `requests`, `turns`, `events` and `faults` age by `ts`; `agents` and `agent_turns` by `COALESCE(stopped, started)`; `sessions` by `last_ts`; then orphan `command_parts` go. A `calls` row with NULL `ts` ages by its `requests` row's `ts`; other NULL-time rows stay.

In the same transaction, before deleting, every row about to go is copied with `INSERT OR IGNORE` into `{CALLMETER_HOME}/archive.db`, which holds the same tables minus `calls.input`, `calls.error`, `faults.error` and `events.detail` (`user_version` 1, created on first use). An archive failure rolls the prune back and is returned as an error: no row is deleted that was not archived.

## Parsing a command

A Bash call is often several commands (`F=x; wc -l "$F" && grep -n func "$F"`) and often carries another language inside it (`python3 -c "…"`, `python3 - <<'EOF' … EOF`, `node -e "…"`). The parse happens at report time only (`report.EnsureParsed`), never in the hook, and its result is cached in `command_parts`, keyed by the call. No parser is written here; each language uses its established one:

| Language | Parser | Yields |
| --- | --- | --- |
| Shell | `mvdan.cc/sh/v3`, the parser behind `shfmt`, pinned at `v3.14.1` | every simple command with its arguments through pipes, `&&`, `;`, loops, `$(…)`, assignments, redirections and heredoc bodies |
| Python | Python's own `ast` module, one `python3 -c {embedded script}` process per batch of snippets | calls and string constants: `open(…)`, `Path(…)`, `subprocess` arguments |
| JavaScript (`node -e`) | not parsed: a named gap, counted in every report that counts commands and in `faults` | — |

The Python batch protocol is one process per batch: stdin is a JSON list of `{id, code}`, one entry per snippet found in that batch's Bash calls; stdout is a JSON list of `{id, calls, strings, error}` in the same order. A Python snippet is found in `python3 -c ARG`, in a `python3 - <<EOF … EOF` or `python3 <<EOF … EOF` heredoc body (the same forms for the bare `python` alias), and `python3 script.py` attributes the file `script.py` with the action `exec` rather than parsing it.

`command_parts.parse_status` is one of:

| Value | Meaning |
| --- | --- |
| `ok` | parsed cleanly |
| `error` | the parser rejected the snippet; the message is kept in `faults` |
| `unparsed` | a language not parsed (`node -e` and others) |
| `python-unavailable` | no `python3` on `PATH`: every Python snippet in the run is marked this way, and `faults` reports the count so the gap is never a silent zero |

From the parsed commands, a file is attributed to a call when an argument, an assigned value or a string constant resolves to a file from the directory the command started in, braces and globs expanded as the shell expands them ([Corpus findings and fixes](#corpus-findings-and-fixes) names when a missing file counts). What a command did with the file (whole read, line range, search, write) comes from a table of the common readers (`cat`, `head`, `tail`, `sed -n`, `grep`, `rg`, `wc`, `open`), each row naming which argument is the range. A command the table does not know attributes its files with the action `unknown`.

One Bash call returns one output: the bytes of a compound command belong to the call, never split across its parts.

A static parse cannot know which branch ran, so every file a part names is attributed as read, whether or not its branch ran; the call's bytes are measured at its end either way. What the parse does know is where the part sits: `command_parts.conditional` is 1 for a part inside an `if` branch or the `else` chain (an `elif` condition included), a `case` arm, or on the right-hand side of `&&` or `||`, and 0 otherwise; the first `if` condition and the `case` word run unconditionally. A Bash call's read of a file is certain when any unconditional part of that call names the file, conditional otherwise. A file test (`[`, `test`, `[[ ]]`) reads nothing and attributes no file. The `files` report shows `READS` (every attributed read), then `CERTAIN` and `CONDITIONAL`, so a file that ranks high only through guarded reads (`if [ -f x ]; then cat x; fi`) shows as such.

## Reports

`callmeter report {topic} [--since D] [--project P] [--agent-type T] [--session S] [--limit N] [--json]`, each topic a fixed query over the store; flags may follow the topic. Inside Claude Code the same runs as `/callmeter:report {topic} [flags]`. Every report prunes and archives rows older than 30 days, and ingests `missed.log`, before it runs.

| Flag | Meaning |
| --- | --- |
| `--since D` | a duration (`7d`, `24h`) or a date (`2026-09-01`); default is the whole 30-day retention window; an older value is clamped with one stderr note |
| `--project P` | calls whose `cwd` is `P` or under it; `P` is made absolute, symlinks unresolved |
| `--agent-type T` | calls made by that agent type |
| `--session S` | calls in that session |
| `--limit N` | rows per table, default 25 |
| `--json` | one JSON object on stdout instead of the text table |

Text output is a plain fixed-width table: one header line naming the topic, the active filters and the window, then the rows, then a `note:` line for every named gap the query hit — calls not recorded (from `faults`), requests still pending, snippets unparsed per reason, binary runs missed, chat names that could not be read. An empty window prints `callmeter: no calls recorded in window`.

`--json` prints one object: `{"topic": "...", "store": "present", "title": "...", "columns": ["..."], "rows": [["..."]], "notes": ["..."]}`. `columns` are the text table's header cells, `rows` arrays of the same cell strings, `notes` the text `note:` lines without the prefix; an empty window is `"rows": []`.

Absence and failure never share a line. No store file: `callmeter: no store at {path}: nothing recorded yet` on stdout (with `--json`: `{"topic": "...", "store": "absent", "path": "...", "columns": [], "rows": [], "notes": []}`), exit 0, nothing created. A store that cannot be opened, or a parse or report error: exit 1. A bad topic or flag: exit 2.

| Topic | Answers |
| --- | --- |
| `files` | files by bytes delivered, read count (certain and conditional as two columns), distinct agents, size on disk, whole against ranged reads, re-reads by one agent; `Read` bytes and Bash-attributed bytes are two separate columns, never summed, because a Bash call's bytes are shared evenly across the files it credits |
| `writes` | files by write count and total growth |
| `commands` | command shapes by bytes delivered (sum, p50, p95), count in the band 20,000–30,000 chars, persisted count, follow-up reads of `tool-results/`. A call's shape is the shapes of its non-trivial parts in order (`cd`, `export`, `set` and bare assignments dropped, and `echo`, `printf`, `true` and `:` when their arguments are literal and they redirect to no file), deduplicated, joined with ` ; `, at most three parts. One part's shape is the program's base name plus its first argument, when that argument is neither a flag nor a path (`go test`, `git diff`, `make test`). A stream filter (`head`, `tail`, `grep`, `sed`, `awk`, `sort`, `uniq`, `wc`, `cut`, `tr`, `tee`, `cat`, `jq`, `less`, `more`, `column`, `nl`) naming no file is dropped when the call has another non-trivial part, so `cd x && go test ./... \| tail` is `go test` (`report/commands.go`, `callShape`) |
| `context` | per agent: requests, start and peak context, mean growth per request, and the three requests with the largest growth, named by their calls |
| `sequences` | call runs of length 2 to 5 that recur across agents, each step normalized to its shape (tool, program, the file's role), ranked by occurrences × bytes delivered. Steps are taken per agent (`session_id` + `agent_id`) in `ts` order; a step's shape is the tool, for Bash the first non-trivial program, and the file's role — `same` when the step repeats a file an earlier step in the same run touched, `new` otherwise, `-` when the step names no file. A run must recur across at least two distinct agents to be listed |
| `faults` | calls not recorded, snippets not parsed, binary runs missed |
| `sessions` | one row per session: chat, session, started, last, model, start source, end reason, calls, agents, cwd, host, time zone; latest first |
| `prompts` | one row per prompt: first and last call, calls, failed calls, agents, requests, context and output tokens; most calls first |
| `effort` | calls, turns and failed calls per effort level, permission mode and agent type; NULL shows `-` |
| `tokens` | per model and agent type: requests, input, cache read, cache write at 5 m and at 1 h, cache write, output, and the cache hit rate `cache_read / (input + cache_read + cache_creation)`; a zero denominator shows `-` |
| `agents` | one row per sub-agent turn: agent, type, chat, turn, started, stopped, seconds, prompt; an open turn shows stopped `-` |
| `outcomes` | edits per file (lines added and removed), commits per sha with their branch, test runs per runner |
| `coverage` | transcripts under each seat's `projects/` modified in the window that the store never recorded, with a note counting modified, recorded and unrecorded; an unreadable `projects/` directory is a note naming it, never zero |
| `events` | lifecycle events by kind and value (source, reason, trigger, error type, load reason, tool name, command name, notification type), with count, first and last |

Chat names come from each session's own transcript, read-only, by `session_id` over the seats the store recorded for it: its last `customTitle`, else `aiTitle`, else `summary`, else the session id's first 8 characters. A transcript found but unreadable leaves the name column `?` and prints one note line with the error, rather than failing the report.

## Corpus findings and fixes

A one-time study of 30 days of real transcripts (62,005 calls, 4,068 transcripts, three seats) parsed all 37,355 Bash calls, and exposed where attribution was wrong. Each rule below answers one measured defect:

- **Byte share.** A Bash call's bytes are split evenly across the files it attributes: each file is credited `call bytes ÷ files attributed`, so per-file sums add up to the call. One `grep -l` over a 1,445-file glob had credited its whole output to every file.
- **Wrappers.** `command`, `builtin`, `exec`, `env` (its assignments and flags), `timeout` (its duration), `nice`, `nohup`, `time`, `/usr/bin/time`, `sudo` and `xargs` are unwrapped to the program they run, with that program's arguments. A program given as a path is matched by its base name (`/bin/ls` is `ls`); a program path that is a file under the directory its part runs in (after any `cd`) is also attributed with the action `exec`.
- **Inner shells.** The `-c` string of `bash`, `sh` and `zsh` is parsed as shell with the same directory and conditional depth. `ssh`, `docker exec`, `kubectl exec` and any other remote runner are never parsed: their paths live on another machine. A project's own runner that takes a command string (`./run.sh "…"`) is not parsed: a named gap.
- **`cd` within a call.** A literal `cd DIR` moves the directory later parts of the same call resolve against; a `cd` inside `( … )` ends with the subshell; a `cd` to a non-literal target stops relative attribution for the rest of that list.
- **Redirects.** `> f` and `>> f` attribute a `write`, `< f` a `read-whole`; heredoc bodies, `/dev/*` and fd duplications (`2>&1`) attribute nothing.
- **Metadata is not a read.** `ls`, `du`, `stat`, `file`, `find`, `realpath` and `readlink` attribute the action `stat`, which no read count includes; `[`, `test` and `[[ ]]` attribute nothing.
- **Missing files.** A path that a known reader or writer names (a file operand of `cat`, `head`, `tail`, `sed`, `grep`, `rg`, `wc`, a redirect target, a Python `open()`) is attributed even when it does not exist at parse time, with `exists = 0`: history keeps its deleted and scratch files. An unknown program's arguments are attributed only when they exist, so a word that merely looks like a path is never a file. Text that still carries an expansion nobody performed — a `$` or a backquote the shell left inside a Python string, a glob character, braces (`{}` of `xargs -I{}` and `find -exec`, a quoted `{a,b}`), a leading `~` — names a file only when that exact file exists, never as a missing one: 1,315 missing-file rows of the second pass were such text.
- **Tilde and braces.** A word's leading unquoted `~` or `~/` is the home of the user the commands ran as (the report's home); `~user`, `~+` and `~-` attribute nothing. A Python string starting `~/` is expanded the same way, since it is `expanduser`'s input. An argument's `{a,b}` is brace-expanded as bash does before any glob (mvdan's `syntax.SplitBraces` and `expand.Braces`), so `wc -l internal/{a,b}/x.go` names two files; a sequence (`{1..9}`) or a product over 64 words stays one literal word. Before this, 756 rows were a `~` joined to the call's directory as a literal folder.
- **The directory a command starts in.** `PostToolUse`'s `cwd` is where the command left the shell, and the parser replays the command's own `cd` from the stored directory, so the hook stores `PreToolUse`'s ([The hooks](#the-hooks)). The transcript's `cwd` is the starting directory in 16 of the 23 calls of the corpus that open with a relative `cd`; the other 7 are calls sent in parallel. The Bash tool's directory persists from one call to the next, even between calls of one parallel batch, which is why only `PreToolUse` is trusted.
- **A parser fix reaches stored calls.** Every part records the `cmdparse.Version` that produced it, and a report parses again every call whose parts carry an older one, replacing its parts and its parse faults; without it a fix would leave 30 days of stale attributions.
- **Shapes.** `echo`, `printf`, `true` and `:` with literal-only arguments are trivial in a command shape, like `cd` and `export`, so a separator `echo ---` does not split `sed ; grep` into a new shape.

### Test corpora

Unit tests alone passed while this corpus failed, so two tests made of real data guard the parser and the hook:

1. **Real commands.** A table-driven test of Bash commands taken from the one-time study of real transcripts (nested quoting, heredocs, `cd` chains, wrappers, globs, redirects, inner `bash -c`, Python heredocs, loops, arrays), each with its expected parts, attributions, actions and `conditional` flags. Paths are rewritten to `/tmp/demo-proj/…`; the case keeps the command's structure verbatim. The corpus lives in `internal/callmeter/cmdparse/testdata/commands-corpus.json`; a case whose parse differs from the spec carries `known_defect` and is skipped by name until it matches.
2. **Captured hook payloads.** The payloads captured from headless sessions ([Evidence](#evidence)), in `internal/hookentry/testdata/callmeter/` with their transcripts in its `demo-home/`, are fed through the hook into a real store; the test then runs `report.EnsureParsed` and the reports over what the hook recorded.

Fixtures come only from sessions over invented toy projects, never from a project holding client data, and every fixture file passes `scripts/leak-check.sh` before it is committed: every path rewritten to `/tmp/demo-proj/…` and `/tmp/demo-home/…`, no username, no email.

## Build decisions

Decided while building, one line each, file named:

- Store (`store.go`, `write.go`): prune keeps rows with no timestamp and ages agents by `COALESCE(stopped, started)`; `ResolveRequest` merges a provisional row into the message row (stored values win, NULLs filled, `calls` summed, the earlier `ts` kept); the 500-character error cut counts characters, not bytes; an open writes nothing when the schema version is current, creates the schema under `BEGIN IMMEDIATE`, and retries a busy open until `BusyTimeout`.
- Sanitized input (`sanitize.go`): `prompt` is dropped for every tool; any other field over 4096 bytes becomes `{name}_bytes`; input that is not a JSON object is an error.
- The hook (`hookentry/callmeter.go`): `bytes_real` is the first of `persistedOutputSize`, stdout, `file.content`, `content`, the response JSON's length; a batch spanning several messages writes one request per message; `Stop` and `SubagentStop` read the transcript only when a request is pending; a payload with no `session_id`, and a stat failure other than not-exist, are `payload` faults; the store's home resolves through `paths.Home`, so `CALLMETER_HOME` jails it; `SubagentStop` fills an agent's still-empty `total_tokens`, `tool_uses` and `model` from its transcript (a message id's usage counted once, distinct `tool_use` blocks), never over the `Agent` result's values; a store open that fails names a batch's call ids.
- Delivered bytes (`callmeter.DeliveredBytes`): only `text` blocks count; a shape it cannot measure is an error, never a guess.
- File columns (`FileColumnsFromInput`, `FileColumnsFromResult`), used by the hook: `Read`, `Write`, `Edit`, `MultiEdit`, `NotebookEdit`; a relative path joins `cwd`; the Read input's `offset`/`limit` are overridden field by field by the result's `startLine`/`numLines`/`totalLines`; `type: create` with a null `originalFile` is 0; a file tool with no path is a fault.
- Parser (`cmdparse`): a bare assignment makes no part and its literal value is substituted into later uses; ranges are `head` → `1,N`, `tail` → `-N` or `K,$`, `sed` → `A,B`; `sed -i` is a write; a Python snippet's id is `{callID}#{seq}`; `node file.js` is a plain shell part, only `-e`/`--eval`/`-p`/`--print` are unparsed; a file operand of `cat`, `head`, `tail`, `sed`, `grep`/`egrep`/`fgrep`/`rg`, `wc`, a redirect target and a Python `open()`/`Path(…)` read or write is attributed when missing, `Exists = false`, unless its text is a placeholder (`placeholder`: `$`, a backquote, a glob character, `{…}`, a leading `~`); `-`, a directory, a glob that matched nothing and a reader's operands under `xargs` never are; a leading unquoted `~`/`~/` is `Call.Home` (the report passes the home `paths` resolves), and no home, `~user`, `~+` or `~-` leave the word unknown; an argument word is brace-expanded first (`braces`, over `syntax.SplitBraces` and `expand.Braces`, which `syntax.Walk` cannot visit, hence `braceBound`), a sequence or a product over 64 kept literal; a wrapper is unwrapped in `wrappers.go` and the part records the program it runs with that program's arguments; `command -v`/`-V` and a wrapper naming no program stay the wrapper's part and attribute no file; a program path resolves against the directory its part runs in after any `cd` and is `exec` only when it is a regular file under that directory; `find` stats only its starting points, never a word of its expression; a literal `bash`/`sh`/`zsh` `-c` string (flag clusters `-lc`, `-ec` included) keeps the shell part (its program file and redirections) and appends the string's parts after it at the same conditional depth, starting in the current directory, in a child scope whose `cd` and variables end with it; a non-literal string is an ordinary part, one that fails to parse is one error part; `cd` moves the directory statically, even in a branch, and `( … )`, `$(…)` and each side of a pipe restore it; after a `cd` the parse cannot know, relative paths attribute nothing until a literal absolute `cd`; a Python part resolves its strings in the directory it ran in; `NAME=(…)` with literal elements expands through `${NAME[@]}`, `${NAME[*]}` (every element), `${NAME[N]}` and `$NAME` (the first); a `for` loop's items and an array's elements expand as any unquoted word does, and a value built from a glob that matched nothing is never a missing file; an unquoted `\X` is `X` and a backslash-newline is removed, so an escaped glob character stays literal; a backslash inside double quotes escapes only `$`, a backquote, `"`, `\` and a newline; `ssh`, `mosh` and the `exec`/`run` verbs of `docker`, `podman`, `kubectl` and `oc` attribute no argument; `cmdparse.Version` is raised with every change to what a command parses to, and `report.EnsureParsed` parses again every call whose parts carry another version (`Tx.ReplaceCommandParts` replaces the parts and the call's parse faults together); word evaluation lives in `words.go`, the walk in `cmdparse.go`.
- Reports (`report/*.go`): a Bash call's bytes are shared evenly across the distinct files it credits for read, search and unknown actions, a missing file included, bytes ÷ N each and the remainder one byte each to the first files in part order, so the shares sum to the call's bytes exactly (`bashShares`); `echo`, `printf`, `true` and `:` are trivial in a shape only when the part credits no file and no argument's stored text holds `$`, a backquote, `<(` or `>(` (one rule, `trivialPart`, shared by `commands` and `sequences`); `files` shows SIZE `gone` when the latest call naming a file found it missing, `-` when no size was ever seen; a Read is ranged when `read_start > 1` or `read_lines < read_total_lines`; the 20,000–30,000 band is measured on `bytes_real`; p50 and p95 are nearest-rank; `context` lists growth only and names each jump by the previous request's calls; `sequences` counts overlapping windows, drops a shorter run occurring exactly as often as a longer run containing it, and ranks by occurrences × bytes.
- Found live in the first minutes after install: a failed Bash call's size is its error text, the hook's `PostToolUseFailure` `error` (the transcript's `toolUseResult` is the same text behind `Error: `); Claude Code stops its own internal agents (every `/compact` among them) with a `SubagentStop` that has no `agent_type`, no `SubagentStart` and no transcript on disk, and the hook records no agent for those (`untypedAgentMissingTranscript`; 16 false transcript faults before), while a typed agent missing its transcript stays a fault; the same exemption covers `PostToolBatch` for one of these agents, which used to fail `FindRequests` against the missing file, write a pending request, then fail `resolvePending` against the same file for a second fault; a call still running has only its `PreToolUse` row until its result lands; a headless `claude -p` cancels running async hooks at exit ("Hook cancelled"), so its last calls can lose their `PostToolUse` and keep only their `PreToolUse` row.
- Pending requests: every `PostToolBatch` also retries the pending requests of its own chat or sub-agent (`resolvePending`), so a request that missed the disk at its batch resolves at the next one, not at a `Stop` an hour away (live: 11 pending on running sub-agents, every id already on disk).
- Agent totals (`settledAgentTotals`, `recordAgent`): 31 of 33 live agents were summed without their final message, because `SubagentStop` fires before that line is flushed; a waiting orchestrator stored 10 of its 86 tool uses and a `started` after its `stopped`, because each turn restarts and re-stops it. The stop now waits for the final message, the latest stop's sums win, and `started` keeps the first start. A compaction's stop skips the wait.
- Request lookup (`callmeter.FindRequests`): reads the transcript's last MB first and widens fourfold only while a wanted id is missing, and decodes a line only when it names a wanted id; a malformed line naming one is an error at its byte offset, a malformed line naming none is never read. Measured on a 40 MB transcript: `PostToolBatch` 111 ms → 10 ms at the median. Every event's hook runs about 12 ms (process start, store open, one write) on a 63,000-call store, all of it async.
- The start directory (`hookentry/callmeter.go`, `recordStartCwd`): `PreToolUse` upserts `cwd` with `Overwrite` and `ts`/`tool` with `FillEmpty`, so a call that never finishes still ages out; `PostToolUse` upserts every other column with `Overwrite` and `cwd` alone with `FillEmpty`; a `PreToolUse` payload without `tool_use_id` or `cwd` is a `payload` fault.
- One run, one transaction: every write of one hook run (the call or request, its event, turn, agent turns and session refresh) goes in one batch, and a `UserPromptSubmit` whose prompt starts with `<task-notification>` carries `task_notification` in its `detail`, since Claude Code fires that event for its own background-task notices, which are not user turns.

## What it does not do

- It changes nothing a model sees: no hook output, no added context, no blocked call.
- No account join: a call is tied to the seat it ran under (`seat_dir`), never to an account name.
- No guard-fire rows and no receiver id for chat-to-chat messages.
- No Codex or OpenCode recording; that belongs to a later pfm flight.
- No `node -e` parsing: an unparsed, counted gap.
- No Windows: the wrapper is POSIX sh and the binaries are built for darwin and linux only.
- No calls the harness refused before running (an unavailable tool): they leave no hook event, so they are not recorded.
- No prompt text, message text, file content, patch lines, commit messages or command output in the store.

## Evidence

A capture hook, loaded only into headless sessions in a scratch directory over invented toy projects, logged every raw payload.

Claude Code 2.1.280, loaded through `--settings`:

- A scripted sonnet run read one file eight ways (Read whole and ranged, `cat`, `head | tail`, `sed -n`, `python3 -c "open(…)"`, a compound with a variable, a loop), then wrote, edited, appended through Bash, printed a large output, failed a command and spawned a haiku sub-agent. Every call that ran produced `PostToolUse` or `PostToolUseFailure`; the sub-agent's calls carried its `agent_id`.
- `seq` at 5,000 / 6,000 / 6,500 / 7,000 / 9,000 lines: delivered whole at 23,892 and 28,892 chars, as a 2,775-byte preview at 31,393 and above.
- `PostToolBatch`: three parallel calls came as one batch of three, a single call as a batch of one; its `tool_response` for `seq 1 7000` was the `<persisted-output>` preview.
- `updatedToolOutput`: with a test-output filter wired on `PostToolUse`, a fake `pytest` printing 302 lines reached the model as 200 lines, and `PostToolBatch` carried those 200.
- Transcript timing, 11 calls across a chat and a sub-agent: the call's request was absent at `PreToolUse` in 11 of 11, present at `PostToolUse` in 11 of 11 in one run and 0 of 5 in another, and present 0.5 s later in 11 of 11.
- A natural opus run in bypass mode, where the harness withholds the Grep and Glob tools, searched with `grep -rn` through Bash in 4 of its 6 calls and read with ranged `Read` in 2.

Claude Code 2.1.286, as a plugin: a per-event verification capture (every hook event registered, 12 headless sessions on haiku: tools, permissions, a sub-agent, a slash-command skill, a bad model, a one-token output cap, `--init-only`, a resumed `/compact`, `--worktree`) and a gymnastics capture (four sonnet sessions in bypass mode: wide parallel batches, persisted output, background and nested sub-agents woken by `SendMessage`, two `/compact` cycles over one session id, paths with spaces, Unicode and symlinks, deleted files, an image, a Bash timeout, a background Bash). What they showed:

- Plugin hooks fire under `claude -p --plugin-dir`; `CLAUDE_PLUGIN_ROOT` is the `--plugin-dir` path as given, and `CLAUDE_PLUGIN_DATA` is `{CLAUDE_CONFIG_DIR}/plugins/data/{plugin}-{marketplace}` (per seat, hence [Where the store lives](#where-the-store-lives)). `TZ` is unset in every hook process.
- `effort` is in tool payloads on sonnet and absent on haiku; `CLAUDE_EFFORT` is exported to every hook, hence the env fallback of [Effort](#effort).
- `prompt_id` is absent on `SessionStart` startup and resume and on `Setup`, present on nearly every other event.
- `SessionStart` carries `model` only for source `compact`; a resume adds `seconds_since_last_response`, `context_tokens` and cache fields.
- `SessionEnd` `reason` is `other` on every `-p` exit, and one fires at the end of every process, so a resume cycle is `SessionStart` (`resume`) … `SessionEnd`.
- `PostToolBatch.tool_calls[].tool_response` is a string; `PostToolUse.tool_response` is an object whose shape varies by tool.
- A persisted output's `PostToolUse` `stdout` is cut at 30,000 chars beside `persistedOutputPath` and `persistedOutputSize`; the `<persisted-output>` wrapper appears only in `PostToolBatch`.
- Every `/compact` fires a `SubagentStop` with empty `agent_type`, no `SubagentStart`, and an agent transcript path missing on disk.
- Harness `<task-notification>` prompts fire `UserPromptSubmit`; `/compact` itself does not.
- The Bash cwd persists across calls, even inside one parallel batch: a `cd sub && …` in one call made two sibling calls of the same batch fail on relative paths.
- A background sub-agent woken by `SendMessage` starts and stops twice; a nested agent's `SubagentStop` arrives after its parent's, and the parent's `Agent` `PostToolUse` after the parent's own stop.
- `StopFailure` replaces `Stop` on an API error (`model_not_found`, `max_output_tokens`) and is the one non-tool event seen carrying `effort`.
- `WorktreeCreate` and `WorktreeRemove` never fired: in a git repository Claude Code created the worktree natively. A `WorktreeCreate` hook can replace creation, so neither is registered.
- `MessageDisplay` fires per streamed chunk under `-p` and is not registered.
- Never triggered under `-p` and not registered or not relied on: `Notification`, `PermissionDenied` (registered, recorded when they fire), `CwdChanged`, `DirectoryAdded`, `ConfigChange`, `FileChanged`, `TeammateIdle`, `PreModelSwitch`, `PostModelSwitch`, `Elicitation`, `ElicitationResult`.

## Open items

- Retention: **Decided.** 30 days. Every report prunes rows older than 30 days, archiving them first; the hook never prunes.
- The store's location: **Decided.** `CALLMETER_HOME`, one store per machine across seats ([Where the store lives](#where-the-store-lives)).
- `node -e` parsing: **Decided.** It stays unparsed in this version: a named gap, counted in every report that counts commands and in `faults`.
- `BASH_MAX_OUTPUT_LENGTH`: **Decided.** Unchanged.
- `Notification` and `PermissionDenied` payloads: open. Both are registered and stored with sanitized `detail`, but neither fired under `-p` in any capture, so no named column relies on their shape.
