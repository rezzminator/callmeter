<div align="center">

# callmeter

**Every tool call, model request, sub-agent turn and lifecycle event of every Claude Code chat, recorded into one local SQLite store, with reports that show where the context went.**

[![Claude Code plugin](https://img.shields.io/badge/Claude%20Code-plugin-D97757)](https://docs.claude.com/en/docs/claude-code/plugins)
[![Version](https://img.shields.io/badge/version-0.1.0-blue)](./CHANGELOG.md)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](./LICENSE)
[![Built with Professor](https://img.shields.io/badge/built%20with-Professor-8A2BE2)](https://github.com/rezzminator/professor)

</div>

---

A long-running agent rarely runs out of ideas. It runs out of context: one `go test ./...` that printed 28,000 characters, one file read whole five times, one sub-agent that re-read what its parent had already read. Claude Code keeps the transcripts, but nothing adds them up.

**callmeter** adds them up. It hooks every tool call, model request, sub-agent turn and session event, stores what each one touched and cost, and answers from that store: which files are read most and how large they were, which commands push the most text into a context, how context grows request by request, which call sequences repeat often enough to deserve one script, and what each session, prompt, agent and effort level spent.

It records counts and sizes, never your prompts, messages or file contents. It never changes what a model sees: no hook output, no added context, no blocked call.

## 🚀 Quick start

```sh
claude plugin marketplace add rezzminator/callmeter
claude plugin install callmeter@callmeter
```

Start a new session, work as usual, then ask:

```text
/callmeter:report files
```

## 🧠 How it works

```text
Claude Code hook ─▶ libexec/callmeter (sh wrapper) ─▶ callmeter binary ─▶ callmeter.db
```

- Every registered event runs the plugin's wrapper with `hook`. The wrapper finds the `callmeter` binary for this plugin version and `exec`s it with the hook's payload on stdin.
- On the first `SessionStart` after install, the wrapper downloads the binary for your platform from the GitHub release of this version, checks it against the `SHA256SUMS` committed in the plugin, and caches it under `CALLMETER_HOME`. Every later run uses the cache.
- The binary writes one short SQLite transaction per event and exits 0 on every path. A hook it could not serve is logged to `missed.log` and counted as a fault on the next run, so a gap shows in the reports instead of a smaller number.
- Reports read the same store. Each report run first prunes rows older than 30 days, archiving them to `archive.db`, then settles every session with no hook and no transcript write for an hour from its transcripts: calls left in flight, requests no hook wrote, a turn end whose `Stop` or `StopFailure` hook was lost.

Every hook but `SessionStart`, `SessionEnd` and `StopFailure` is async: nothing waits for callmeter before a tool runs.

## 📒 What it records

- `calls`: one row per tool call: tool, sanitized input, chat or sub-agent, prompt, effort, permission mode, duration, failure, bytes produced and bytes delivered to the model, the file touched and its size, lines added and removed, commits, test runners.
- `requests`: one row per model request: model, stop reason, the token split (input, cache read, cache write at 5 m and 1 h, output) and the context size.
- `agents`: one row per sub-agent: type, parent call, first start, last stop, total tokens, tool uses, model.
- `agent_turns`: one row per sub-agent turn, so an agent woken again by `SendMessage` shows every turn.
- `turns`: one row per `Stop` and `SubagentStop`: effort, permission mode, background tasks, the size of the last message. A `Stop` rebuilt from the transcript after its hook was lost has these unknown.
- `events`: one row per lifecycle event: session start and end, prompts (their size only), slash-command expansions, instructions loaded, compactions, stop failures, permission requests, notifications, tasks.
- `sessions`: one row per session: first and last activity, model, how it started and ended, cwd, seat, host, time zone.
- `command_parts`: every simple command inside a Bash call, with the files it read or wrote, parsed at report time.
- `faults`: every event that could not be recorded and every command part that could not be parsed, by stage; a parse fault keeps the byte count of the parser's message, never its text.

## 📊 Reports

```text
/callmeter:report {topic} [--since D] [--project P] [--agent-type T] [--session S] [--limit N] [--json]
```

The same runs from a shell as `callmeter report …` when the binary is on your `PATH`.

| Topic | Answers |
| --- | --- |
| `files` | files by bytes delivered, read count, distinct agents, size on disk, whole against ranged reads, re-reads, runs of the file as a script |
| `writes` | files by write count and total growth |
| `commands` | command shapes by bytes delivered (sum, p50, p95), outputs just under the 30,000-char limit, persisted outputs |
| `context` | per agent: start and peak context, mean growth, the requests that grew it most and the calls behind them |
| `sequences` | call runs that recur across agents, ranked by occurrences × bytes |
| `faults` | calls not recorded, snippets not parsed, hooks the wrapper missed, turns refused by an API error |
| `sessions` | one row per session: model, start and end, calls, agents, cwd, host |
| `prompts` | one row per prompt: calls, failures, agents, requests, tokens |
| `effort` | calls and turns per effort level, permission mode and agent type |
| `tokens` | the token split and cache hit rate per model and agent type |
| `agents` | every sub-agent turn: start, stop, duration, prompt |
| `outcomes` | lines changed per file, commits, test runs |
| `coverage` | transcripts on disk that callmeter never recorded |
| `events` | lifecycle events by kind and value |

| Flag | Meaning |
| --- | --- |
| `--since D` | a duration (`7d`, `24h`) or a date (`2026-09-01`); default the whole 30-day window |
| `--project P` | calls whose `cwd` is `P` or under it |
| `--agent-type T` | calls made by that agent type |
| `--session S` | one session |
| `--limit N` | rows per table, default 25 |
| `--json` | one JSON object (`topic`, `store`, `title`, `columns`, `rows`, `notes`) instead of the text table |

## ⚙️ Configuration

| Variable | Effect |
| --- | --- |
| `CALLMETER_HOME` | where the store, logs and binary cache live; default `${XDG_STATE_HOME:-$HOME/.local/state}/callmeter` (`XDG_STATE_HOME` counts only when absolute) |
| `CALLMETER_BIN` | an executable the wrapper runs instead of resolving one (development) |
| `CALLMETER_RELEASE_BASE` | where the wrapper downloads binaries from; default `https://github.com/rezzminator/callmeter/releases/download` |
| `CALLMETER_E2E` | `1` enables the end-to-end tests (development) |

Under `CALLMETER_HOME`: `callmeter.db` (the store), `callmeter.log` (JSON lines), `missed.log`, `archive.db` (pruned rows) and `bin/{version}/callmeter` (the cached binary).

## 🔒 Privacy

The store stays on your machine; callmeter sends nothing anywhere. Its only network use is the one-time binary download.

No prompt, message or file content is ever stored: each becomes a `{name}_bytes` count. A tool input keeps its command with every heredoc body cut out, an unquoted body keeping only the `$(…)` and backquote substitutions the shell runs, then each `git commit` message and every operand an `echo` or `printf` writes to a file or into `tee` replaced by the one word `'[cut]'`, the bytes cut stored as `operand_bytes` (a command that cannot be cut safely keeps only its byte count), file paths, search patterns and read range; `content`, `old_string`, `new_string`, an edit's `edits`, an agent's `prompt` and a `description`, `query` or `target` are replaced by their byte length. Event details keep only their shape, numbers, booleans, a short list of enum-like keys (`type`, `status`, `source`, `model`, `file_path`, …) and the known labels of a stop failure's `error` and a session end's `reason`; every other string becomes its byte count. A failed call keeps only its exit code or an outcome label, and a parse fault only the size of the parser's message. Commit messages given as `git commit` operands, the text an `echo` or `printf` writes to a file, patch lines, command output, heredoc bodies and error text are never stored. The rest of a command is stored as written, so text it carries in another form stays: a message assigned to a variable before the commit, a commit inside a `bash -c` or `ssh` script string, a word an `echo` prints to the terminal. `callmeter redact` rewrites rows already stored under these rules.

## ❓ FAQ

<details>
<summary>Why not store it in the plugin's data directory?</summary>

Claude Code gives each plugin a data directory per seat: `{CLAUDE_CONFIG_DIR}/plugins/data/callmeter-{marketplace}`. If you run several config dirs (several accounts, or a fleet controller such as Professor), each would get its own store, and calls that belong together would be split. The `callmeter` CLI and other tools also need to find the store without any plugin environment. So the store lives at `CALLMETER_HOME`, one per machine, and each row records the seat it came from.

</details>

<details>
<summary>Does it run on Windows?</summary>

No. The wrapper is POSIX sh, and binaries are released for macOS and Linux on amd64 and arm64 only.

</details>

<details>
<summary>What happens when a hook cannot run?</summary>

The hook still exits 0 and the call goes on untouched. If the wrapper could not find or download the binary, it appends one line to `{CALLMETER_HOME}/missed.log`; the next run turns it into a `binary` fault, and `/callmeter:report faults` shows it. If the hook binary is stopped by SIGTERM, SIGINT or SIGHUP before it recorded the event, it appends its own line there, which becomes a `terminated` fault beside the `binary` ones. A failure inside the binary is a fault of its own stage. Gaps are counted, never hidden.

</details>

<details>
<summary>I use Professor, which has its own recorder. Will calls be counted twice?</summary>

Professor's built-in recorder writes its own store, so the two never mix rows, but both hooks run on every call. Run one recorder per chat: either the plugin or Professor's built-in hook, not both.

</details>

## 🛠️ Development

```text
cmd/callmeter/            the binary
internal/callmeter/       the store, with cmdparse/, report/, command/
internal/hookentry/       the hook entry and its captured payload fixtures
internal/wrappertest/     tests of the sh wrapper
internal/…                clock, sqlitedb, runner, testjail, paths, applog
e2e/                      real Claude Code runs (CALLMETER_E2E=1)
plugins/callmeter/        only what installs: manifest, hooks, wrapper, skill
scripts/                  build-release.sh, release-check.sh, leak-check.sh, sanitize-capture.py, reconcile/
docs/                     design.md, testing.md
```

Go 1.27.1, CGO off. The design is [docs/design.md](./docs/design.md); how to test is [docs/testing.md](./docs/testing.md).

```sh
go test ./internal/<pkg>/ -run <Test> -count=1   # affected
go test ./...                                     # full
go vet ./... && gofmt -l .                        # lint; gofmt prints nothing
scripts/leak-check.sh
claude plugin validate --strict .
claude plugin validate --strict plugins/callmeter
scripts/release-check.sh
CALLMETER_E2E=1 go test ./e2e/... -count=1        # real Claude Code, costs tokens
```

Work lands on `develop`, the default branch; `main` is release-only. A release bumps the version places (`plugins/callmeter/.claude-plugin/plugin.json`, the marketplace entry, the README badge, the CHANGELOG heading), passes `scripts/release-check.sh --release`, and is tagged `callmeter--v{version}` on `main`; `release.yml` builds the four binaries and publishes them.

## 🎓 Built with Professor

callmeter is built and maintained with [Professor](https://github.com/rezzminator/professor), a fleet controller and discipline layer for Claude Code, Codex and OpenCode: chats that message each other, agents held to the project's rules, and gated releases. Professor runs fleets of chats and sub-agents, and callmeter is how it measures what each one actually did.

## License

MIT

<sub>Keywords: Claude Code plugin · Claude Code hooks · tool call telemetry · token usage · context window · prompt cache · cache hit rate · sub-agent tracking · SubagentStop · PostToolBatch · SQLite · observability · cost · Professor</sub>
