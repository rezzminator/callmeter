# callmeter — a Claude Code plugin recording every tool call, request, agent and turn into SQLite

Every hook of every live session on the host runs the binary cache: a bad binary deployed there loses events silently across every chat, and the store is the user's only record of them.

# Vocabulary

- plugin: what installs into Claude Code · `plugins/callmeter/`
- marketplace: the plugin listing · `.claude-plugin/marketplace.json`
- manifest: the plugin's version and author · `plugins/callmeter/.claude-plugin/plugin.json`
- hooks: every registered event runs the wrapper with `hook`, async · `plugins/callmeter/hooks/hooks.json`
- wrapper: the POSIX sh entry every hook and the report skill run; resolves the binary as `$CALLMETER_BIN`, then the binary cache, then a checksum-verified download pinned in `plugins/callmeter/libexec/SHA256SUMS` · `plugins/callmeter/libexec/callmeter` · design `docs/design.md` § The wrapper
- binary: the Go program behind `callmeter {hook|report|version}` · `cmd/callmeter/main.go`
- `CALLMETER_HOME`: the store's directory, `${XDG_STATE_HOME:-$HOME/.local/state}/callmeter` when unset · `internal/paths/paths.go`
- binary cache: the installed binary per plugin version · `{CALLMETER_HOME}/bin/{version}/callmeter`
- `missed.log`: one line per hook the wrapper could not hand to the binary, counted on the binary's next run · `{CALLMETER_HOME}/missed.log`
- store: SQLite rows per tool call, request, agent, turn, event and command part · `{CALLMETER_HOME}/callmeter.db`, schema `internal/callmeter/store.go` · design `docs/design.md` § The store
- seat: one Claude Code config dir; every seat writes the one store, its rows told apart by `seat_dir` · design `docs/design.md` § Where the store lives
- `hookentry`: the `callmeter hook` handler, with the captured payload fixtures · `internal/hookentry/`
- cmdparse: the Bash command parser filling `command_parts` · `internal/callmeter/cmdparse/` · design `docs/design.md` § Parsing a command
- reports: `callmeter report {name}`, read by the report skill · `internal/callmeter/report/`, `plugins/callmeter/skills/report/SKILL.md` · design `docs/design.md` § Reports
- `testjail`: every test package's `TestMain` points `HOME`, `TMPDIR` and `CALLMETER_HOME` into temp dirs · `internal/testjail/testjail.go`
- e2e: real `claude -p` runs reading the store they wrote · `e2e/`
- sanitizer: the only writer of captured fixtures · `scripts/sanitize-capture.py`
- leak gate: the identifying-content scan over a private, untracked terms file · `scripts/leak-check.sh`
- version places: the manifest, the marketplace entry, the README badge and the `CHANGELOG.md` heading, kept in agreement · `scripts/release-check.sh`
- release: tag `v{version}` on `main` builds and publishes the four binaries · `.github/workflows/release.yml`, `scripts/build-release.sh`
- testing manual: what a change owes in tests, and every run command · `docs/testing.md`

# Runtime

## Local

- Affected: `go test ./internal/{pkg}/ -run {Test} -count=1`
- Full: `go test ./...`
- Lint: `go vet ./...`, and `gofmt -l .` printing nothing.
- Leak gate: `scripts/leak-check.sh`; in a worktree, `LEAK_TERMS={checkout}/scripts/leak-terms.txt scripts/leak-check.sh`.
- Validate: `claude plugin validate --strict .` (the marketplace) and `claude plugin validate --strict plugins/callmeter` (the plugin)
- Version places: `scripts/release-check.sh`
- e2e: `CALLMETER_E2E=1 go test ./e2e/... -count=1`; real Claude Code, costs tokens.
- Scratch: `/tmp/callmeter/{purpose}/`

## CI

- `.github/workflows/ci.yml` on every push to `{develop|main}` and every pull request: gofmt, vet, tests, both validates, the leak gate.

## Dev install

- Marketplace copy: `~/.local/share/callmeter-dev`, the `callmeter-dev` directory marketplace.
- Plugin cache: `~/.claude/plugins/cache/callmeter-dev/callmeter/{version}`
- Binary cache: `~/.local/state/callmeter/bin/{version}/callmeter`
- A binary fix deploys to every live hook by atomically replacing the cached binary: build beside it, then `mv -f` onto it.
- A `hooks.json` change also needs the marketplace copy and the plugin cache refreshed, then `/reload-plugins`.

# Rules

## Sacred ground

- **Privacy:** NEVER store prompt, message or file content; the store keeps only its `{name}_bytes` count.
- **Publication:** NEVER push, tag or release without the user's explicit ask in the current turn.
- **Nothing identifying** ships in a tracked file: no machine-absolute path (`/Users/…`, `/home/…`), no private term; the leak gate is the backstop.
- **The live store:** a probe or test NEVER opens `~/.local/state/callmeter/callmeter.db`; it reads a snapshot copy of the db and its `-wal`. `callmeter report` is the store's own writer: it fills the command-parse cache.

## Git

- Git writes go through the `gitter` agent.
- Work lands on `develop`; `main` moves only by release.

## Plugin

- `plugins/callmeter/` holds only what installs: manifest, hooks, wrapper, skill; every other file lives outside it.
- Text the model reads (the report skill, report output, hook output) stays unbranded; Professor's credit lives only in the READMEs and the `author` and `keywords` of the manifest and the marketplace.
