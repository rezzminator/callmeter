# wire.db — the optional wire-facts contract (version 1)

callmeter reads what Claude Code's transcripts carry. Some facts about a model request exist only on the wire: the TTL the request asked for, where its prompt stopped matching the previous request's, the rate-limit headers of the response. A recording proxy between Claude Code and the API can write them to `wire.db`, and the `cache` report joins them onto the store's requests. callmeter defines this file and never names, calls or requires a producer: with no `wire.db`, every report works and the wire facts are unknown.

## File

- Path: `{CALLMETER_HOME}/wire.db`. `CALLMETER_HOME` resolves by callmeter's rule: `${CALLMETER_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/callmeter}` (`XDG_STATE_HOME` counts only when absolute).
- Written only by a producer the user runs and opts in; nothing writes it by default. One writer: the producer. callmeter opens it read-only (`file:…?mode=ro`) and never writes, creates, prunes or archives it; reading a WAL database leaves SQLite's empty `-wal` and `-shm` beside it when no writer has them open. The producer prunes its own rows (older than 90 days, at start and once a day).
- `PRAGMA user_version = 1`. WAL journal.
- Absent file = "no wire data": every wire column reads NULL and no report calls it "no break". An unreadable file, a wrong `user_version` or a missing table or column is a visible warning in the report output and the callmeter log, never rendered as absence.
- No prompt, message, file or tool content, ever: only the integers and short enums below. Block hashes live in the producer's memory only.
- Nothing in `callmeter.db` refers to it: the report joins it at read time, and a person or model attaches it read-only (`ATTACH 'file:{path}?mode=ro' AS wire`, the recipe in the `callmeter` skill).

## Table

```sql
CREATE TABLE wire_requests (
  request_id   TEXT PRIMARY KEY,  -- API message id (msg_…): SSE message_start.message.id, or the JSON body's id; equals the id callmeter stores for the request
  ts           INTEGER NOT NULL,  -- unix ms the response began
  agent_id     TEXT,              -- value of the x-claude-code-agent-id request header; NULL = main chat
  ttl_sent     TEXT,              -- TTL the REQUEST ITSELF asked for on its cache_control breakpoints, as received from the client: '5m' | '1h' | 'mixed' | NULL (no breakpoint); a breakpoint without a ttl counts as '5m' (the API's default); any other ttl the API accepted is copied as sent when it is 1-8 characters of [0-9a-z], else 'other'
  advisor_ttl  TEXT,              -- TTL on the advisor tool entry as forwarded upstream: '5m' | '1h' | NULL (no advisor tool, or no caching on it)
  advisor_added INTEGER,          -- 1 = the producer added the advisor caching, 0 = the producer added none (the client had set it, or nobody did: then advisor_ttl is NULL), NULL = no advisor tool in the request
  break_at     INTEGER,           -- index, in the party's ordered block list, of the first block of the PREVIOUS request (§ Previous request) that no longer matches this request; NULL = previous request is an intact prefix, there is no previous request, or it is unknown
  break_kind   TEXT,              -- 'first' (no previous request for the party) | 'none' (prefix intact) | 'tools' | 'system' | 'messages' | 'thinking' (the differing block is a thinking block: Claude Code resends earlier thinking emptied) | NULL (the request names a previous message the producer never saw: unknown, never 'none')
  rl_status    TEXT,              -- response header anthropic-ratelimit-unified-status, e.g. 'allowed'; NULL = header absent
  overage      TEXT               -- response header anthropic-ratelimit-unified-overage-status, e.g. 'rejected'; NULL = header absent
) WITHOUT ROWID;
```

A party is the main chat or one sub-agent: key = (session, agent_id). The session is the `x-claude-code-session-id` request header, else `session_id` inside the body's `metadata.user_id`, else the proxy connection's client address.

## Block list

The ordered block list is tools, then system, then each message's content blocks, in request order. A string `system` or a string message `content` is one block. A block is compared without its `cache_control` field (moving a breakpoint is not a break); a message block is compared together with its message's role.

A thread continuation (a request body carrying `thread: {"type": "continue", "previous_message_id": …}`, the `message-threads-2026-08-12` beta) sends only the new messages: the API holds the rest. Its block list is the previous request's block list, then one block standing for the previous request's response, then the continuation's own message blocks; the continuation's top-level `system` and `tools` (a resent fragment) are not part of it. A later full request compared against a continuation therefore differs at that response block at the latest: a `messages` break (`thinking` when the full request's block there is a thinking block).

## Previous request

1. A thread continuation: the party's request whose response id equals `thread.previous_message_id`.
2. Else a request carrying `diagnostics.previous_message_id` (the `cache-diagnosis-2026-04-07` beta): the party's request whose response id equals it.
3. Else the party's latest earlier request that produced a row.

In 1 and 2 an id the producer never saw (restarted, evicted from memory) is unknown: `break_at` and `break_kind` NULL. Only requests that produced a row count; the producer keeps parties in memory only, drops a party idle for an hour and caps their number.

## Evidence

Captured traffic of Claude Code 2.1.294 on 2026-10-08 (143 requests, keys and enums only): 105 requests are thread continuations carrying 2 messages and no tools; side requests (`max_tokens` 3072, one message, no stream) share the main chat's session and agent id; responses carry `anthropic-ratelimit-unified-status` and `anthropic-ratelimit-unified-overage-status`. Hence § Block list's thread rule, § Previous request, `break_kind` NULL and the named headers.
