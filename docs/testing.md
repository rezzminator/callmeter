# callmeter testing manual

Fixed headings, fixed order. The behaviour under test is [design.md](design.md); this file states what a change owes. Keep it true in the same change that alters how callmeter is tested.

## Tiers

- Unit: a package test beside its package, in-process. The store is a real SQLite file, the filesystem real, under the package's jail.
- Process: a test that builds `./cmd/callmeter` into `t.TempDir()`, or runs the sh wrapper, and spawns it as a separate process: `internal/wrappertest` (resolution, download, lock, checksum, `missed.log`), a signalled hook binary (SIGTERM, SIGINT or SIGHUP before and after its event is recorded) and the concurrency gymnastics (many hooks writing one store at once). Still hermetic: no network beyond a local `httptest` server, no Claude Code.
- e2e: `CALLMETER_E2E=1 go test ./e2e/...` runs a real `claude -p --model haiku --setting-sources project,local` with the plugin loaded through `--plugin-dir`, then reads the store it wrote. It costs tokens, needs the host's Claude Code login, and is run once by the lander, never by an executor.

## Where a test lives

- `<file>_test.go` beside its source file, same package. Extend the owning test file; add one only when none exists.
- Hook payload fixtures: `internal/hookentry/testdata/callmeter/`; the transcripts they point at: that directory's `demo-home/`.
- Real-session fixtures, replayed by `internal/hookentry/replay_test.go`: `internal/hookentry/testdata/verify/` (the lifecycle capture) and `internal/hookentry/testdata/gym/{S1,S1b,S2,S3,S4}/` (the gymnastics sessions), each a `payloads.jsonl` beside the `home/.claude/projects/-tmp-demo-proj/` transcripts, sub-agent transcripts and tool-results its payloads name.
- Those fixtures are written only by the sanitizer: `python3 scripts/sanitize-capture.py --map=FROM=TO … --terms scripts/leak-terms.txt IN OUT`. The machine paths travel as `--map=` arguments (the `=` form, since an encoded project directory starts with `-`); a leftover machine path, private term or foreign email refuses the whole run, naming `path:line`, and writes nothing.
- The real-command corpus: `internal/callmeter/cmdparse/testdata/` (`commands-corpus.json`).
- Go fuzz targets: beside their package, seeded from the fixtures above.
- Wrapper tests: `internal/wrappertest/`.
- The e2e suite: `e2e/`.

## Lanes and registries

- none.

## Mock boundary

- Always real: SQLite under `t.TempDir()`, never a mock; the filesystem under the jail; `python3` for the Python parser (its absence fails the test, never skips it).
- The download server is a local `httptest` server; a test never reaches GitHub.
- A real Claude Code only in e2e. Every other test feeds captured payloads.
- Captured payloads only, never hand-written guesses at a payload's shape; every path in them rewritten to `/tmp/demo-proj/…` and `/tmp/demo-home/…`, no username, no email.

## Environments and cleanup

- Every package with tests except `e2e/` has a `TestMain` calling `testjail.Run(m)`: `HOME`, `TMPDIR` and `CALLMETER_HOME` point into temp dirs, `CLAUDE_CONFIG_DIR` and `CLAUDE_CODE_SESSION_ID` are cleared. Resolving `CALLMETER_HOME` unset under `go test` is an error, never the real home.
- `e2e/` keeps the real `HOME` and seat (Claude Code needs its login) and sets `CALLMETER_HOME` to a fresh scratch dir under `/tmp/callmeter/e2e/` per run.
- Tests never touch `~/.local/state/callmeter`.
- The plugin is never enabled on the host: e2e loads it per session with `--plugin-dir`, nothing else installs it.
- One temp root per test; a test leaves nothing outside it.

## Run commands

- Affected, an executor's only run: `go test ./internal/<pkg>/ -run <Test> -count=1`, timeout 600 s.
- Full, the lander's: `go test ./...`.
- Lint: `go vet ./...`, and `gofmt -l .` printing nothing.
- Leak gate: `scripts/leak-check.sh`.
- Plugin manifests: `claude plugin validate --strict .` (the marketplace) and `claude plugin validate --strict plugins/callmeter` (the plugin).
- Release consistency: `scripts/release-check.sh` (the version places agree, the committed `SHA256SUMS` matches a fresh build).
- e2e: `CALLMETER_E2E=1 go test ./e2e/... -count=1`, the lander only.

## Concurrency

- A test calling `t.Setenv` or `t.Chdir` stays serial; any other test may use `t.Parallel`.
- Isolation is one temp root per test, never a shared store.
- The `SubagentStop` settle wait runs on the real clock (up to 3 s); a test fakes only `now`, never the wait.
  - One exception: a fuzz target zeroes the settle wait through the `agentSettle` seam, because a fuzz target tests input handling, not timing, and a real 3 s settle per `SubagentStop` input starves the engine and would mask a real hang. Every other test keeps the rule.

## Gates and floors

- CI (`.github/workflows/ci.yml`): gofmt, vet, tests, both plugin validates and the leak check.
- No coverage floor in 0.1.0.
- Allowed skips, each named: the e2e gate without `CALLMETER_E2E=1`, and corpus cases carrying `known_defect`. Any other skip fails review.

## Bug classes

- Order dependence: async hooks land in any order and sometimes twice; a store behaviour is tested with its payloads in more than one order and with a duplicate delivery.
- Absence read as zero: a gap (an unrecorded call, an unparsed snippet, a missed binary run, an unreadable transcript) must show as a note or a fault, never as a smaller number.
- Privacy: a test that feeds a prompt, message or file content asserts its text appears nowhere in the store, only its `_bytes` count.

## Tricks and traps

- On macOS `/tmp` is a symlink to `/private/tmp`, and Claude Code reports `cwd` as `/private/tmp/…`; a test comparing paths resolves both sides or uses the jail's own paths.
- A probe that launches `claude` closes stdin (`</dev/null`) or it hangs.
- A headless `claude -p` cancels running async hooks at exit, so an e2e run's last calls can keep only their `PreToolUse` row; an e2e assertion on a call's result columns names the call it checks, never "every call".
- `ls` may be aliased on a developer host; scripts call `command ls` or `/bin/ls`.
- `FuzzHookPayload` switches the engine's coverage minimization off (it sets `test.fuzzminimizetime` to 0 before `f.Fuzz`; a `-fuzzminimizetime` named on the command line wins). The engine hands each input that found new coverage to one worker to shrink for up to 60 s, every attempt a full hook run over a payload of up to tens of KB, and those attempts are not counted as execs: a few at once leave every worker minimizing and the run printing `0/sec` samples, which reads as a stall but is not a hang. The cost: such an input, and a crasher, is kept unminimized, and the Go fuzz cache grows with every run (`go clean -fuzzcache` empties it).

## What not to test

- A model's words: e2e asserts from the store and the transcripts, never from what the model said.
- A value one run printed about its data (a count, an id).
- A removed feature takes its tests with it, never inverted into an absence assertion.
