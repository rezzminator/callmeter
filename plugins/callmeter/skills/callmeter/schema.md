# callmeter store schema

Every table, one row per what, and the columns whose meaning is not their name. Times are Unix milliseconds UTC; `seat_dir` is the Claude Code config dir the row came from, `config_dir` the home whose `projects/` holds the transcripts; On `calls`, `requests` and `agents`, `source` is `hook` for a row a hook wrote and `transcript` for one rebuilt from a transcript by a report run (`events.source` is something else, below). Contents: [calls](#calls) · [requests](#requests) · [request_iterations](#request_iterations) · [request_cache](#request_cache) · [agents](#agents) · [agent_turns](#agent_turns) · [turns](#turns) · [events](#events) · [sessions](#sessions) · [command_parts](#command_parts) · [faults](#faults) · [compactions](#compactions) · [session_costs](#session_costs) · [stop_hooks and stop_hook_runs](#stop_hooks-and-stop_hook_runs) · [turn_durations](#turn_durations) · [Joins](#joins)

A store gains newer tables and columns on its next open; `PRAGMA user_version` stays 1, so test for one with `SELECT 1 FROM pragma_table_info('requests') WHERE name = 'thinking_tokens'` or `sqlite_master`, never by version.

## calls

One row per tool call. Key `tool_use_id`.

- `session_id`, `agent_id` (NULL: the main chat), `agent_type`, `request_id` (the model request that issued it), `prompt_id` (the user prompt it ran under).
- `ts`: the call's start. `duration_ms`: NULL while running or when no result hook landed.
- `tool`: the name as called (`Bash`, `Read`, `Agent`, `mcp__server__tool`, …).
- `input`: sanitized JSON, never content. Every text field it drops becomes `{name}_bytes`, its byte count. Bash and Monitor keep `command`, with each heredoc body cut (`heredoc_bytes`) and a commit message, the operands an `echo`/`printf` writes to a file (or into `tee` or a pipe ending in one) and every `<<<` here-string replaced by `'[cut]'` (`operand_bytes`); a command that cannot be cut safely keeps only `command_bytes`. Edit and Write keep `file_path`, with `content_bytes`, `old_string_bytes`, `new_string_bytes`; MultiEdit `edits_bytes`. Agent keeps `subagent_type`, `model` and its booleans, with `prompt_bytes`, `description_bytes`, `name_bytes`. File tools keep `file_path`, `path`, `pattern`, `glob`; numbers and booleans stay. An `mcp__` tool keeps only numbers, booleans and `*_bytes` of its strings.
- `cwd`: for Bash, the directory the command started in (where it ended when its start hook was lost); for other tools, the directory the result hook reported; NULL for refused calls.
- `failed`: 1 failed, 0 succeeded, NULL outcome not recorded. `is_interrupt`: the user interrupted it. `error`: never the error text, only `Exit code N`, an outcome label (`denied by a PreToolUse hook`, `denied by permission`, `rejected or interrupted by the user`, `refused by Claude Code`) or `error text not stored`.
- `bytes_real`: the tool's real output size; `bytes_delivered`: what reached the model's context (smaller when Claude Code spilled the output to a file, whose path is `persisted_path`). `bytes_real` is NULL for a successful Edit, Write or Agent call; a failed call of any tool stores its error text's length.
- `file_path`: the file a file tool named. `file_bytes`, `file_bytes_before`: its size after and before an Edit or Write; `file_bytes_before` is 0 on a Write that created the file and NULL when the call recorded no result. MultiEdit and NotebookEdit carry `file_bytes` only, and NotebookEdit no line counts. `read_start`, `read_lines`, `read_total_lines`: the range a Read returned and the file's length.
- `lines_added`, `lines_removed`: an Edit's or Write's diff size. `commit_sha`, `commit_branch`: a `git commit` the call made. `test_runner`: the runner a test command used.
- `effort`, `permission_mode`: as the hook payload carried them.

## requests

One row per model request (one assistant message). Key `request_id`: the API message id, or `pending:{first tool_use_id}` with `pending = 1` until the transcript shows it.

- `session_id`, `agent_id` (NULL: the main chat), `prompt_id`, `ts`, `model`, `stop_reason`.
- `input_tokens` (uncached input), `cache_read_tokens`, `cache_creation_tokens` (split by cache lifetime into `cache_creation_5m_tokens` and `cache_creation_1h_tokens`), `context_tokens` (the sum of the first three: the request's whole prompt), `output_tokens`, `thinking_tokens` (part of output; NULL when not reported).
- `calls`: how many tool calls the request issued. A request with none (a final text answer) has no `calls` row pointing at it, so a filter that goes through `calls` drops it.
- A request whose usage lists `iterations` with a `type` other than `message` has a `request_iterations` row for each such entry. For a `fallback_message` (a fallback model answered) the request keeps the answering attempt's `input_tokens`, `cache_read_tokens`, `cache_creation_tokens` and `output_tokens`, never their sum over the attempts, and its 5 m / 1 h split can be the first attempt's, so the two split columns may not add up to `cache_creation_tokens`; for any other type the relation is unverified.

## request_iterations

One row per entry of a request's transcript usage `iterations` whose `type` is not `message` (the message itself). Key `request_id`, `seq`: the entry's index in that list (0 is the first attempt). Seen so far: `fallback_message`, the answer of a fallback model after the first model's attempt (`seq` 1). Other types are stored the same way; how their tokens relate to the request's columns is unverified.

- `ts`: the request's. `type`, `model` (NULL when the entry names none).
- `input_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `cache_creation_5m_tokens`, `cache_creation_1h_tokens`, `output_tokens`: the entry's own counts, copied, never derived; NULL when the entry lacks one.
- A request with no such attempt has no row here: no row is not "no fallback" for a request recorded before this table existed.

## request_cache

A view, computed at read time: one row per request that is not pending and has a `ts`, judged against the previous request of its party, the same `session_id` and `agent_id` (the main chat, or one sub-agent).

- `request_id`. `gap_ms`: this request's `ts` minus the previous one's; NULL for a party's first request.
- `entry_ttl`: the lifetime of the cache this request could read, from the latest earlier request of the party that wrote cache (`cache_creation_tokens > 0`): `1h` or `5m` by its split, `mixed` when it wrote both; NULL when none wrote, or the writer carried no split.
- `expected_read`: the previous request's `context_tokens`, what a full cache hit would read.
- `outcome`: `first` (no previous request), `hit` (`cache_read_tokens` at least 95 % of `expected_read`), `partial` (at least 50 %), `miss`; NULL when either count is unknown, never a miss.
- `cause`, for `partial` and `miss` only, the first the stored columns prove: `model_changed` (another model than the previous request's), `compacted` (a `compactions` row of the party between the two requests), `expired` (`gap_ms` past `entry_ttl`: 1 h, or 5 m for `5m` and `mixed`), else `unknown`. No stored column proves a changed thinking block or prompt prefix: those land in `unknown`.
- Join it to `requests` on `request_id` for the party, model and time. It reads only this file; an open recreates it when its SQL differs from the shipped one, unless the stored view's `-- version N` marker is newer; a callmeter older than the stored view does not read it.

## agents

One row per sub-agent. Key `agent_id`, the id the Agent tool returns.

- `session_id`: the main chat it belongs to. `agent_type`, `model`, `prompt_id`.
- `parent_tool_use_id`: the `calls.tool_use_id` of the Agent call that started it; NULL when that link was never recorded.
- `started`, `stopped` (NULL: running, or its stop never recorded). `total_tokens`, `tool_uses`: whole-transcript totals written when a stop is recorded or rebuilt by a report run; NULL while running or after a lost stop.
- `transcript_path`: may be NULL; sub-agent transcripts sit at `{config dir}/projects/{project}/{session_id}/subagents/agent-{agent_id}.jsonl`.

## agent_turns

One row per turn of a sub-agent (an agent woken again by a message gets another). Key `agent_id`, `seq` (from 1). `started`, `stopped` (NULL: open; `started` NULL: a stop with no recorded start), `start_event_id`, `stop_event_id` into `events`.

## turns

One row per `Stop` or `SubagentStop` firing: turn ends only, never prompts. Key `event_id`. `event`, `agent_id`, `prompt_id`, `ts`, `effort`, `permission_mode`, `last_assistant_message_bytes`, `stop_hook_active`, `background_tasks` and `session_crons` (sanitized JSON).

## events

One row per lifecycle hook firing other than the four tool events: `Stop`, `UserPromptSubmit` (prompts live here, `prompt_bytes` their size), `SessionStart`, `SessionEnd`, `SubagentStart`, `SubagentStop`, `Notification`, `PreCompact`, `PostCompact`, `InstructionsLoaded`, `PermissionRequest`, `UserPromptExpansion` (a typed slash command: `command_name`), `StopFailure` (`error_type`), and others as Claude Code adds them. Key `event_id`.

- `source`: SessionStart's `startup`, `resume`, `clear`, `compact`, `fork`. `reason`: SessionEnd's reason. `trigger`: a compaction's `auto` or `manual`. `load_reason`, `memory_type`, `file_path`: an InstructionsLoaded's.
- `detail`: sanitized JSON of the rest, numbers and labels only; the `tool_input` of an `mcp__` tool's permission event keeps only numbers, booleans and `*_bytes`. A resume's `SessionStart` carries `seconds_since_last_response`, `context_tokens`, `prompt_cache_likely_expired`, `estimated_cache_write_usd`; a Notification carries `notification_type` (`idle_prompt`, `permission_prompt`, …).
- `PreCompact` and `PostCompact` carry no `agent_id`, so they cannot tell a sub-agent's compaction from the main chat's: `compactions.agent_id` does.

## sessions

One row per session. Key `session_id`.

- `first_ts`, `last_ts`: over every run of the session. Recording starts when the hooks were installed, so a session older than that begins mid-way.
- `model`: of its latest main-chat request. `start_source`: its first SessionStart's source. `end_reason`: its latest SessionEnd's reason, `never` (the latest run ended with no SessionEnd), `lost` (the SessionEnd hook ran and its write was lost), NULL (unknown or still running).
- `cwd`, `transcript_path`, `host`, `tz_name`, `tz_offset_minutes` (the user's time zone), `engine`. Each `{column}_ts` is the ts of the run that supplied that column.
- No chat name is stored: the name lives in the transcript (`customTitle`, `aiTitle`), and the report topics show it.

## command_parts

One row per simple command inside a Bash call, filled by report runs, never by the hook. Key `tool_use_id`, `seq`.

- `lang` (`sh`, `python`, …), `program`, `args` (JSON list). `files`: JSON list of `{action}\t{range}\t{exists 0|1}\t{path}` strings, action one of `read-whole`, `read-range`, `search`, `write`, `exec`, `stat`, `unknown`.
- `conditional`: 1 when the part sits in an `if` branch, `case` arm or after `&&` / `||`. `parse_status`: `ok`, `error`, `unparsed`, `script-body` (a heredoc script, whose body is not stored), `python-unavailable`, `python-error`.
- A Bash call with no rows here was not yet parsed, has no `cwd`, or stored only `command_bytes`.

## faults

One row per failure to record or parse. Key `fault_id`. `ts` (milliseconds; whole seconds for rows ingested from `missed.log`), `session_id`, `tool_use_id`, `stage` (`payload`, `store`, `transcript`, `parse`, `binary`, `terminated`), `error` (the failure's own message, which may name a file path; a parse fault keeps only a label and byte count). `terminated` and `binary` rows are events lost before they reached the store: count them before claiming something did not happen.

## compactions

One row per context compaction. Key `entry_id`. `session_id`, `agent_id`, `ts`, `trigger`, `pre_tokens`, `post_tokens`, `cumulative_dropped_tokens` (Claude Code's running count), `duration_ms`.

## session_costs

One row per session: Claude Code's latest cumulative cost snapshot. Key `session_id`. `ts`, `started`, `cost_usd`, `api_ms`, `api_no_retry_ms`, `tool_ms`, `wall_ms` (all running totals), `model_costs` (JSON `{model: usd}`; model names may carry a context suffix such as `[1m]` that `requests.model` lacks).

## stop_hooks and stop_hook_runs

`stop_hooks`: one row per Stop-hook summary of a turn, key `entry_id`: `session_id`, `agent_id`, `prompt_id`, `ts`, `hook_count`, `hook_errors`. `stop_hook_runs`: one row per hook run in a summary, key `entry_id`, `seq`, joined to `stop_hooks` by `entry_id` (it has no `session_id`): `name` (derived from the command's program or script, `prompt#…` for a prompt hook, never the command text), `command_bytes`, `duration_ms` (NULL for an async hook: counted, never timed).

## turn_durations

One row per turn wall time Claude Code measured. Key `entry_id`. `session_id`, `agent_id`, `prompt_id`, `ts`, `duration_ms`, `message_count`, `background_agents`. Join to prompts by `prompt_id`.

## Joins

| From | To | On |
| --- | --- | --- |
| `calls` | `requests` | `calls.request_id = requests.request_id` |
| `request_iterations`, `request_cache` | `requests` | `request_id` |
| `agents` | the Agent call that started it | `agents.parent_tool_use_id = calls.tool_use_id` (that call's `agent_id` is the parent agent, NULL for the main chat) |
| `requests`, `calls`, `events`, `turns`, `compactions`, `turn_durations` | `agents` | `agent_id` |
| any table with `session_id` | `sessions`, `session_costs` | `session_id` |
| `calls`, `requests`, `turns`, `events`, `turn_durations` | one user prompt | `prompt_id` |
| `command_parts` | `calls` | `tool_use_id` |
| `stop_hook_runs` | `stop_hooks` | `entry_id` |
| `agent_turns` | `events` | `start_event_id`, `stop_event_id` = `events.event_id` |
