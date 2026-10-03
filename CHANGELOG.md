# Changelog

Every release of callmeter. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release carrying the section below and the four platform binaries.

## [Unreleased]

### Fixed
- Attribute `awk` program files and input operands as reads, without counting `-v` and `-F` option values (#3).

## [0.1.0] — 2026-10-03

### Added
- callmeter as a standalone Claude Code plugin, ported from Professor's built-in recorder: the hook, the store, the Bash command parser (shell through `mvdan.cc/sh`, Python through its own `ast`) and the `files`, `writes`, `commands`, `context`, `sequences` and `faults` reports, unchanged in what they answer.
- A fresh store at `CALLMETER_HOME` (default `~/.local/state/callmeter/callmeter.db`), one per machine across every Claude Code config dir, schema version 1, with a 30-day prune that archives every pruned row to `archive.db`.
- The plugin and its wrapper: one hook registration per recorded event, a POSIX sh wrapper that downloads the release binary on first use, verifies it against the committed `SHA256SUMS`, caches it per version, and logs any hook it could not serve to `missed.log` for the next run to count.
- New tables: `turns`, `events`, `agent_turns` and `sessions`, and new columns on calls, requests and agents: prompt id, effort, permission mode, interrupts, the request token split by cache TTL, lines added and removed, commits and test runners. Prompts and messages are stored as byte counts only.
- New reports: `sessions`, `prompts`, `effort`, `tokens`, `agents`, `outcomes`, `coverage` and `events`, and `/callmeter:report` inside Claude Code.
- `--json` on every report: one object with the topic, columns, rows and notes.
