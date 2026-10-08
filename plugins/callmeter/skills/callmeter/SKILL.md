---
name: callmeter
description: "Query the store directly with SQL or Python: where it lives, safe reads, units, joins, tested recipes. Use for what no report topic answers — per-agent tokens and peak context, sub-agent trees, what one agent did, your own joins and windows. Fixed summaries → /callmeter:report."
allowed-tools: Bash, Read
---

# Querying the callmeter store

Answer a question from the SQLite store the callmeter hooks fill, read-only, with SQL or Python. The full column reference is [schema.md](schema.md); read it before querying a table not covered below, instead of discovering columns with `.schema` or `PRAGMA table_info`.

## Open the store

The store is `{CALLMETER_HOME}/callmeter.db`, `CALLMETER_HOME` defaulting to `$XDG_STATE_HOME/callmeter` when that variable is an absolute path, else `~/.local/state/callmeter`: one file per machine, every Claude Code config dir writing into it, rows older than 30 days moved to `archive.db` beside it. Hooks write to it concurrently in WAL mode; every read carries a busy timeout so a moment of contention waits instead of failing. A read that still fails is reported as the error it is, never as an empty store.

Shell state does not survive between Bash calls: define the helper in the same command that uses it. A function, because zsh does not word-split a command stored in a variable:

```sh
s="${XDG_STATE_HOME:-}"; case "$s" in /*) ;; *) s="$HOME/.local/state" ;; esac
CM_DB="${CALLMETER_HOME:-$s/callmeter}/callmeter.db"
cmq() { sqlite3 -readonly -cmd '.timeout 15000' -header -column "$CM_DB" "$@"; }
cmq "SELECT count(*) AS calls, datetime(max(ts)/1000,'unixepoch') AS latest_utc FROM calls"
```

- Output format goes in flags (`-header -column`, `-json`, `-csv`); a dot-command after SQL in the quoted argument fails with `near ".": syntax error`. Several statements or dot-commands go in a heredoc: `sqlite3 -readonly -cmd '.timeout 15000' "$CM_DB" <<'SQL' … SQL`.
- Every query carries `LIMIT` or aggregates: the tables hold tens of thousands of rows and `calls.input` is wide.
- A long or repeated analysis works on one snapshot instead of the live file. Take it with SQLite's backup, into one path you overwrite each time (each copy is the store's full size): `sqlite3 -readonly -cmd '.timeout 15000' "$CM_DB" ".backup '${TMPDIR:-/tmp}/callmeter-snap.db'" || echo "FAILED: snapshot"`. A plain `cp` of a live WAL database can produce a corrupt copy, and `immutable=1` reads the main file without its WAL, missing recent rows.
- Older history: `ATTACH` `archive.db` read-only (`ATTACH 'file:{dir}/archive.db?mode=ro' AS archive` on a URI-enabled connection) and union the same table from both, naming columns explicitly: the archive lacks `calls.input`, `calls.error`, `faults.error` and `events.detail`, and an older archive lacks newer columns. A row can sit in both files, so take the archive's rows only where the key is absent from the store: `… FROM archive.calls WHERE tool_use_id NOT IN (SELECT tool_use_id FROM main.calls)`.

## Never write to the store

The store is the user's only record of their sessions. Open it read-only, every time; scratch tables are `CREATE TEMP TABLE` (allowed on a read-only connection) or live in a snapshot. Run no `callmeter redact` or other maintenance command yourself: a report note naming one is for the user.

The one exception is a report run, the store's own writer, and only when the user asked for a report or agrees to one. A run settles quiet sessions (below), fills `command_parts`, and prunes rows older than 30 days into `archive.db`, dropping their `calls.input`, `calls.error`, `faults.error` and `events.detail` for good. Never point one at a snapshot you want to keep raw.

## Rules that keep an answer true

