# callmeter — a Claude Code plugin recording every tool call, request, agent and turn into SQLite

Every hook of every live session on the host runs the binary cache: a bad binary deployed there loses events silently across every chat, and the store is the user's only record of them.

# Vocabulary

- plugin: what installs into Claude Code · `plugins/callmeter/`
- marketplace: the plugin listing · `.claude-plugin/marketplace.json`
- manifest: the plugin's version and author · `plugins/callmeter/.claude-plugin/plugin.json`
- hooks: every registered event runs the wrapper with `hook`, async · `plugins/callmeter/hooks/hooks.json`
- wrapper: the POSIX sh entry every hook and both skills run; resolves the binary as `$CALLMETER_BIN`, then the binary cache, then a checksum-verified download pinned in `plugins/callmeter/libexec/SHA256SUMS` · `plugins/callmeter/libexec/callmeter` · design `docs/design.md` § The wrapper
- binary: the Go program behind `callmeter {hook|report|classify|redact|version}` · `cmd/callmeter/main.go`
- `CALLMETER_HOME`: the store's directory, `${XDG_STATE_HOME:-$HOME/.local/state}/callmeter` when unset · `internal/paths/paths.go`
- settings file: the one optional config, `retention_days` alone; absent keeps every row forever, and only `callmeter report` reads it · `{CALLMETER_HOME}/config.json`, `internal/config/config.go` · design `docs/design.md` § Where the store lives
- option variable: `CALLMETER_{OPTION}`, a settings-file option in upper snake (`CALLMETER_RETENTION_DAYS`), read over `config.json` by `config.Resolve`, the single reader, which only `callmeter report` calls · `internal/config/config.go`
- binary cache: the installed binary per plugin version · `{CALLMETER_HOME}/bin/{version}/callmeter`
- `missed.log`: one line per hook the wrapper could not hand to the binary, counted on the binary's next run · `{CALLMETER_HOME}/missed.log`
- store: SQLite rows per tool call, request, agent, turn, event and command part · `{CALLMETER_HOME}/callmeter.db`, schema `internal/callmeter/store.go` · design `docs/design.md` § The store
- seat: one Claude Code config dir; every seat writes the one store, its rows told apart by `seat_dir` · design `docs/design.md` § Where the store lives
- `hookentry`: the `callmeter hook` handler, with the captured payload fixtures · `internal/hookentry/`
- cmdparse: the Bash command parser filling `command_parts` · `internal/callmeter/cmdparse/` · design `docs/design.md` § Parsing a command
- callclass: the call classifier behind `callmeter classify`, labelling a tool call `edit`, `test`, `git`, `housekeeping`, `search`, `read`, `capture`, `wait`, `spawn` or `other` from cmdparse's parts, plus the optional `--rules` file naming a project's wrappers, test programs and tool classes · `internal/callmeter/callclass/` · design `docs/design.md` § Classifying a call
- reports: `callmeter report {name}`, read by the report skill · `internal/callmeter/report/`, `plugins/callmeter/skills/report/SKILL.md` · design `docs/design.md` § Reports
- `testjail`: every test package's `TestMain` points `HOME`, `TMPDIR` and `CALLMETER_HOME` into temp dirs and clears `PFM_COST_LEDGER_DIR` and `PFM_HOME` · `internal/testjail/testjail.go`
- cost ledger: pfm's per-call price files `{session_id}.jsonl` in `$PFM_COST_LEDGER_DIR`, else `{PFM_HOME or HOME}/.local/state/pfm/cost-ledger`, read at report time as optional data · `internal/callmeter/ledger.go`; `requests.api_request_id` links a row to transcripts
- price table: a copy of pfm's, moved together with it · `internal/callmeter/pricing.go`
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
- Lint: `go vet ./...`, and `"$(go env GOROOT)/bin/gofmt" -l .` printing nothing: the toolchain's own gofmt, since a `gofmt` on `PATH` from another Go formats differently.
- Leak gate: `scripts/leak-check.sh`; in a worktree, `LEAK_TERMS={checkout}/scripts/leak-terms.txt scripts/leak-check.sh`.
- Validate: `claude plugin validate --strict .` (the marketplace) and `claude plugin validate --strict plugins/callmeter` (the plugin)
- Version places: `scripts/release-check.sh`
- e2e: `CALLMETER_E2E=1 go test ./e2e/... -count=1`; real Claude Code, costs tokens.
- Scratch: `/tmp/callmeter/{purpose}/`

## CI

- `.github/workflows/ci.yml` on every push to `{develop|main}` and every pull request: gofmt, vet, tests, both validates, the leak gate.

## Dev install

- On a Professor alpha host whose `claude.plugins` names callmeter bare or is unset, `pfm install` owns this install: it builds the binary into the binary cache, keeping the replaced one as `callmeter.prev`, then refreshes the marketplace copy; the steps below are for a host without it.
- Marketplace copy: `~/.local/share/callmeter-dev`, the `callmeter-dev` directory marketplace.
- Plugin cache: `~/.claude/plugins/cache/callmeter-dev/callmeter/{version}`
- Binary cache: `~/.local/state/callmeter/bin/{version}/callmeter`
- Deploy order: the new version's binary into the binary cache first, then the marketplace copy; a plugin version without its cached binary falls through to the download, which an unreleased version's `SHA256SUMS` rejects, and its hooks drop events.
- A binary fix deploys to every live hook by atomically replacing the cached binary: build beside it, then `mv -f` onto it.
- A `hooks.json` change also needs the marketplace copy and the plugin cache refreshed, then `/reload-plugins`.
- Refresh the marketplace with `claude plugin marketplace update callmeter-dev` exactly as written: a `CLAUDE_CONFIG_DIR` override pointing at `~/.claude` makes `claude` write `.claude.json` and `backups/` into the shared store, and `pfm`'s store-guard hook denies it.

# Rules

## Sacred ground

- **Standalone:** NEVER require pfm, Professor or any other tool: the plugin, binary, tests and gates run for anyone with Claude Code and nothing else installed; other tools read the store, callmeter never calls them, and data it takes from one is optional.
- **Privacy:** NEVER store prompt, message or file content; the store keeps only its `{name}_bytes` count.
- **Publication:** NEVER push, tag or release without the user's explicit ask in the current turn.
- **Nothing identifying** ships in a tracked file: no machine-absolute path (`/Users/…`, `/home/…`), no private term; the leak gate is the backstop.
- **The live store:** a probe or test NEVER opens `~/.local/state/callmeter/callmeter.db`; it reads a snapshot copy of the db and its `-wal`. `callmeter report` is the store's own writer: it fills the command-parse cache.

## Git

- Git writes go through the `gitter` agent.
- Work lands on `develop`; `main` moves only by release.

## Plugin

- `plugins/callmeter/` holds only what installs: manifest, hooks, wrapper, skills; every other file lives outside it.
- A store change (a table, a column, a unit, a NULL meaning) updates `plugins/callmeter/skills/callmeter/schema.md` and any recipe in its `SKILL.md` that it touches, each recipe re-run against a snapshot.
- An option variable beats `config.json`, which beats the default; unset or blank is absent; it takes the option's own grammar (`retention_days`: one integer 0 to 36500, 0 keeps every row); a bad one is reported by its name (stderr and a `config-env-invalid` error row) and the default applies, while an untrusted `config.json` still stops the report.
- A new option adds its variable and its entry in the README Configuration tables' variable column.
- Text the model reads (the skills, report output, hook output) stays unbranded; Professor's credit lives only in the READMEs and the `author` and `keywords` of the manifest and the marketplace.