- Times are integer Unix milliseconds, UTC: `ts`, `started`, `stopped`, `first_ts`, `last_ts`. Show one with `datetime(ts/1000,'unixepoch')` (append `,'localtime'` for the host's clock and say which you show). Filter with milliseconds: `ts >= (strftime('%s','now') - 2*3600) * 1000`. Comparing `ts` to `datetime(...)` text silently matches nothing.
- Durations (`duration_ms`, `wall_ms`, `api_ms`) are milliseconds; divide by `1000.0` or `60000.0`, since integer division truncates.
- The main chat is `agent_id IS NULL`; a sub-agent's rows carry its `agent_id`, and the main chat's `session_id`, so `WHERE session_id = …` covers a session's main chat and every sub-agent at any depth. Your own session id is `$CLAUDE_CODE_SESSION_ID`. A sub-agent you spawned has, as its `agents.agent_id`, the agent id the Agent tool returned.
- An empty result is not proof of absence. Before you report "none", run the same query without its narrowest filter (or `count(*), max(ts)` over the table) and read `faults` for the window: hooks killed by a busy store or a missing binary leave rows there, not in `calls`.
- Content is never stored. `calls.input` is sanitized JSON (per tool in [schema.md § calls](schema.md#calls)): Bash keeps its `command` with heredoc bodies and some operands cut, or only `command_bytes`; Edit and Write keep `file_path` and the byte counts of their text. Size an edit by `lines_added`, `lines_removed`, `file_bytes` and `file_bytes_before` (0 on a Write that created the file). Edits made through Bash (`sed -i`, a Python heredoc) carry no line counts.
- `tool = 'Read'` undercounts reading: agents read mostly through Bash (`cat`, `sed -n`, `rg`). The files a Bash call touched are in `command_parts.files`, which only a report run fills.
- `context_tokens` is a request's whole prompt (`input_tokens + cache_read_tokens + cache_creation_tokens`), so peak context is `max(context_tokens)`. `sum(context_tokens)` re-counts the cached prefix at every request: label it "tokens processed", never "unique tokens". `thinking_tokens` is part of `output_tokens`, never added to it; NULL means not reported, not 0.
- A request with `pending = 1` (its `request_id` starts `pending:`) has no final usage yet; exclude it from sums and say how many you excluded. `agents.total_tokens` and `tool_uses` are written when a stop is recorded or rebuilt, so NULL means a running agent or a lost stop; sum its `requests` instead. `agents.stopped IS NULL` is an agent still running or one whose stop was never recorded.
- Cost: `session_costs` is Claude Code's own cumulative USD per session (`cost_usd`, plus `model_costs` JSON per model, NULL when Claude Code gave no split), the number to quote; only sessions recorded by a recent callmeter on a recent Claude Code have a row, so count the sessions without one. No per-agent cost column exists: when you price `requests` yourself, take the rates from the user or the provider's current price page (cache writes at 5 m and 1 h are priced differently: `cache_creation_5m_tokens`, `cache_creation_1h_tokens`), reconcile your total against `session_costs.cost_usd` for the same session, and state the gap.
- Numbers settle late. A session quiet for an hour is completed from its transcripts (missing sizes, pending requests, lost stops) only when a report runs (§ Never write to the store). Without one, say that a quiet session's numbers may be unsettled.
- `tool` is the name as the model called it: a handful of calls refused by Claude Code carry a misspelt name such as `bash`.

## Recipes

Every recipe runs as written inside `cmq "…"` or the heredoc form. `:sid` stands for a session id and `:aid` for an agent id: substitute the quoted literal (`'$CLAUDE_CODE_SESSION_ID'` inside double quotes expands in the shell).

Spend and peak context per agent in one session, main chat included:

```sql
SELECT coalesce(r.agent_id, '(main)') AS agent, a.agent_type, count(*) AS requests,
       max(r.context_tokens) AS peak_context, sum(r.output_tokens) AS output,
       sum(r.thinking_tokens) AS thinking, sum(r.cache_read_tokens) AS cache_read,
       sum(r.cache_creation_tokens) AS cache_write
FROM requests r LEFT JOIN agents a USING (agent_id)
WHERE r.session_id = :sid AND r.pending = 0
GROUP BY r.agent_id ORDER BY peak_context DESC LIMIT 30;
```

Each Agent call in a session and the agent it started, with wall time:

```sql
SELECT c.tool_use_id, coalesce(c.agent_id, '(main)') AS spawned_by,
       json_extract(c.input, '$.subagent_type') AS type, coalesce(a.model, json_extract(c.input, '$.model')) AS model,
       a.agent_id, datetime(a.started/1000, 'unixepoch') AS started_utc,
       round((a.stopped - a.started)/60000.0, 1) AS wall_min, a.total_tokens, a.tool_uses
FROM calls c LEFT JOIN agents a ON a.parent_tool_use_id = c.tool_use_id
WHERE c.session_id = :sid AND c.tool IN ('Agent', 'Task')
ORDER BY c.ts LIMIT 50;
```

An agent and every descendant, with the family's totals. Both join keys are primary keys, so every agent has one parent call and appears once:

```sql
WITH RECURSIVE tree(agent_id, depth) AS (
  SELECT :aid, 0
  UNION
  SELECT a.agent_id, t.depth + 1
  FROM tree t
  JOIN calls c ON c.agent_id = t.agent_id
  JOIN agents a ON a.parent_tool_use_id = c.tool_use_id
)
SELECT count(DISTINCT t.agent_id) AS agents, max(t.depth) AS depth, count(r.request_id) AS requests,
       sum(r.output_tokens) AS output, max(r.context_tokens) AS peak_context
FROM tree t LEFT JOIN requests r ON r.agent_id = t.agent_id AND r.pending = 0;
```

The walk misses an agent whose `parent_tool_use_id` is NULL (launched before recording began, or its parent's result lost) or points at no recorded call. A whole session needs no walk: filter by `session_id`, which every row of the main chat and of every sub-agent carries.

What one agent did — tool mix, then its slowest calls:

```sql
SELECT tool, count(*) AS calls, sum(failed) AS failed, round(sum(duration_ms)/60000.0, 1) AS tool_min,
       sum(bytes_delivered) AS bytes_into_context
FROM calls WHERE agent_id = :aid GROUP BY tool ORDER BY calls DESC;

SELECT round(duration_ms/1000.0, 1) AS secs, tool,
       substr(replace(coalesce(json_extract(input, '$.command'), file_path, ''), char(10), ' '), 1, 120) AS what
FROM calls WHERE agent_id = :aid ORDER BY duration_ms DESC LIMIT 10;
```

Which tools put the most bytes into the context in the last day (`callmeter report commands` breaks Bash down by command shape):

```sql
SELECT tool, count(*) AS calls, sum(bytes_delivered) AS bytes, max(bytes_delivered) AS largest,
       sum(persisted_path IS NOT NULL) AS spilled_to_file
FROM calls WHERE ts >= (strftime('%s','now') - 86400) * 1000
GROUP BY tool ORDER BY bytes DESC LIMIT 15;
```

Compactions, newest first (`agent_id` NULL is the main chat; the `PreCompact` and `PostCompact` events carry no agent id, so this table is the one that tells them apart):

```sql
SELECT datetime(ts/1000, 'unixepoch') AS utc, session_id, coalesce(agent_id, '(main)') AS agent, trigger,
       pre_tokens, post_tokens, pre_tokens - post_tokens AS freed, round(duration_ms/1000.0, 1) AS secs
FROM compactions ORDER BY ts DESC LIMIT 20;
```

Prompt-cache outcomes per party in the last day, with the cause the stored columns prove (`request_cache`, [schema.md § request_cache](schema.md#request_cache); an empty `outcome` is an unknown count, never a miss; `callmeter report cache` is the fixed summary):

```sql
SELECT CASE WHEN r.agent_id IS NULL THEN '(main)' ELSE 'sub-agent' END AS party, c.entry_ttl, c.outcome, c.cause,
       count(*) AS requests
FROM request_cache c JOIN requests r USING (request_id)
WHERE r.ts >= (strftime('%s','now') - 86400) * 1000
GROUP BY 1, 2, 3, 4 ORDER BY requests DESC LIMIT 20;
```

Cost per session, and per model inside it where Claude Code gave the split:

```sql
SELECT s.session_id, round(s.cost_usd, 2) AS usd, round(s.wall_ms/3600000.0, 2) AS wall_h,
       round(s.api_ms/3600000.0, 2) AS api_h, m.key AS model, round(m.value, 2) AS model_usd
FROM session_costs s LEFT JOIN json_each(s.model_costs) m
ORDER BY s.cost_usd DESC, m.value DESC LIMIT 30;
```

Sub-agents of a session with no recorded stop, with the minutes since each one's last tool call (minutes: likely running; hours or days: its stop was never recorded, the agent killed or its hook lost; empty: no call recorded), then the session's counts (a zero there is a real zero, not a dead query):

```sql
SELECT a.agent_id, a.agent_type, datetime(a.started/1000, 'unixepoch') AS started_utc,
       round((strftime('%s','now') * 1000 - max(c.ts)) / 60000.0) AS min_since_last_call
FROM agents a LEFT JOIN calls c ON c.session_id = a.session_id AND c.agent_id = a.agent_id
WHERE a.session_id = :sid AND a.stopped IS NULL
GROUP BY a.agent_id ORDER BY a.started DESC LIMIT 20;

SELECT count(*) AS agents_in_session, sum(stopped IS NULL) AS without_stop,
       datetime(max(started)/1000, 'unixepoch') AS latest_start_utc
FROM agents WHERE session_id = :sid;
```

The turns of a session with their wall time, one row per turn: every prompt submitted into a running turn (a queued message, a scheduled prompt) shares its `prompt_id`, counted in `submits`:

```sql
SELECT e.prompt_id, datetime(min(e.ts)/1000, 'unixepoch') AS first_utc, count(*) AS submits,
       sum(e.prompt_bytes) AS prompt_bytes,
       (SELECT round(sum(d.duration_ms)/1000.0, 1) FROM turn_durations d
        WHERE d.session_id = e.session_id AND d.prompt_id = e.prompt_id) AS wall_s
FROM events e WHERE e.session_id = :sid AND e.event = 'UserPromptSubmit'
GROUP BY e.prompt_id ORDER BY min(e.ts) DESC LIMIT 20;
```

## Python

For loops, percentiles or joins with files, use the standard library; check that pandas is installed before reaching for it.

```python
import os, sqlite3, statistics
from pathlib import Path

xdg = os.environ.get("XDG_STATE_HOME", "")
home = os.environ.get("CALLMETER_HOME") or str(Path(xdg if xdg.startswith("/") else Path.home() / ".local/state") / "callmeter")
con = sqlite3.connect(Path(home, "callmeter.db").resolve().as_uri() + "?mode=ro", uri=True, timeout=15)
con.row_factory = sqlite3.Row
try:
    peaks = {}
    for row in con.execute(
        "SELECT a.agent_type, max(r.context_tokens) AS peak FROM requests r JOIN agents a USING (agent_id) "
        "WHERE r.pending = 0 AND r.ts >= (strftime('%s','now') - 7*86400) * 1000 GROUP BY r.agent_id"
    ):
        peaks.setdefault(row["agent_type"] or "?", []).append(row["peak"])
finally:
    con.close()
for agent_type, values in sorted(peaks.items(), key=lambda kv: -max(kv[1])):
    p50, p90 = (statistics.quantiles(values, n=10, method="inclusive")[i] for i in (4, 8)) if len(values) > 1 else (values[0], values[0])
    print(f"{agent_type:32} n={len(values):4} p50={p50:9.0f} p90={p90:9.0f} max={max(values):9}")
```

Pass values as parameters (`con.execute(sql, (sid,))`), never by formatting them into the SQL, and close the connection when done: an open read transaction holds back the WAL checkpoint the hooks rely on.
